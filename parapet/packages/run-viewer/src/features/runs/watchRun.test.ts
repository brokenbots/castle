import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { ConnectError, Code } from '@connectrpc/connect';
import {
  Envelope,
  LogStream,
  RunCompleted,
  StepLog,
  WatchReady,
} from '../../gen/criteria/v1/events_pb';
import { runsSlice } from './runsSlice';
import { sessionExpired } from '../auth/sessionSlice';

const watchRunMock = vi.fn();

vi.mock('../../api/client', () => ({
  server: {
    watchRun: (...args: unknown[]) => watchRunMock(...args),
  },
}));

// Import after mock is installed.
const {
  startWatch,
  watchReconnectDelayMs,
  WATCH_RECONNECT_BASE_MS,
  WATCH_RECONNECT_MAX_ATTEMPTS,
} = await import('./watchRun');

function makeEnvelope(seq: number, payloadCase: 'stepLog' | 'runCompleted' = 'stepLog'): Envelope {
  const base = { schemaVersion: 1, runId: 'r1', seq: BigInt(seq) };
  if (payloadCase === 'runCompleted') {
    return new Envelope({ ...base, payload: { case: 'runCompleted', value: new RunCompleted({}) } });
  }
  return new Envelope({
    ...base,
    payload: { case: 'stepLog', value: new StepLog({ step: 'build', stream: LogStream.STDOUT, chunk: `#${seq}` }) },
  });
}

function makeWatchReady(): Envelope {
  return new Envelope({
    schemaVersion: 1,
    runId: 'r1',
    seq: BigInt(0),
    payload: { case: 'watchReady', value: new WatchReady({}) },
  });
}

async function* asyncIter(items: Envelope[]): AsyncIterableIterator<Envelope> {
  for (const it of items) yield it;
}

