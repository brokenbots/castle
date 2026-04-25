import { beforeEach, describe, expect, test, vi } from 'vitest';
import { Envelope, LogStream, StepLog, WatchReady } from '../../gen/overlord/v1/events_pb';
import { runsSlice } from './runsSlice';

const watchRunMock = vi.fn();

vi.mock('../../api/client', () => ({
  castle: {
    watchRun: (...args: unknown[]) => watchRunMock(...args),
  },
}));

// Import after mock is installed.
const { startWatch } = await import('./watchRun');

function makeEnvelope(seq: number): Envelope {
  return new Envelope({
    schemaVersion: 1,
    runId: 'r1',
    seq: BigInt(seq),
    payload: {
      case: 'stepLog',
      value: new StepLog({ step: 'build', stream: LogStream.STDOUT, chunk: `#${seq}` }),
    },
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

describe('startWatch', () => {
  beforeEach(() => {
    watchRunMock.mockReset();
  });

  test('dispatches events received from the stream and skips WatchReady', async () => {
    watchRunMock.mockReturnValueOnce(asyncIter([makeWatchReady(), makeEnvelope(1), makeEnvelope(2)]));
    const dispatch = vi.fn();
    const ctrl = new AbortController();

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);

    const actions = dispatch.mock.calls.map((c) => c[0]);
    const received = actions.filter((a) => a.type === runsSlice.actions.eventReceived.type);
    expect(received).toHaveLength(2);
    expect(received[0].payload.seq).toBe(1);
    expect(received[1].payload.seq).toBe(2);
  });

  test('passes sinceSeq and signal through to the Connect client', async () => {
    watchRunMock.mockReturnValueOnce(asyncIter([]));
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
    expect(dispatch).not.toHaveBeenCalled();
    warn.mockRestore();
  });

  test('logs and returns when the stream errors without abort', async () => {
    watchRunMock.mockImplementationOnce(async function* () {
      throw new Error('boom');
      // eslint-disable-next-line no-unreachable
      yield makeEnvelope(1);
    });
    const dispatch = vi.fn();
    const ctrl = new AbortController();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);

    await startWatch('r1', 0, 'sub-1', dispatch, ctrl.signal);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(dispatch).not.toHaveBeenCalled();
    warn.mockRestore();
  });
});
