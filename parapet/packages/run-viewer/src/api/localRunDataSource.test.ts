import { afterEach, describe, expect, test, vi } from 'vitest';
import {
  RUNVIEW_API_BASE,
  RUNVIEW_POLL_MS,
  localRunDataSource,
} from './localRunDataSource';
import { getRunDataSource, resetRunDataSource, setRunDataSource } from './dataSource';
import type { EventEnvelope } from './castleApi';

type FetchCall = { url: string; init?: RequestInit };

/** Installs a fetch stub recording calls and answering from `routes`. */
function stubFetch(routes: (url: string, init?: RequestInit) => unknown) {
  const calls: FetchCall[] = [];
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    calls.push({ url, init });
    const body = routes(url, init);
    if (body instanceof Error) throw body;
    return {
      ok: true,
      status: body === undefined ? 204 : 200,
      json: async () => body,
    };
  });
  vi.stubGlobal('fetch', fetchMock);
  return calls;
}

function jsonError(status: number): unknown {
  return { ok: false, status, json: async () => ({}) };
}

function envelope(seq: number, type: string): EventEnvelope {
  return { schemaVersion: 1, runId: 'run-1', seq, type, correlationId: '', payload: null };
}

/**
 * Wraps a real controller's signal so abort-listener add/remove calls are
 * observable. Only explicit removeEventListener calls count as removed —
 * the `{ once: true }` auto-detach does not — which is exactly what
 * distinguishes a detached listener from a leaked one.
 */
function spySignal(): {
  signal: AbortSignal;
  controller: AbortController;
  addedCount: () => number;
  removedCount: () => number;
} {
  const controller = new AbortController();
  const real = controller.signal;
  let added = 0;
  let removed = 0;
  const signal = {
    get aborted() {
      return real.aborted;
    },
    addEventListener(type: string, listener: EventListener, options?: AddEventListenerOptions) {
      added += 1;
      real.addEventListener(type, listener, options);
    },
    removeEventListener(type: string, listener: EventListener) {
      removed += 1;
      real.removeEventListener(type, listener);
    },
  } as unknown as AbortSignal;
  return { signal, controller, addedCount: () => added, removedCount: () => removed };
}

// The seam is global state; restore the castle default after every test so
// no other test file observes the local implementation.
afterEach(() => {
  resetRunDataSource();
  vi.unstubAllGlobals();
});

