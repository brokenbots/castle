import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, test, vi } from 'vitest';
import { configureStore } from '@reduxjs/toolkit';
import { createApi, fakeBaseQuery } from '@reduxjs/toolkit/query';
import type { Middleware } from '@reduxjs/toolkit';
import { createRunViewerStore, createUnauthenticatedErrorMiddleware } from './store';
import { sessionRecovered, sessionSlice } from './features/auth/sessionSlice';
import { castleApi, RUN_VIEWER_API_REDUCER_PATH } from './api/castleApi';
import { server } from './test/mocks/server';
import { serverPath } from './test/mocks/handlers';

describe('createRunViewerStore', () => {
  test('reserves the viewer reducer keys for host extras', () => {
    expect(() => createRunViewerStore({ extraReducers: { castleApi: () => null } })).toThrow(
      /reserves/,
    );
    expect(() => createRunViewerStore({ extraReducers: { runs: () => null } })).toThrow(/reserves/);
    expect(() => createRunViewerStore({ extraReducers: { session: () => null } })).toThrow(
      /reserves/,
    );
    // A host-owned key (like the settings console slice) is accepted.
    expect(() =>
      createRunViewerStore({ extraReducers: { settingsApiHost: () => null } }),
    ).not.toThrow();
  });

  test('concats host middleware after the viewer middleware', () => {
    const hostActions: string[] = [];
    const spy: Middleware = () => (next) => (action) => {
      if (typeof action === 'object' && action !== null && 'type' in action) {
        hostActions.push(String((action as { type: string }).type));
      }
      return next(action);
    };
    const store = createRunViewerStore({ extraMiddleware: [spy] });

    store.dispatch({ type: 'host/probe' });

    expect(hostActions).toContain('host/probe');
  });
});

// A host RTK api slice mirroring the settings console slice: its rejected
// unauthenticated requests must flip the session gate only when its reducer
// path rides in via `extraApiPaths`. The probe fetch rides msw directly so
// no castle wire call is implied.
function buildHostApi() {
  return createApi({
    reducerPath: 'hostApi',
    baseQuery: fakeBaseQuery<{ status: string | number; data?: string }>(),
    tagTypes: [],
    endpoints: (b) => ({
      probe: b.query<unknown, void>({
        queryFn: async () => {
          try {
            const resp = await fetch('/criteria.v1.ServerService/ListRuns', { method: 'POST' });
            const detail = (await resp.json()) as { code?: string; message?: string };
            if (!resp.ok) {
              return {
                error: { status: detail.code ?? String(resp.status), data: detail.message ?? '' },
              };
            }
            return { data: detail };
          } catch (err) {
            return { error: { status: 'CUSTOM_ERROR', data: String(err) } };
          }
        },
      }),
    }),
  });
}

function hostStore(apiPaths: string[]) {
  const hostApi = buildHostApi();
  const store = configureStore({
    reducer: {
      [castleApi.reducerPath]: castleApi.reducer,
      [hostApi.reducerPath]: hostApi.reducer,
      session: sessionSlice.reducer,
    },
    middleware: (getDefault) =>
      getDefault()
        .concat(hostApi.middleware)
        .concat(createUnauthenticatedErrorMiddleware(apiPaths)),
  });
  store.dispatch(sessionRecovered());
  return { store, hostApi };
}

describe('createUnauthenticatedErrorMiddleware apiPaths', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  test('marks the session expired for listed host api paths', async () => {
    const { store, hostApi } = hostStore([RUN_VIEWER_API_REDUCER_PATH, 'hostApi']);

    server.use(
      http.post(serverPath('ListRuns'), () =>
        new HttpResponse(JSON.stringify({ code: 'unauthenticated', message: 'token rejected' }), {
          status: 401,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );

    await store.dispatch(hostApi.endpoints.probe.initiate());

    expect(store.getState().session.authExpired).toBe(true);
  });

  test('does not gate unlisted host api paths', async () => {
    const { store, hostApi } = hostStore([RUN_VIEWER_API_REDUCER_PATH]);

    server.use(
      http.post(serverPath('ListRuns'), () =>
        new HttpResponse(JSON.stringify({ code: 'unauthenticated', message: 'token rejected' }), {
          status: 401,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );

    await store.dispatch(hostApi.endpoints.probe.initiate());

    expect(store.getState().session.authExpired).toBe(false);
  });
});