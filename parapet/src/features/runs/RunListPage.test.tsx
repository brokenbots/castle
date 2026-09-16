import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test, vi } from 'vitest';
import { RunListPage, RUN_LIST_POLL_INTERVAL_MS } from './RunListPage';
import { castleApi } from '../../api/castleApi';
import { store } from '../../store';
import { server } from '../../test/mocks/server';
import { serverPath } from '../../test/mocks/handlers';

// Runs fixture shape mirrors the ListRuns MSW handler (snake_case protojson).
function run(id: string, status: string, extra: Record<string, unknown> = {}) {
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

interface ListRunsPage {
  runs: unknown[];
  nextPageToken: string;
}

// Installs a ListRuns override that records request bodies (camelCase or
// snake_case keys) and answers with pages produced by `responder`.
function installListRuns(responder: (pageToken: string, status: string) => ListRunsPage) {
  const bodies: Array<Record<string, unknown>> = [];
  server.use(
    http.post(serverPath('ListRuns'), async ({ request }) => {
      const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
      bodies.push(body);
      const pageToken = String(body.pageToken ?? body.page_token ?? '');
      const status = String(body.status ?? '');
      const page = responder(pageToken, status);
      return HttpResponse.json({ runs: page.runs, next_page_token: page.nextPageToken });
    }),
  );
  return bodies;
}

function bodyPageToken(body: Record<string, unknown>): string {
  return String(body.pageToken ?? body.page_token ?? '');
}

function bodyStatus(body: Record<string, unknown>): string {
  return String(body.status ?? '');
}

function renderPage() {
  return render(
    <Provider store={store}>
      <MemoryRouter initialEntries={['/runs']}>
        <RunListPage />
      </MemoryRouter>
    </Provider>,
  );
}

// Flushing an in-flight fetch under fake timers: the mocked chain (MSW ->
// undici -> connect -> RTK Query) needs real event-loop turns, so advance a
// little faked time (yielding between timer batches) inside act.
async function settle() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(100);
  });
}

function hideTab() {
  Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
  document.dispatchEvent(new Event('visibilitychange'));
}

function showTab() {
  Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
  document.dispatchEvent(new Event('visibilitychange'));
}

afterEach(() => {
  // Unmount before resetting the API state: while a component is still
  // mounted, clearing its cache makes RTK Query immediately re-initiate the
  // query, and that late fulfillment can pollute the next test's cache.
  cleanup();
  // Restore the jsdom prototype getter patched by hideTab().
  delete (document as { visibilityState?: unknown }).visibilityState;
  vi.useRealTimers();
  store.dispatch(castleApi.util.resetApiState());
});

