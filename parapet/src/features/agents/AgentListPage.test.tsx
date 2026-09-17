import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test } from 'vitest';
import { AgentListPage } from './AgentListPage';
import { AgentDetailPage } from './AgentDetailPage';
import { castleApi } from '../../api/castleApi';
import { store } from '../../store';
import { selectAuthExpired, sessionRecovered } from '../../features/auth/sessionSlice';
import { server } from '../../test/mocks/server';
import { serverPath } from '../../test/mocks/handlers';

function agent(criteriaId: string, name: string, extra: Record<string, unknown> = {}) {
  return {
    criteria_id: criteriaId,
    name,
    labels: { hostname: 'dev' },
    status: 'online',
    last_seen_at: new Date().toISOString(),
    ...extra,
  };
}

function renderPage() {
  return render(
    <Provider store={store}>
      <MemoryRouter
        initialEntries={['/agents']}
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <Routes>
          <Route path="/agents" element={<AgentListPage />} />
          <Route path="/agents/:criteriaId" element={<AgentDetailPage />} />
        </Routes>
      </MemoryRouter>
    </Provider>,
  );
}

afterEach(() => {
  cleanup();
  // Module-level session state is shared across tests; clear any expired
  // flag a previous test left behind so the gate starts neutral.
  store.dispatch(sessionRecovered());
  store.dispatch(castleApi.util.resetApiState());
  document.title = 'Parapet — Castle';
});

describe('AgentListPage', () => {
  test('renders agents from the live endpoint', async () => {
    server.use(
      http.post(serverPath('ListAgents'), () =>
        HttpResponse.json({ agents: [agent('agent-1', 'local')], next_page_token: '' }),
      ),
    );

    renderPage();

    expect(await screen.findByText('dev')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Agents' })).toBeInTheDocument();
  });

  test('entries navigate to the agent detail view', async () => {
    server.use(
      http.post(serverPath('ListAgents'), () =>
        HttpResponse.json(
          { agents: [agent('agent-1', 'local', { labels: { hostname: 'dev' } })], next_page_token: '' },
        ),
      ),
    );
    server.use(
      http.post(serverPath('GetAgent'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as { criteriaId?: string };
        return HttpResponse.json({
          criteria_id: body.criteriaId ?? 'agent-1',
          name: 'local',
          labels: { hostname: 'dev' },
          status: 'online',
          registered_at: new Date().toISOString(),
          last_seen_at: new Date().toISOString(),
        });
      }),
    );
    server.use(
      http.post(serverPath('ListRuns'), () => HttpResponse.json({ runs: [], next_page_token: '' })),
    );

    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('link', { name: 'local' }));

    // The agents row link landed on the agent's own detail view.
    expect(await screen.findByTestId('agent-detail')).toBeInTheDocument();
    expect(screen.getByTestId('agent-criteria-id')).toHaveTextContent('agent-1');
    expect(screen.getByRole('heading', { level: 2, name: 'local' })).toBeInTheDocument();
  });
});


describe('AgentListPage state branches', () => {
  test('shows the loading state while the request is in flight', () => {
    server.use(
      http.post(serverPath('ListAgents'), ({ request }) =>
        new Promise<Response>((_resolve, reject) => {
          request.signal.addEventListener('abort', () => reject(request.signal.reason));
        }),
      ),
    );

    renderPage();

    expect(screen.getByText('Loading agents…')).toBeInTheDocument();
  });

  test('shows the empty state with guidance when no agents are registered', async () => {
    server.use(
      http.post(serverPath('ListAgents'), () => HttpResponse.json({ agents: [], next_page_token: '' })),
    );

    renderPage();

    expect(await screen.findByText('No agents yet.')).toBeInTheDocument();
    expect(
      screen.getByText('Agents appear here once they register with Castle.'),
    ).toBeInTheDocument();
    expect(screen.getByTestId('agent-list-empty')).toBeInTheDocument();
    expect(screen.queryByTestId('page-state-retry')).not.toBeInTheDocument();
  });

  test('prompts re-authentication instead of a generic failure when the session expires', async () => {
    server.use(
      http.post(
        serverPath('ListAgents'),
        () => HttpResponse.json({ code: 'unauthenticated', message: 'token rejected' }, { status: 401 }),
      ),
    );

    renderPage();

    // Auth failures get their own state — not "Failed to load agents.".
    expect(await screen.findByText('Session expired')).toBeInTheDocument();
    expect(
      screen.getByText('Your token was rejected by Castle. Sign in again to continue.'),
    ).toBeInTheDocument();
    expect(screen.getByTestId('page-state-reauth')).toBeInTheDocument();
    expect(screen.queryByText('Failed to load agents.')).not.toBeInTheDocument();
    expect(screen.queryByTestId('page-state-retry')).not.toBeInTheDocument();
    expect(selectAuthExpired(store.getState())).toBe(true);
  });

  test('retries the initial load from the error state', async () => {
    let failing = true;
    server.use(
      http.post(serverPath('ListAgents'), () => {
        if (failing) {
          return new HttpResponse(JSON.stringify({ code: 'unavailable', message: 'offline' }), {
            status: 503,
            headers: { 'content-type': 'application/json' },
          });
        }
        return HttpResponse.json({
          agents: [agent('agent-1', 'local')],
          next_page_token: '',
        });
      }),
    );

    renderPage();

    const retry = await screen.findByTestId('page-state-retry', {}, { timeout: 3000 });
    failing = false;
    await userEvent.click(retry);

    expect(await screen.findByText('local')).toBeInTheDocument();
  });
});