async function* failingIter(err: unknown): AsyncIterableIterator<Envelope> {
  throw err;
  // eslint-disable-next-line no-unreachable
  yield makeEnvelope(1);
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

beforeEach(() => {
  watchRunMock.mockReset();
});

describe('startWatch', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  test('dispatches events received from the stream, skips WatchReady and marks the stream live', async () => {
    watchRunMock.mockReturnValueOnce(
      asyncIter([makeWatchReady(), makeEnvelope(1), makeEnvelope(2, 'runCompleted')]),
    );
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);

    expect(eventCount(dispatch)).toBe(2);
    const statuses = watchStatusPayloads(dispatch);
    expect(statuses[0]).toMatchObject({ state: 'connecting' });
    expect(statuses.at(-1)).toMatchObject({ state: 'live' });
  });

  test('passes sinceSeq and signal through to the Connect client', async () => {
    watchRunMock.mockReturnValueOnce(asyncIter([makeEnvelope(1, 'runCompleted')]));
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 7, 'sub-7', dispatch, ctrl.signal);

    expect(watchRunMock).toHaveBeenCalledTimes(1);
    const [req, opts] = watchRunMock.mock.calls[0];
    expect(req).toEqual({ runId: 'r1', sinceSeq: 7n, subscriberId: 'sub-7' });
    expect(opts).toEqual({ signal: ctrl.signal });
  });

  test('swallows errors once the caller has aborted', async () => {
    const ctrl = new AbortController();
    watchRunMock.mockImplementationOnce(async function* () {
      ctrl.abort();
      throw new Error('aborted');
      // eslint-disable-next-line no-unreachable
      yield makeEnvelope(1);
    });
    const dispatch = vi.fn();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    await expect(startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal)).resolves.toBeUndefined();
    expect(warn).not.toHaveBeenCalled();
    // Only the initial 'connecting' status was dispatched; no events and no
    // reconnect churn after the caller aborted.
    expect(eventCount(dispatch)).toBe(0);
    expect(watchRunMock).toHaveBeenCalledTimes(1);
    expect(watchStatusPayloads(dispatch)).toHaveLength(1);
    warn.mockRestore();
  });

  test('a clean end after a terminal event ends the watch without reconnecting', async () => {
    watchRunMock.mockReturnValueOnce(
      asyncIter([makeWatchReady(), makeEnvelope(1), makeEnvelope(2, 'runCompleted')]),
    );
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);

    // Only the initial connection: a terminal end is normal, not a loss.
    expect(watchRunMock).toHaveBeenCalledTimes(1);
    expect(eventCount(dispatch)).toBe(2);
    expect(dispatch.mock.calls.map((c) => c[0].type)).toContain(runsSlice.actions.watchEnded.type);
    const statuses = watchStatusPayloads(dispatch);
    expect(statuses.at(-1)).toMatchObject({ state: 'live' });
  });

  test('a clean end without a terminal event reconnects resuming from the last seq', async () => {
    vi.useFakeTimers();
    // Stream 1: delivers two events, then the server drops the stream
    // (clean end, no terminal event) — a stream loss.
    watchRunMock.mockReturnValueOnce(asyncIter([makeWatchReady(), makeEnvelope(1), makeEnvelope(3)]));
    // Stream 2: the reconnected stream delivers a newer event.
    watchRunMock.mockReturnValueOnce(asyncIter([makeEnvelope(4)]));
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    const promise = startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);
    // First backoff step: 1s.
    await vi.advanceTimersByTimeAsync(1_000);

    expect(watchRunMock).toHaveBeenCalledTimes(2);
    const [req] = watchRunMock.mock.calls[1];
    // Resume after the last delivered seq, not from the original cursor.
    expect(req).toEqual({ runId: 'r1', sinceSeq: 3n, subscriberId: 'sub-1' });

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
    watchRunMock.mockImplementation(() => failingIter(new Error('boom')));
    const dispatch = vi.fn();
    const ctrl = new AbortController();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    const promise = startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);
    // Total backoff: 1+2+4+8+15 (capped from 16) = 30s, then give up.
    await vi.advanceTimersByTimeAsync(30_000);
    await promise;

    // Initial connection + 5 reconnect attempts, then stop.
    expect(watchRunMock).toHaveBeenCalledTimes(WATCH_RECONNECT_MAX_ATTEMPTS + 1);

    const statuses = watchStatusPayloads(dispatch);
    expect(statuses[0]).toMatchObject({ state: 'connecting' });
    expect(statuses.filter((s) => s.state === 'reconnecting').map((s) => s.attempt)).toEqual([1, 2, 3, 4, 5]);
    expect(statuses.at(-1)).toMatchObject({ state: 'lost', attempt: 6, maxAttempts: WATCH_RECONNECT_MAX_ATTEMPTS });
    warn.mockRestore();
  });

  test('an unauthenticated stream end stops retrying and expires the session', async () => {
    watchRunMock.mockReturnValueOnce(
      failingIter(new ConnectError('token rejected', Code.Unauthenticated)),
    );
    const dispatch = vi.fn();
    const ctrl = new AbortController();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);

    // No reconnect: auth failures require re-login, not retries.
    expect(watchRunMock).toHaveBeenCalledTimes(1);
    const statuses = watchStatusPayloads(dispatch);
    expect(statuses).toHaveLength(2);
    expect(statuses[0]).toMatchObject({ state: 'connecting' });
    expect(statuses[1]).toMatchObject({ state: 'unauthenticated' });
    expect(dispatch.mock.calls.map((c) => c[0].type)).toContain(sessionExpired.type);
    warn.mockRestore();
  });

  test('an abort during the backoff sleep ends the watch without further calls', async () => {
    vi.useFakeTimers();
    watchRunMock.mockImplementation(() => failingIter(new Error('boom')));
    const dispatch = vi.fn();
    const ctrl = new AbortController();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    const promise = startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);
    // Let the first failure happen and the backoff sleep begin…
    await vi.advanceTimersByTimeAsync(1);
    expect(watchRunMock).toHaveBeenCalledTimes(1);
    // …then abort mid-sleep.
    ctrl.abort();
    await promise;

    expect(watchRunMock).toHaveBeenCalledTimes(1);
    warn.mockRestore();
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