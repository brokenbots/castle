import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import {
  TERMINAL_EVENT_TYPES,
  setRunDataSource,
  resetRunDataSource,
  type RunDataSource,
  type RunStreamArgs,
  type RunStreamEnd,
} from '../../api/dataSource';
import type { EventEnvelope } from '../../api/castleApi';
import { runsSlice } from './runsSlice';
import { sessionExpired } from '../auth/sessionSlice';

// The seam is mocked, not the transport: startWatch consumes
// openRunStream(args, onEvent) and classifies the RunStreamEnd, so the fake
// data source replays that contract directly.
const openRunStreamMock = vi.fn();

function makeEvent(seq: number, type = 'stepLog'): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'r1',
    seq,
    type,
    ts: '2026-01-01T00:00:00.000Z',
    correlationId: '',
    payload: { stream: 'STDOUT', chunk: `#${seq}` },
  };
}

// Imports after the data source is swapped in.
const {
  startWatch,
  watchReconnectDelayMs,
  WATCH_RECONNECT_BASE_MS,
  WATCH_RECONNECT_MAX_ATTEMPTS,
} = await import('./watchRun');

beforeEach(() => {
  openRunStreamMock.mockReset();
  // Every test drives the seam through the mock, never the castle impl.
  setRunDataSource({ openRunStream: openRunStreamMock } as unknown as RunDataSource);
});

// Installs a data source whose openRunStream replays a scripted stream per
// call; scripts are consumed in order (the last one repeats).
function scriptStreams(scripts: Array<() => { deliver: (onEvent: (e: EventEnvelope) => void) => void; end: RunStreamEnd }>): void {
  openRunStreamMock.mockImplementation(async (_args: RunStreamArgs, onEvent: (e: EventEnvelope) => void) => {
    const script = scripts.length > 1 ? scripts.shift()! : scripts[0];
    const { deliver, end } = script();
    deliver(onEvent);
    return end;
  });
}

function eventCount(dispatch: ReturnType<typeof vi.fn>): number {
  return dispatch.mock.calls.map((c) => c[0]).filter((a) => a.type === runsSlice.actions.eventReceived.type)
    .length;
}

function watchStatusPayloads(dispatch: ReturnType<typeof vi.fn>): Array<Record<string, unknown>> {
  return dispatch.mock.calls
    .map((c) => c[0])
    .filter((a) => a.type === runsSlice.actions.watchStatusChanged.type)
    .map((a) => a.payload.status);
}

afterEach(() => {
  resetRunDataSource();
});