describe('localRunDataSource', () => {
  test('defaults the loopback base to the /runview/api route', () => {
    expect(RUNVIEW_API_BASE).toBe('/runview/api');
    expect(RUNVIEW_POLL_MS).toBeGreaterThan(0);
  });

  test('installs through the RunDataSource seam', () => {
    setRunDataSource(localRunDataSource);
    // A seam consumer (runViewerApi / watchRun) resolves against the local
    // implementation, not the castle Connect client.
    expect(getRunDataSource()).toBe(localRunDataSource);
  });

  test('listRuns requests the runs page with loopback query params', async () => {
    const calls = stubFetch(() => ({ runs: [], nextPageToken: '' }));
    const page = await localRunDataSource.listRuns({ criteriaId: 'crn:1', status: 'running', pageToken: 't2' });
    expect(page).toEqual({ runs: [], nextPageToken: '' });
    expect(calls[0].url).toBe(
      `${RUNVIEW_API_BASE}/runs?agent=crn%3A1&status=running&limit=50&cursor=t2`,
    );
  });

  test('listRuns omits empty query params', async () => {
    const calls = stubFetch(() => ({ runs: [], nextPageToken: '' }));
    await localRunDataSource.listRuns({});
    expect(calls[0].url).toBe(`${RUNVIEW_API_BASE}/runs?limit=50`);
  });

  test('getRun and inspectRun fetch the run and inspection resources', async () => {
    const calls = stubFetch((url) =>
      url.includes('/inspect')
        ? { runId: 'run-1', sessionId: 's1', adapter: 'slack', currentStep: 'build', pendingPermissions: [] }
        : { runId: 'run-1', status: 'running' },
    );
    const run = await localRunDataSource.getRun('run-1');
    expect(run).toMatchObject({ runId: 'run-1', status: 'running' });
    const inspection = await localRunDataSource.inspectRun({ runId: 'run-1', sessionId: 's1' });
    expect(inspection).toMatchObject({ adapter: 'slack', currentStep: 'build' });
    expect(calls.map((c) => c.url)).toEqual([
      `${RUNVIEW_API_BASE}/runs/run-1`,
      `${RUNVIEW_API_BASE}/runs/run-1/inspect?session=s1`,
    ]);
  });

  test('encodes run ids in resource paths', async () => {
    const calls = stubFetch(() => ({ runId: 'a/b', status: 'pending' }));
    await localRunDataSource.getRun('a/b');
    expect(calls[0].url).toBe(`${RUNVIEW_API_BASE}/runs/a%2Fb`);
  });

  test('listRunEvents passes since_seq and limit through', async () => {
    const calls = stubFetch(() => ({ events: [], lastSeq: 0, nextSinceSeq: null }));
    await localRunDataSource.listRunEvents({ runId: 'run-1', sinceSeq: 7, limit: 10 });
    expect(calls[0].url).toBe(`${RUNVIEW_API_BASE}/runs/run-1/events?since_seq=7&limit=10`);
  });

  test('listAgents and getAgent fetch the agent resources', async () => {
    const calls = stubFetch((url) =>
      url.endsWith('/agents/crn%3A1')
        ? { criteriaId: 'crn:1', name: 'a', labels: [], status: 'online' }
        : [{ criteriaId: 'crn:1', name: 'a', labels: [], status: 'online' }],
    );
    const agents = await localRunDataSource.listAgents();
    expect(agents).toHaveLength(1);
    const agent = await localRunDataSource.getAgent('crn:1');
    expect(agent).toMatchObject({ name: 'a' });
    expect(calls.map((c) => c.url)).toEqual([
      `${RUNVIEW_API_BASE}/agents`,
      `${RUNVIEW_API_BASE}/agents/crn%3A1`,
    ]);
  });

  test('connectionStatus resolves on a healthy probe and rejects otherwise', async () => {
    const calls = stubFetch(() => undefined);
    await expect(localRunDataSource.connectionStatus()).resolves.toBeUndefined();
    expect(calls[0].url).toBe(`${RUNVIEW_API_BASE}/health`);

    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonError(503)),
    );
    await expect(localRunDataSource.connectionStatus()).rejects.toThrow(/HTTP 503/);
  });

  test('control mutations post the documented body shapes', async () => {
    const calls = stubFetch(() => ({ issuedAt: '2026-09-16T17:00:00.000Z' }));
    await localRunDataSource.resume({ runId: 'run-1', signal: 'sig', payload: { k: 'v' } });
    await localRunDataSource.pauseRun('run-1');
    await localRunDataSource.stopRun({ runId: 'run-1', reason: 'ops' });

    expect(calls.map((c) => ({ url: c.url, method: c.init?.method }))).toEqual([
      { url: `${RUNVIEW_API_BASE}/runs/run-1/resume`, method: 'POST' },
      { url: `${RUNVIEW_API_BASE}/runs/run-1/pause`, method: 'POST' },
      { url: `${RUNVIEW_API_BASE}/runs/run-1/stop`, method: 'POST' },
    ]);
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ signal: 'sig', payload: { k: 'v' } });
    expect(JSON.parse(String(calls[2].init?.body))).toEqual({ reason: 'ops' });
  });

  test('rejects with the HTTP status when the loopback answers an error', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonError(404)),
    );
    await expect(localRunDataSource.getRun('run-1')).rejects.toThrow(/HTTP 404/);
  });

  test('openRunStream delivers watchReady, then events, then terminates', async () => {
    const delivered: EventEnvelope[] = [];
    stubFetch(() => ({
      events: [envelope(1, 'stepEntered'), envelope(2, 'runCompleted')],
      lastSeq: 2,
      nextSinceSeq: null,
    }));

    const end = await localRunDataSource.openRunStream(
      { runId: 'run-1', sinceSeq: 0, subscriberId: 'sub', signal: new AbortController().signal },
      (event) => delivered.push(event),
    );

    expect(end).toEqual({ kind: 'terminal' });
    expect(delivered.map((e) => e.type)).toEqual(['watchReady', 'stepEntered', 'runCompleted']);
    expect(delivered[0]).toMatchObject({ type: 'watchReady', runId: 'run-1' });
  });

  test('openRunStream resumes from sinceSeq and keeps polling until terminal', async () => {
    const delivered: EventEnvelope[] = [];
    let poll = 0;
    const calls = stubFetch(() => {
      poll += 1;
      return poll === 1
        ? { events: [envelope(3, 'stepEntered')], lastSeq: 3, nextSinceSeq: null }
        : { events: [envelope(4, 'runFailed')], lastSeq: 4, nextSinceSeq: null };
    });

    const end = await localRunDataSource.openRunStream(
      { runId: 'run-1', sinceSeq: 2, subscriberId: 'sub', signal: new AbortController().signal },
      (event) => delivered.push(event),
    );

    expect(end).toEqual({ kind: 'terminal' });
    expect(delivered.map((e) => e.type)).toEqual(['watchReady', 'stepEntered', 'runFailed']);
    // The second poll resumes strictly after the last delivered seq.
    expect(calls[1].url).toContain('since_seq=3');
  });

  test('openRunStream reports an error end when the loopback is unreachable', async () => {
    stubFetch(() => new Error('network down'));
    const end = await localRunDataSource.openRunStream(
      { runId: 'run-1', sinceSeq: 0, subscriberId: 'sub', signal: new AbortController().signal },
      () => {},
    );
    expect(end).toEqual({ kind: 'error', message: 'network down' });
  });

  test('openRunStream classifies an abort before the first poll as clean', async () => {
    const controller = new AbortController();
    controller.abort();
    stubFetch(() => ({ events: [], lastSeq: 0, nextSinceSeq: null }));
    const end = await localRunDataSource.openRunStream(
      { runId: 'run-1', sinceSeq: 0, subscriberId: 'sub', signal: controller.signal },
      () => {},
    );
    expect(end).toEqual({ kind: 'clean' });
  });

  test('openRunStream resolves clean when aborted between polls', async () => {
    const controller = new AbortController();
    stubFetch(() => ({ events: [], lastSeq: 0, nextSinceSeq: null }));
    const pending = localRunDataSource.openRunStream(
      { runId: 'run-1', sinceSeq: 0, subscriberId: 'sub', signal: controller.signal },
      () => {},
    );
    controller.abort();
    const end = await pending;
    expect(end).toEqual({ kind: 'clean' });
  });

  test('openRunStream delivers watchReady only once across polls', async () => {
    const delivered: EventEnvelope[] = [];
    let poll = 0;
    stubFetch(() => {
      poll += 1;
      return poll <= 2
        ? { events: [], lastSeq: 0, nextSinceSeq: null }
        : { events: [envelope(1, 'runCompleted')], lastSeq: 1, nextSinceSeq: null };
    });

    const end = await localRunDataSource.openRunStream(
      { runId: 'run-1', sinceSeq: 0, subscriberId: 'sub', signal: new AbortController().signal },
      (event) => delivered.push(event),
    );

    expect(end).toEqual({ kind: 'terminal' });
    expect(delivered.filter((e) => e.type === 'watchReady')).toHaveLength(1);
  });

  test('openRunStream detaches the poll-sleep abort listener when the sleep timer fires', async () => {
    vi.useFakeTimers();
    try {
      const delivered: EventEnvelope[] = [];
      let poll = 0;
      stubFetch(() => {
        poll += 1;
        return poll === 1
          ? { events: [], lastSeq: 0, nextSinceSeq: null }
          : { events: [envelope(1, 'runCompleted')], lastSeq: 1, nextSinceSeq: null };
      });

      const { signal, addedCount, removedCount } = spySignal();
      const pending = localRunDataSource.openRunStream(
        { runId: 'run-1', sinceSeq: 0, subscriberId: 'sub', signal },
        (event) => delivered.push(event),
      );
      // Drain the first poll so the sleep (and its abort listener) registers.
      await vi.advanceTimersByTimeAsync(0);
      expect(addedCount()).toBe(1);
      // The sleep timer fires, the loop polls again and lands on the
      // terminal event — the sleep's listener must not outlive the sleep.
      await vi.advanceTimersByTimeAsync(RUNVIEW_POLL_MS);

      expect(await pending).toEqual({ kind: 'terminal' });
      expect(delivered.map((e) => e.type)).toEqual(['watchReady', 'runCompleted']);
      expect(removedCount()).toBe(addedCount());
    } finally {
      vi.useRealTimers();
    }
  });

  test('openRunStream detaches the poll-sleep abort listener when abort lands during the sleep', async () => {
    vi.useFakeTimers();
    try {
      stubFetch(() => ({ events: [], lastSeq: 0, nextSinceSeq: null }));

      const { signal, controller, addedCount, removedCount } = spySignal();
      const pending = localRunDataSource.openRunStream(
        { runId: 'run-1', sinceSeq: 0, subscriberId: 'sub', signal },
        () => {},
      );
      await vi.advanceTimersByTimeAsync(0); // first poll done, sleep registered
      expect(addedCount()).toBe(1);

      // Aborting mid-sleep ends the watch as clean and detaches the
      // listener as part of handling the abort.
      controller.abort();
      expect(await pending).toEqual({ kind: 'clean' });
      expect(addedCount()).toBe(1);
      expect(removedCount()).toBe(1);
    } finally {
      vi.useRealTimers();
    }
  });
});