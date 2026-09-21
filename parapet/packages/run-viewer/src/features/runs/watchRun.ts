import type { Dispatch } from '@reduxjs/toolkit';
import { getRunDataSource, TERMINAL_EVENT_TYPES, type RunStreamEnd } from '../../api/dataSource';
import { sessionExpired } from '../auth/sessionSlice';
import { runsSlice, type WatchStatus } from './runsSlice';

// Bounded reconnect backoff for the watchRun stream: exponential with a
// ceiling, giving up (and surfacing a manual reconnect affordance) after a
// bounded number of attempts.
export const WATCH_RECONNECT_BASE_MS = 1_000;
export const WATCH_RECONNECT_MAX_MS = 15_000;
export const WATCH_RECONNECT_MAX_ATTEMPTS = 5;

export function watchReconnectDelayMs(attempt: number): number {
  const clamped = Math.max(1, attempt);
  return Math.min(WATCH_RECONNECT_BASE_MS * 2 ** (clamped - 1), WATCH_RECONNECT_MAX_MS);
}

function safeMessage(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  return raw.length > 200 ? `${raw.slice(0, 200)}…` : raw;
}

function setStatus(runId: string, dispatch: Dispatch, status: WatchStatus): void {
  dispatch(runsSlice.actions.watchStatusChanged({ runId, status }));
}

function sleepAbortable(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer);
      signal.removeEventListener('abort', onAbort);
      resolve();
    };
    const onAbort = () => finish();
    const timer = setTimeout(finish, ms);
    if (signal.aborted) {
      finish();
      return;
    }
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

// Streams a run's events through the host's RunDataSource, dispatching them
// into the runs slice. The stream closes cleanly after a terminal event
// (runCompleted / runFailed), so a terminal end is normal. Any other clean
// end means the stream was lost (server restart or eviction); it and stream
// errors are surfaced to the user and retried with bounded backoff, resuming
// from the last delivered seq. An unauthenticated end marks the session
// expired and stops retrying — the user must sign in again first.
export async function startWatch(
  runId: string,
  sinceSeq: number,
  subscriberId: string,
  dispatch: Dispatch,
  signal: AbortSignal,
): Promise<void> {
  let cursor = sinceSeq;
  let attempt = 0;
  setStatus(runId, dispatch, { state: 'connecting', attempt: 0, maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS });

  while (!signal.aborted) {
    let terminalSeen = false;
    let end: RunStreamEnd;
    try {
      end = await getRunDataSource().openRunStream(
        { runId, sinceSeq: cursor, subscriberId, signal },
        (event) => {
          if (signal.aborted) return;
          // WatchReady confirms the watch is established; it has no run state.
          if (event.type === 'watchReady') {
            attempt = 0;
            setStatus(runId, dispatch, { state: 'live', attempt: 0, maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS });
            return;
          }
          dispatch(runsSlice.actions.eventReceived(event));
          cursor = Math.max(cursor, event.seq);
          if (TERMINAL_EVENT_TYPES.has(event.type)) {
            terminalSeen = true;
          }
        },
      );
    } catch (err) {
      if (signal.aborted) return;
      // eslint-disable-next-line no-console
      console.warn('watchRun terminated:', err);
      end = { kind: 'error', message: safeMessage(err) };
    }

    if (signal.aborted) return;

    if (terminalSeen || end.kind === 'terminal') {
      dispatch(runsSlice.actions.watchEnded(runId));
      return;
    }

    if (end.kind === 'unauthenticated') {
      setStatus(runId, dispatch, {
        state: 'unauthenticated',
        attempt: 0,
        maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS,
        message: end.message,
      });
      dispatch(sessionExpired());
      return;
    }

    attempt += 1;
    if (attempt > WATCH_RECONNECT_MAX_ATTEMPTS) {
      setStatus(runId, dispatch, {
        state: 'lost',
        attempt,
        maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS,
        message: `stream lost after ${WATCH_RECONNECT_MAX_ATTEMPTS} attempts`,
      });
      return;
    }
    setStatus(runId, dispatch, {
      state: 'reconnecting',
      attempt,
      maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS,
      message:
        end.kind === 'error'
          ? (end.codeName ?? end.message ?? 'stream failed')
          : 'stream ended unexpectedly',
    });
    await sleepAbortable(watchReconnectDelayMs(attempt), signal);
  }
}