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

    expect(await screen.findByRole('heading', { name: 'Agents' })).toBeInTheDocument();
    expect(screen.getByText('dev')).toBeInTheDocument();
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