describe('RunListPage', () => {
  test('renders runs from the live endpoint with a detail link', async () => {
    server.use(
      http.post(serverPath('ListRuns'), () =>
        HttpResponse.json({
          runs: [run('run-1', 'running', { ticket: 'CRI-187' })],
          next_page_token: '',
        }),
      ),
    );

    renderPage();

    expect(await screen.findByText('CRI-187')).toBeInTheDocument();
    const link = screen.getByRole('link', { name: 'run-1' });
    expect(link.getAttribute('href')).toBe('/runs/run-1');
  });

  test('shows the loading state while the request is in flight', async () => {
    // A request that never resolves on its own; abort (e.g. resetApiState in
    // afterEach) must settle it so no pending query outlives the test.
    server.use(
      http.post(serverPath('ListRuns'), ({ request }) =>
        new Promise<Response>((_resolve, reject) => {
          request.signal.addEventListener('abort', () => reject(request.signal.reason));
        }),
      ),
    );

    renderPage();

    expect(screen.getByText('Loading runs…')).toBeInTheDocument();
  });

  test('shows the empty state when the server has no runs', async () => {
    server.use(
      http.post(serverPath('ListRuns'), () => HttpResponse.json({ runs: [], next_page_token: '' })),
    );

    renderPage();

    expect(await screen.findByText('No runs.')).toBeInTheDocument();
  });

  test('shows the error state when the request fails', async () => {
    server.use(
      http.post(
        serverPath('ListRuns'),
        () =>
          new HttpResponse(JSON.stringify({ code: 'unavailable', message: 'offline' }), {
            status: 503,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    renderPage();

    expect(await screen.findByText('Failed to load runs.')).toBeInTheDocument();
  });

  test('passes the status filter and page limit to ListRuns', async () => {
    const user = userEvent.setup();
    const bodies = installListRuns(() => ({
      runs: [run('run-1', 'running')],
      nextPageToken: '',
    }));

    renderPage();
    await screen.findByText('run-1');

    await user.selectOptions(screen.getByLabelText('Status'), 'running');
    await waitFor(() => expect(bodies).toHaveLength(2));

    expect(bodyStatus(bodies[0])).toBe('');
    expect(bodies[0].limit).toBe(50);
    expect(bodyStatus(bodies[1])).toBe('running');
    expect(bodies[1].limit).toBe(50);
  });

  test('filtering shows only runs matching the selected status', async () => {
    const user = userEvent.setup();
    installListRuns((_pageToken, status) =>
      status === 'running'
        ? { runs: [run('run-2', 'running')], nextPageToken: '' }
        : {
            runs: [run('run-1', 'succeeded'), run('run-2', 'running')],
            nextPageToken: '',
          },
    );

    renderPage();
    expect(await screen.findByText('run-1')).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('Status'), 'running');
    await waitFor(() => {
      expect(screen.queryByText('run-1')).not.toBeInTheDocument();
      expect(screen.getByText('run-2')).toBeInTheDocument();
    });
  });

  test('Load more fetches the next page through the pagination cursor and appends rows', async () => {
    const user = userEvent.setup();
    const bodies = installListRuns((pageToken) =>
      pageToken === ''
        ? { runs: [run('run-1', 'succeeded')], nextPageToken: 'tok-2' }
        : { runs: [run('run-2', 'running')], nextPageToken: '' },
    );

    renderPage();
    expect(await screen.findByText('run-1')).toBeInTheDocument();
    expect(screen.queryByText('run-2')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Load more' }));

    expect(await screen.findByText('run-2')).toBeInTheDocument();
    expect(screen.getByText('run-1')).toBeInTheDocument();
    expect(bodies).toHaveLength(2);
    expect(bodyPageToken(bodies[0])).toBe('');
    expect(bodyPageToken(bodies[1])).toBe('tok-2');
  });

  test('Load more failure keeps the loaded rows and shows an inline error', async () => {
    const user = userEvent.setup();
    const bodies = installListRuns((pageToken) => {
      if (pageToken === '') return { runs: [run('run-1', 'succeeded')], nextPageToken: 'tok-2' };
      throw new Error('boom');
    });

    renderPage();
    expect(await screen.findByText('run-1')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Load more' }));

    expect(await screen.findByText('Failed to load more runs.')).toBeInTheDocument();
    expect(screen.getByText('run-1')).toBeInTheDocument();
    expect(bodies).toHaveLength(2);
  });

  test('hides Load more when the server reports no further page', async () => {
    installListRuns(() => ({ runs: [run('run-1', 'succeeded')], nextPageToken: '' }));

    renderPage();

    expect(await screen.findByText('run-1')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
  });

  test('duration column shows endedAt - startedAt for finished runs', async () => {
    installListRuns(() => ({
      runs: [
        run('run-1', 'succeeded', {
          ticket: 'CRI-187',
          started_at: '2026-02-05T08:30:00.000Z',
          ended_at: '2026-02-05T08:31:30.000Z',
        }),
        // No started_at: the duration column shows an em dash while the
        // started column falls back to created_at, so give the row a ticket
        // to keep the em dash unique.
        run('run-2', 'failed', { ticket: 'CRI-188' }),
      ],
      nextPageToken: '',
    }));

    renderPage();

    expect(await screen.findByText('1m 30s')).toBeInTheDocument();
    // No started_at means the duration is unknown.
    expect(screen.getByText('—')).toBeInTheDocument();
  });

  test('duration column shows a live elapsed time for running runs', async () => {
    const started = new Date('2026-02-05T08:31:00.000Z').getTime();
    vi.useFakeTimers();
    vi.setSystemTime(started + 30_000);
    installListRuns(() => ({
      runs: [run('run-1', 'running', { started_at: '2026-02-05T08:31:00.000Z' })],
      nextPageToken: '',
    }));

    renderPage();
    await settle();
    expect(screen.getByText('30s')).toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(31_000);
    });
    expect(screen.getByText('1m 01s')).toBeInTheDocument();
  });

  test('started column shows relative time with an absolute timestamp on hover', async () => {
    const now = new Date('2026-02-05T08:35:00.000Z').getTime();
    vi.useFakeTimers();
    vi.setSystemTime(now);
    installListRuns(() => ({
      runs: [run('run-1', 'succeeded', { created_at: '2026-02-05T08:30:00.000Z' })],
      nextPageToken: '',
    }));

    renderPage();
    await settle();

    expect(screen.getByText('5 minutes ago')).toBeInTheDocument();
    expect(screen.getByTitle(/2026/)).toBeInTheDocument();
  });

  test('polls on an interval while a run is non-terminal', async () => {
    vi.useFakeTimers();
    const bodies = installListRuns(() => ({
      runs: [run('run-1', 'running')],
      nextPageToken: '',
    }));

    renderPage();
    await settle();
    expect(bodies).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(13_000);
    });
    await settle();
    expect(bodies.length).toBeGreaterThanOrEqual(2);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(13_000);
    });
    await settle();
    expect(bodies.length).toBeGreaterThanOrEqual(3);
  });

  test('stops polling once every loaded run is terminal', async () => {
    vi.useFakeTimers();
    let serveRunning = true;
    const bodies = installListRuns(() => ({
      runs: [run('run-1', serveRunning ? 'running' : 'succeeded')],
      nextPageToken: '',
    }));

    renderPage();
    await settle();
    expect(bodies).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(13_000);
    });
    await settle();
    expect(bodies.length).toBeGreaterThanOrEqual(2);

    // From here on the server only reports terminal runs.
    serveRunning = false;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(13_000);
    });
    await settle();
    const settledCallCount = bodies.length;
    expect(settledCallCount).toBeGreaterThanOrEqual(3);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(40_000);
    });
    await settle();
    expect(bodies.length).toBe(settledCallCount);
  });

  test('does not poll while the tab is hidden', async () => {
    vi.useFakeTimers();
    const bodies = installListRuns(() => ({
      runs: [run('run-1', 'running')],
      nextPageToken: '',
    }));

    hideTab();
    renderPage();
    await settle();
    expect(bodies).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(40_000);
    });
    await settle();
    expect(bodies).toHaveLength(1);

    showTab();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_LIST_POLL_INTERVAL_MS + 1_000);
    });
    await settle();
    expect(bodies.length).toBeGreaterThan(1);
  });
});