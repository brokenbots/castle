import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test } from 'vitest';
import { AgentDetailPage } from './AgentDetailPage';
import { castleApi } from '@castle/run-viewer';
import { store } from '../../store';
import { selectAuthExpired, sessionRecovered } from '@castle/run-viewer';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import { serverPath } from '@castle/run-viewer/src/test/mocks/handlers';

// Fixtures use timestamps relative to "now" so relative-time assertions stay
// stable regardless of when the suite runs.
const HOUR_MS = 3_600_000;

function run(id: string, status: string, extra: Record<string, unknown> = {}) {
  return {
    run_id: id,
    criteria_id: 'agent-1',
    workflow_name: 'demo',
    workflow_hash: 'deadbeef',
    status,
    created_at: new Date(Date.now() - HOUR_MS).toISOString(),
    started_at: new Date(Date.now() - HOUR_MS).toISOString(),
    final_state: '',
    failure_reason: '',
    ...extra,
  };
}

// Captures ListRuns request bodies so tests can assert the criteria_id
// filter reached the wire.
let listRunsBodies: Array<Record<string, unknown>>;

function installListRuns(responder: (body: Record<string, unknown>) => object | never) {
  listRunsBodies = [];
  server.use(
    http.post(serverPath('ListRuns'), async ({ request }) => {
      const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
      listRunsBodies.push(body);
      try {
        return HttpResponse.json(responder(body));
      } catch {
        return HttpResponse.json(
          { code: 'unavailable', message: 'simulated ListRuns failure' },
          { status: 503 },
        );
      }
    }),
  );
}

