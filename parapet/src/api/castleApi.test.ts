import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test, vi } from 'vitest';
import { castleApi } from './castleApi';
import { store } from '../store';
import { server } from '../test/mocks/server';
import { serverPath } from '../test/mocks/handlers';

// The store persists across tests; drop cached mutation entries so the
// dispatches below always hit the wire.
afterEach(() => {
  store.dispatch(castleApi.util.resetApiState());
});

describe('castleApi run-control mutations', () => {
  test('stopRun posts the run id and surfaces issued_at', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(serverPath('StopRun'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        bodies.push(body);
        return HttpResponse.json({ issued_at: '2026-09-16T17:00:00.000Z' });
      }),
    );

    const res = await store.dispatch(
      castleApi.endpoints.stopRun.initiate({ runId: 'run-1' }),
    );

    expect(bodies).toHaveLength(1);
    expect(String(bodies[0].runId ?? bodies[0].run_id)).toBe('run-1');
    expect(res.data).toEqual({ issuedAt: '2026-09-16T17:00:00.000Z' });
  });

  test('pauseRun posts the run id and surfaces issued_at', async () => {
    const res = await store.dispatch(
      castleApi.endpoints.pauseRun.initiate({ runId: 'run-1' }),
    );

    expect(res.data).toEqual({ issuedAt: expect.any(String) });
  });

  test('resumeRun surfaces issued_at', async () => {
    const res = await store.dispatch(
      castleApi.endpoints.resume.initiate({ runId: 'run-1', signal: 'go' }),
    );

    expect(res.data).toEqual({ issuedAt: expect.any(String) });
  });

  test('maps connect errors onto a readable status/data shape', async () => {
    server.use(
      http.post(
        serverPath('StopRun'),
        () =>
          new HttpResponse(
            JSON.stringify({ code: 'failed_precondition', message: 'criteria agent not connected' }),
            { status: 412, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    const res = await store.dispatch(
      castleApi.endpoints.stopRun.initiate({ runId: 'run-1' }),
    );

    expect(res.data).toBeUndefined();
    expect(res.error).toEqual({
      status: 'failed_precondition',
      data: 'criteria agent not connected',
    });
  });
});

// Runs fixture shape mirrors the ListRuns MSW handler (snake_case protojson).
function runFixture(id: string, status: string, extra: Record<string, unknown> = {}) {
  return {
    run_id: id,
    criteria_id: 'crn:v1:criteria:workflow/demo',
    workflow_name: 'demo',
    workflow_hash: 'deadbeef',
    status,
    created_at: '2026-02-05T08:30:00.000Z',
    final_state: '',
    failure_reason: '',
    ...extra,
  };
}

describe('castleApi listRuns', () => {
  test('sends status filter and limit on the first request', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(serverPath('ListRuns'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({ runs: [runFixture('run-1', 'succeeded')], next_page_token: '' });
      }),
    );

    await store.dispatch(castleApi.endpoints.listRuns.initiate({ status: 'running' }));

    expect(bodies).toHaveLength(1);
    expect(bodies[0].status).toBe('running');
    expect(bodies[0].limit).toBe(50);
  });

  test('serves page 1 and a cursor page as separate cache entries', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(serverPath('ListRuns'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        bodies.push(body);
        const pageToken = String(body.pageToken ?? body.page_token ?? '');
        if (pageToken === '') {
          return HttpResponse.json({
            runs: [runFixture('run-1', 'succeeded'), runFixture('run-2', 'running')],
            next_page_token: 'tok-2',
          });
        }
        return HttpResponse.json({ runs: [runFixture('run-3', 'failed')], next_page_token: '' });
      }),
    );

    await store.dispatch(castleApi.endpoints.listRuns.initiate({ status: '' }));
    await store.dispatch(
      castleApi.endpoints.listRuns.initiate(
        { status: '', pageToken: 'tok-2' },
        { forceRefetch: true },
      ),
    );

    expect(bodies).toHaveLength(2);
    expect(String(bodies[1].pageToken ?? bodies[1].page_token ?? '')).toBe('tok-2');

    const state = store.getState();
    const page1 = castleApi.endpoints.listRuns.select({ status: '' })(state).data;
    expect(page1?.runs.map((r) => r.runId)).toEqual(['run-1', 'run-2']);
    expect(page1?.nextPageToken).toBe('tok-2');
    const cursorPage = castleApi.endpoints.listRuns.select({
      status: '',
      pageToken: 'tok-2',
    })(state).data;
    expect(cursorPage?.runs.map((r) => r.runId)).toEqual(['run-3']);
    expect(cursorPage?.nextPageToken).toBe('');
  });

  test('keeps one cache entry per status filter', async () => {
    server.use(
      http.post(serverPath('ListRuns'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        const status = String(body.status ?? '');
        return HttpResponse.json({
          runs: [runFixture(`run-${status || 'all'}`, status || 'succeeded')],
          next_page_token: '',
        });
      }),
    );

    await store.dispatch(castleApi.endpoints.listRuns.initiate({ status: '' }));
    await store.dispatch(castleApi.endpoints.listRuns.initiate({ status: 'running' }));

    const state = store.getState();
    expect(castleApi.endpoints.listRuns.select({ status: '' })(state).data?.runs[0].runId).toBe(
      'run-all',
    );
    expect(castleApi.endpoints.listRuns.select({ status: 'running' })(state).data?.runs[0].runId).toBe(
      'run-running',
    );
  });

  test('refetches a cursor page in place without touching the first page', async () => {
    server.use(
      http.post(serverPath('ListRuns'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        const pageToken = String(body.pageToken ?? body.page_token ?? '');
        if (pageToken === '') {
          return HttpResponse.json({
            runs: [runFixture('run-1', 'succeeded')],
            next_page_token: 'tok-2',
          });
        }
        return HttpResponse.json({
          runs: [runFixture('run-2', 'running'), runFixture('run-1', 'succeeded')],
          next_page_token: '',
        });
      }),
    );

    await store.dispatch(castleApi.endpoints.listRuns.initiate({ status: '' }));
    await store.dispatch(
      castleApi.endpoints.listRuns.initiate(
        { status: '', pageToken: 'tok-2' },
        { forceRefetch: true },
      ),
    );

    const cursorSelect = castleApi.endpoints.listRuns.select({ status: '', pageToken: 'tok-2' });
    const page1Select = castleApi.endpoints.listRuns.select({ status: '' });
    expect(cursorSelect(store.getState()).data?.runs.map((r) => r.runId)).toEqual([
      'run-2',
      'run-1',
    ]);
    expect(page1Select(store.getState()).data?.runs.map((r) => r.runId)).toEqual(['run-1']);
    expect(page1Select(store.getState()).data?.nextPageToken).toBe('tok-2');

    // A repeated cursor fetch replaces that page's entry in place; page 1
    // stays exactly as the last page-1 fetch returned it.
    await store.dispatch(
      castleApi.endpoints.listRuns.initiate(
        { status: '', pageToken: 'tok-2' },
        { forceRefetch: true },
      ),
    );
    expect(cursorSelect(store.getState()).data?.runs.map((r) => r.runId)).toEqual([
      'run-2',
      'run-1',
    ]);
    expect(page1Select(store.getState()).data?.runs.map((r) => r.runId)).toEqual(['run-1']);
  });

  test('invalidating the Run tag refetches every loaded page', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    let page1Fetches = 0;
    let cursorFetches = 0;
    server.use(
      http.post(serverPath('ListRuns'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        bodies.push(body);
        const pageToken = String(body.pageToken ?? body.page_token ?? '');
        if (pageToken === '') {
          page1Fetches += 1;
          return HttpResponse.json({
            runs: [runFixture(`run-1-${page1Fetches}`, 'succeeded')],
            next_page_token: 'tok-2',
          });
        }
        cursorFetches += 1;
        return HttpResponse.json({
          runs: [runFixture(`run-3-${cursorFetches}`, 'failed')],
          next_page_token: '',
        });
      }),
    );

    await store.dispatch(castleApi.endpoints.listRuns.initiate({ status: '' }));
    await store.dispatch(
      castleApi.endpoints.listRuns.initiate(
        { status: '', pageToken: 'tok-2' },
        { forceRefetch: true },
      ),
    );
    expect(bodies).toHaveLength(2);

    store.dispatch(castleApi.util.invalidateTags(['Run']));
    // Wait for both refetched responses to be committed to the store —
    // a recorded request body does not imply its response has landed.
    await vi.waitFor(() => {
      const state = store.getState();
      expect(
        castleApi.endpoints.listRuns.select({ status: '' })(state).data?.runs[0].runId,
      ).toBe('run-1-2');
      expect(
        castleApi.endpoints.listRuns.select({ status: '', pageToken: 'tok-2' })(state).data?.runs[0]
          .runId,
      ).toBe('run-3-2');
    });

    // Both pages refetched independently — page 1 keeps its own cursor-free
    // args so polls and invalidations can never re-request the cursor page.
    const refetchedTokens = bodies
      .slice(2)
      .map((body) => String(body.pageToken ?? body.page_token ?? ''))
      .sort();
    expect(refetchedTokens).toEqual(['', 'tok-2']);
    expect(bodies).toHaveLength(4);
  });

  test('maps connect errors onto the readable error shape', async () => {
    server.use(
      http.post(
        serverPath('ListRuns'),
        () =>
          new HttpResponse(
            JSON.stringify({ code: 'unavailable', message: 'criteria store offline' }),
            { status: 503, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );

    await store.dispatch(castleApi.endpoints.listRuns.initiate({ status: '' }));

    const entry = castleApi.endpoints.listRuns.select({ status: '' })(store.getState());
    expect(entry.error).toEqual({ status: 'unavailable', data: 'criteria store offline' });
  });
});