describe('startWatch', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  test('dispatches events received from the stream, skips WatchReady and marks the stream live', async () => {
    scriptStreams([
      () => ({
        deliver: (onEvent) => {
          onEvent({ ...makeEvent(0), type: 'watchReady' });
          onEvent(makeEvent(1));
          onEvent({ ...makeEvent(2), type: 'runCompleted' });
        },
        end: { kind: 'terminal' },
      }),
    ]);
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);

    expect(eventCount(dispatch)).toBe(2);
    const statuses = watchStatusPayloads(dispatch);
    expect(statuses[0]).toMatchObject({ state: 'connecting' });
    expect(statuses.at(-1)).toMatchObject({ state: 'live' });
  });

  test('passes sinceSeq, subscriberId and signal through to the data source', async () => {
    scriptStreams([
      () => ({
        deliver: () => undefined,
        end: { kind: 'terminal' },
      }),
    ]);
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 7, 'sub-7', dispatch, ctrl.signal);

    expect(openRunStreamMock).toHaveBeenCalledTimes(1);
    const [args, onEvent] = openRunStreamMock.mock.calls[0];
    expect(args).toEqual({ runId: 'r1', sinceSeq: 7, subscriberId: 'sub-7', signal: ctrl.signal });
    expect(typeof onEvent).toBe('function');
  });

  test('swallows errors once the caller has aborted', async () => {
    const ctrl = new AbortController();
    openRunStreamMock.mockImplementation(async () => {
      ctrl.abort();
      throw new Error('aborted');
    });
    const dispatch = vi.fn();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    await expect(startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal)).resolves.toBeUndefined();
    expect(warn).not.toHaveBeenCalled();
    // Only the initial 'connecting' status was dispatched; no events and no
    // reconnect churn after the caller aborted.
    expect(eventCount(dispatch)).toBe(0);
    expect(openRunStreamMock).toHaveBeenCalledTimes(1);
    expect(watchStatusPayloads(dispatch)).toHaveLength(1);
    warn.mockRestore();
  });

  test('a terminal end closes the watch without reconnecting', async () => {
    scriptStreams([
      () => ({
        deliver: (onEvent) => {
          onEvent({ ...makeEvent(0), type: 'watchReady' });
          onEvent(makeEvent(1));
          onEvent({ ...makeEvent(2), type: 'runCompleted' });
        },
        end: { kind: 'terminal' },
      }),
    ]);
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);

    // Only the initial connection: a terminal end is normal, not a loss.
    expect(openRunStreamMock).toHaveBeenCalledTimes(1);
    expect(eventCount(dispatch)).toBe(2);
    expect(dispatch.mock.calls.map((c) => c[0].type)).toContain(runsSlice.actions.watchEnded.type);
    const statuses = watchStatusPayloads(dispatch);
    expect(statuses.at(-1)).toMatchObject({ state: 'live' });
  });

  test('a clean end without a terminal event reconnects resuming from the last seq', async () => {
    vi.useFakeTimers();
    // Stream 1: delivers two events, then the server drops the stream
    // (clean end, no terminal event) — a stream loss.
    scriptStreams([
      () => ({
        deliver: (onEvent) => {
          onEvent({ ...makeEvent(0), type: 'watchReady' });
          onEvent(makeEvent(1));
          onEvent(makeEvent(3));
        },
        end: { kind: 'clean' },
      }),
      () => ({
        deliver: (onEvent) => {
          onEvent(makeEvent(4));
        },
        end: { kind: 'terminal' },
      }),
    ]);
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    const promise = startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);
    // First backoff step: 1s.
    await vi.advanceTimersByTimeAsync(1_000);

    expect(openRunStreamMock).toHaveBeenCalledTimes(2);
    const [args] = openRunStreamMock.mock.calls[1];
    // Resume after the last delivered seq, not from the original cursor.
    expect(args).toEqual({ runId: 'r1', sinceSeq: 3, subscriberId: 'sub-1', signal: ctrl.signal });

    const statuses = watchStatusPayloads(dispatch);
    expect(statuses[0]).toMatchObject({ state: 'connecting' });
    expect(statuses[1]).toMatchObject({ state: 'live' });
    expect(statuses[2]).toMatchObject({
      state: 'reconnecting',
      attempt: 1,
      maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS,
    });
    expect(eventCount(dispatch)).toBe(3);

    ctrl.abort();
    await promise;
  });

  test('reconnects use bounded exponential backoff and give up as lost', async () => {
    vi.useFakeTimers();
    openRunStreamMock.mockResolvedValue({ kind: 'error', codeName: 'unavailable', message: 'boom' });
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    const promise = startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);
    // Total backoff: 1+2+4+8+15 (capped from 16) = 30s, then give up.
    await vi.advanceTimersByTimeAsync(30_000);
    await promise;

    // Initial connection + 5 reconnect attempts, then stop.
    expect(openRunStreamMock).toHaveBeenCalledTimes(WATCH_RECONNECT_MAX_ATTEMPTS + 1);

    const statuses = watchStatusPayloads(dispatch);
    expect(statuses[0]).toMatchObject({ state: 'connecting' });
    expect(statuses.filter((s) => s.state === 'reconnecting').map((s) => s.attempt)).toEqual([1, 2, 3, 4, 5]);
    expect(statuses.at(-1)).toMatchObject({ state: 'lost', attempt: 6, maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS });
  });

  test('an unauthenticated stream end stops retrying and expires the session', async () => {
    openRunStreamMock.mockResolvedValue({
      kind: 'unauthenticated',
      message: 'token rejected',
    });
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);

    // No reconnect: auth failures require re-login, not retries.
    expect(openRunStreamMock).toHaveBeenCalledTimes(1);
    const statuses = watchStatusPayloads(dispatch);
    expect(statuses).toHaveLength(2);
    expect(statuses[0]).toMatchObject({ state: 'connecting' });
    expect(statuses[1]).toMatchObject({ state: 'unauthenticated' });
    expect(dispatch.mock.calls.map((c) => c[0].type)).toContain(sessionExpired.type);
  });

  test('an abort during the backoff sleep ends the watch without further calls', async () => {
    vi.useFakeTimers();
    openRunStreamMock.mockResolvedValue({ kind: 'error', codeName: 'unavailable', message: 'boom' });
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    const promise = startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);
    // Let the first failure happen and the backoff sleep begin…
    await vi.advanceTimersByTimeAsync(1);
    expect(openRunStreamMock).toHaveBeenCalledTimes(1);
    // …then abort mid-sleep.
    ctrl.abort();
    await promise;

    expect(openRunStreamMock).toHaveBeenCalledTimes(1);
  });

  test('terminal event types are the seam contract vocabulary', () => {
    expect([...TERMINAL_EVENT_TYPES].sort()).toEqual(['runCompleted', 'runFailed']);
  });
});

describe('watchReconnectDelayMs', () => {
  test('doubles per attempt with a ceiling', () => {
    expect(watchReconnectDelayMs(1)).toBe(1_000);
    expect(watchReconnectDelayMs(2)).toBe(2_000);
    expect(watchReconnectDelayMs(3)).toBe(4_000);
    expect(watchReconnectDelayMs(4)).toBe(8_000);
    expect(watchReconnectDelayMs(5)).toBe(15_000);
    expect(watchReconnectDelayMs(6)).toBe(15_000);
    expect(watchReconnectDelayMs(0)).toBe(1_000);
    expect(WATCH_RECONNECT_BASE_MS).toBe(1_000);
    expect(WATCH_RECONNECT_MAX_ATTEMPTS).toBe(5);
  });
});