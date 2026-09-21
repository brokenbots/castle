import { render, screen, waitFor } from '@testing-library/react';
import { Provider } from 'react-redux';
import { delay, http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, test } from 'vitest';
import { ConnectionStatus, CONNECTION_POLL_INTERVAL_MS } from './ConnectionStatus';
import { castleApi } from '@castle/run-viewer';
import { store } from '../store';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import { serverPath } from '@castle/run-viewer/src/test/mocks/handlers';

function renderStatus() {
  return render(
    <Provider store={store}>
      <ConnectionStatus />
    </Provider>,
  );
}

// The indicator probes the Castle API (ListAgents) on a slow poll cadence.
describe('ConnectionStatus', () => {
  beforeEach(() => {
    // Each test asserts a different outcome for the same endpoint: drop the
    // cached connection probe so every render issues a fresh request.
    store.dispatch(castleApi.util.resetApiState());
  });

  test('reports online when the API answers', async () => {
    renderStatus();

    expect(await screen.findByText('online')).toBeInTheDocument();
    expect(screen.getByTestId('connection-status')).toHaveAccessibleName(
      'Castle connection: online',
    );
    expect(screen.getByTestId('connection-status')).toHaveAttribute('role', 'status');
  });

  test('shows a connecting state while the probe is in flight', async () => {
    server.use(
      http.post(serverPath('ListAgents'), async () => {
        await delay(500);
        return HttpResponse.json({ agents: [], next_page_token: '' });
      }),
    );

    renderStatus();

    // While no answer has arrived the state is 'connecting', then resolves
    // to 'online' once the probe lands.
    expect(screen.getByText('connecting')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('online')).toBeInTheDocument());
  });

  test('reports offline when the API is unreachable', async () => {
    server.use(
      http.post(serverPath('ListAgents'), () => HttpResponse.error()),
    );

    renderStatus();

    expect(await screen.findByText('offline')).toBeInTheDocument();
    expect(screen.getByTestId('connection-status')).toHaveAccessibleName(
      'Castle connection: offline',
    );
  });

  test('exposes the poll cadence for shell consumers', () => {
    expect(CONNECTION_POLL_INTERVAL_MS).toBeGreaterThan(0);
  });
});