function bodyString(body: Record<string, unknown>, key: string): string {
  const snake = key.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`);
  return String(body[key] ?? body[snake] ?? '');
}

function renderPage(criteriaId = 'agent-1') {
  return render(
    <Provider store={store}>
      <MemoryRouter
        initialEntries={[`/agents/${criteriaId}`]}
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <Routes>
          <Route path="/agents/:criteriaId" element={<AgentDetailPage />} />
          <Route path="/runs/:id" element={<p>run detail</p>} />
        </Routes>
      </MemoryRouter>
    </Provider>,
  );
}

// The module-level store is shared across tests: unmount before clearing the
// RTK Query cache so a mounted query cannot repopulate the next test's
// cache with a late fulfillment.
afterEach(() => {
  cleanup();
  // Module-level session state is shared across tests; clear any expired
  // flag a previous test left behind so the gate starts neutral.
  store.dispatch(sessionRecovered());
  store.dispatch(castleApi.util.resetApiState());
  document.title = 'Parapet — Castle';
});

describe('AgentDetailPage', () => {
  test('renders identity, labels, status and registered/last-seen info from GetAgent', async () => {
    const getAgentBodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(serverPath('GetAgent'), async ({ request }) => {
        getAgentBodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({
          criteria_id: 'agent-1',
          name: 'build-runner',
          labels: { hostname: 'runner-7', team: 'ci' },
          status: 'online',
          registered_at: new Date(Date.now() - 48 * HOUR_MS).toISOString(),
          last_seen_at: new Date(Date.now() - 2 * 60_000).toISOString(),
        });
      }),
    );
    installListRuns(() => ({ runs: [], next_page_token: '' }));

    renderPage('agent-1');

    const detail = await screen.findByTestId('agent-detail');
    expect(detail).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 2, name: 'build-runner' })).toBeInTheDocument();
    // The route param reached the RPC as the criteria_id lookup key.
    expect(getAgentBodies).toHaveLength(1);
    expect(
      String(getAgentBodies[0].criteriaId ?? getAgentBodies[0].criteria_id),
    ).toBe('agent-1');

    expect(screen.getByTestId('agent-criteria-id')).toHaveTextContent('agent-1');
    expect(screen.getByTestId('agent-status')).toHaveTextContent('online');

    const labels = screen.getByTestId('agent-labels');
    expect(labels).toHaveTextContent('hostname:');
    expect(labels).toHaveTextContent('runner-7');
    expect(labels).toHaveTextContent('team:');
    expect(labels).toHaveTextContent('ci');

    // Timestamps render (locale-independent assertions: the values exist and
    // the relative-time variants are covered by the shared cells).
    expect(screen.getByTestId('agent-registered')).toHaveTextContent(/\d/);
    expect(screen.getByTestId('agent-last-seen')).toHaveTextContent(/\d/);
  });

  test('lists the agent runs fetched with the criteria_id filter', async () => {
    installListRuns((body) => {
      if (bodyString(body, 'criteriaId') !== 'agent-1') {
        // Wrong filter: pretend the agent has no runs so the mismatch is
        // visible in the assertion on the request body below.
        return { runs: [], next_page_token: '' };
      }
      return { runs: [run('run-1', 'running'), run('run-2', 'succeeded')], next_page_token: '' };
    });

    renderPage('agent-1');

    expect(await screen.findByRole('link', { name: 'run-1' })).toHaveAttribute('href', '/runs/run-1');
    expect(screen.getByRole('link', { name: 'run-2' })).toBeInTheDocument();
    expect(listRunsBodies).toHaveLength(1);
    expect(bodyString(listRunsBodies[0], 'criteriaId')).toBe('agent-1');
  });

  test('starts with an empty runs list when the agent has not run anything', async () => {
    installListRuns(() => ({ runs: [], next_page_token: '' }));

    renderPage();

    expect(await screen.findByText('No runs for this agent yet.')).toBeInTheDocument();
  });

  test("Load more appends the next page through the pagination cursor", async () => {
    installListRuns((body) => {
      const pageToken = bodyString(body, 'pageToken');
      if (!pageToken) return { runs: [run('run-1', 'running')], next_page_token: 'cursor-2' };
      return { runs: [run('run-2', 'succeeded')], next_page_token: '' };
    });

    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: 'Load more' }));

    await waitFor(() => {
      expect(screen.getByRole('link', { name: 'run-2' })).toBeInTheDocument();
    });
    expect(screen.getByRole('link', { name: 'run-1' })).toBeInTheDocument();
    // Two fetches: page 1 (no cursor) and the cursor page.
    expect(listRunsBodies).toHaveLength(2);
    expect(bodyString(listRunsBodies[1], 'pageToken')).toBe('cursor-2');
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
  });

  test('shows the not-found state when GetAgent fails', async () => {
    server.use(
      http.post(serverPath('GetAgent'), () =>
        HttpResponse.json({ code: 'not_found', message: 'unknown agent' }, { status: 404 }),
      ),
    );

    renderPage('ghost-agent');

    expect(await screen.findByText('Agent not found.')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-detail')).not.toBeInTheDocument();
  });

  test('shows the loading state while the request is in flight', async () => {
    server.use(
      http.post(serverPath('GetAgent'), ({ request }) =>
        new Promise<Response>((_resolve, reject) => {
          request.signal.addEventListener('abort', () => reject(request.signal.reason));
        }),
      ),
    );

    renderPage();

    expect(screen.getByText('Loading agent…')).toBeInTheDocument();
  });

  test('links back to the agents list through the breadcrumb', async () => {
    installListRuns(() => ({ runs: [], next_page_token: '' }));

    renderPage();

    const breadcrumbs = await screen.findByTestId('breadcrumbs');
    expect(breadcrumbs).toHaveAttribute('aria-label', 'Breadcrumb');
    const agentsCrumb = breadcrumbs.querySelector('a');
    expect(agentsCrumb).toHaveAttribute('href', '/agents');
    expect(agentsCrumb).toHaveTextContent('Agents');
  });

  test('the tab title reflects the agent name while the view is open', async () => {
    server.use(
      http.post(serverPath('GetAgent'), () =>
        HttpResponse.json({
          criteria_id: 'agent-1',
          name: 'build-runner',
          labels: {},
          status: 'online',
          registered_at: new Date().toISOString(),
          last_seen_at: new Date().toISOString(),
        }),
      ),
    );
    installListRuns(() => ({ runs: [], next_page_token: '' }));

    renderPage('agent-1');

    await screen.findByTestId('agent-detail');
    await waitFor(() => expect(document.title).toBe('build-runner — Parapet — Castle'));
  });
});


describe('AgentDetailPage load-error states', () => {
  test('prompts re-authentication when GetAgent rejects with 401', async () => {
    server.use(
      http.post(
        serverPath('GetAgent'),
        () => HttpResponse.json({ code: 'unauthenticated', message: 'token rejected' }, { status: 401 }),
      ),
    );

    renderPage('agent-1');

    // The auth failure must never fall through to "Agent not found.".
    expect(await screen.findByText('Session expired')).toBeInTheDocument();
    expect(
      screen.getByText('Your token was rejected by Castle. Sign in again to continue.'),
    ).toBeInTheDocument();
    expect(screen.getByTestId('page-state-reauth')).toBeInTheDocument();
    expect(screen.queryByText('Agent not found.')).not.toBeInTheDocument();
    expect(screen.queryByTestId('page-state-retry')).not.toBeInTheDocument();
    expect(selectAuthExpired(store.getState())).toBe(true);
  });

  test('retries the load from the server-error state', async () => {
    let failing = true;
    server.use(
      http.post(serverPath('GetAgent'), () => {
        if (failing) {
          return new HttpResponse(JSON.stringify({ code: 'unavailable', message: 'offline' }), {
            status: 503,
            headers: { 'content-type': 'application/json' },
          });
        }
        return HttpResponse.json({
          criteria_id: 'agent-1',
          name: 'build-runner',
          labels: { hostname: 'runner-7' },
          status: 'online',
          registered_at: new Date(Date.now() - 48 * HOUR_MS).toISOString(),
          last_seen_at: new Date(Date.now() - 2 * 60_000).toISOString(),
        });
      }),
    );
    installListRuns(() => ({ runs: [], next_page_token: '' }));

    renderPage('agent-1');

    const retry = await screen.findByTestId('page-state-retry', {}, { timeout: 3000 });
    expect(await screen.findByText('Failed to load this agent.')).toBeInTheDocument();
    failing = false;
    await userEvent.click(retry);

    expect(await screen.findByRole('heading', { level: 2, name: 'build-runner' })).toBeInTheDocument();
  });
});
