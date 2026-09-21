import { describe, expect, test } from 'vitest';
import { sessionRecovered, selectAuthExpired, sessionSlice, sessionExpired } from './sessionSlice';
import { castleApi } from '../../api/castleApi';
import { createRunViewerStore } from '../../store';
import { server } from '../../test/mocks/server';
import { serverPath } from '../../test/mocks/handlers';
import { http, HttpResponse } from 'msw';

// One store per file: the middleware behavior is asserted against real
// dispatches through the package's factory.
const store = createRunViewerStore();

describe('sessionSlice reducer', () => {
  test('sessionExpired marks the session and sessionRecovered clears it', () => {
    let state = sessionSlice.getInitialState();
    state = sessionSlice.reducer(state, sessionExpired());
    expect(state.authExpired).toBe(true);
    state = sessionSlice.reducer(state, sessionRecovered());
    expect(state.authExpired).toBe(false);
  });
});

describe('auth-expiry middleware', () => {
  test('a 401 rejection from a Castle API endpoint marks the session expired', async () => {
    store.dispatch(sessionRecovered());
    server.use(
      http.post(serverPath('ListAgents'), () =>
        HttpResponse.json(
          { code: 'unauthenticated', message: 'token rejected' },
          { status: 401 },
        ),
      ),
    );

    await expect(
      store.dispatch(castleApi.endpoints.getConnectionStatus.initiate(undefined, { forceRefetch: true })).unwrap(),
    ).rejects.toMatchObject({ status: 'unauthenticated' });

    expect(selectAuthExpired(store.getState())).toBe(true);
  });

  test('other error kinds do not mark the session expired', async () => {
    store.dispatch(sessionRecovered());
    server.use(
      http.post(serverPath('ListAgents'), () =>
        HttpResponse.json({ code: 'unavailable', message: 'offline' }, { status: 503 }),
      ),
    );

    await expect(
      store.dispatch(castleApi.endpoints.getConnectionStatus.initiate(undefined, { forceRefetch: true })).unwrap(),
    ).rejects.toMatchObject({ status: 'unavailable' });

    expect(selectAuthExpired(store.getState())).toBe(false);
  });

  test('session recovery is observable through the store', () => {
    store.dispatch(sessionExpired());
    expect(selectAuthExpired(store.getState())).toBe(true);
    store.dispatch(sessionRecovered());
    expect(selectAuthExpired(store.getState())).toBe(false);
  });
});