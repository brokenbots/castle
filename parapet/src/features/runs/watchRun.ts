import type { Dispatch } from '@reduxjs/toolkit';
import { ConnectError } from '@connectrpc/connect';
import { server } from '../../api/client';
import { mapEnvelope } from '../../api/castleApi';
import { connectCodeName, isUnauthenticatedError } from '../../api/errors';
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

const TERMINAL_EVENT_TYPES = new Set(['runCompleted', 'runFailed']);

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

// Streams a run's events, dispatching them into the runs slice. The Castle
// server closes the stream cleanly after a terminal event (runCompleted /
// runFailed), so a clean end there is normal. Any other clean end means the
// stream was lost (server restart or eviction); it and stream errors are
// surfaced to the user and retried with bounded backoff, resuming from the
// last delivered seq. An unauthenticated end marks the session expired and
// stops retrying — the user must sign in again first.
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
    let failure: unknown;
    try {
      for await (const env of server.watchRun(
        { runId, sinceSeq: BigInt(cursor), subscriberId },
        { signal },
      )) {
        // WatchReady confirms the watch is established; it has no run state.
        if (env.payload?.case === 'watchReady') {
          attempt = 0;
          setStatus(runId, dispatch, { state: 'live', attempt: 0, maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS });
          continue;
        }
        const mapped = mapEnvelope(env);
        dispatch(runsSlice.actions.eventReceived(mapped));
        cursor = Math.max(cursor, mapped.seq);
        if (TERMINAL_EVENT_TYPES.has(mapped.type)) {
          terminalSeen = true;
          break;
        }
      }
    } catch (err) {
      if (signal.aborted) return;
      // eslint-disable-next-line no-console
      console.warn('watchRun terminated:', err);
      if (isUnauthenticatedError(err)) {
        setStatus(runId, dispatch, {
          state: 'unauthenticated',
          attempt: 0,
          maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS,
          message: safeMessage(err),
        });
        dispatch(sessionExpired());
        return;
      }
      failure = err;
    }

    if (signal.aborted) return;

    if (terminalSeen) {
      dispatch(runsSlice.actions.watchEnded(runId));
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
      message: failure instanceof ConnectError
        ? connectCodeName(failure.code)
        : failure
          ? safeMessage(failure)
          : 'stream ended unexpectedly',
    });
    await sleepAbortable(watchReconnectDelayMs(attempt), signal);
  }
}