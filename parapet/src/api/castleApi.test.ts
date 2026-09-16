import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test } from 'vitest';
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