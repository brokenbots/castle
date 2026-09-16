import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test, vi } from 'vitest';
import { RunListPage } from './RunListPage';
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
      try {
        const page = responder(pageToken, status);
        return HttpResponse.json({ runs: page.runs, next_page_token: page.nextPageToken });
      } catch {
        // Responders throw to simulate a failing request; surface it as a
        // connect-style HTTP error instead of a raw handler exception
        // (which MSW would log as an unhandled failure).
        return HttpResponse.json(
          { code: 'unavailable', message: 'simulated ListRuns failure' },
          { status: 503, headers: { 'content-type': 'application/json' } },
        );
      }
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

// Yields one REAL macrotask turn: undici delivers mocked socket I/O only when
// the event loop polls, and faked-timer advances interleave merely microtasks.
// MessageChannel is not faked, so its callback runs on the host event loop.
function yieldRealTask(): Promise<void> {
  return new Promise((resolve) => {
    const channel = new MessageChannel();
    channel.port1.onmessage = () => {
      channel.port1.close();
      resolve();
    };
    channel.port2.postMessage(0);
  });
}

// Redux ignores unknown actions, but notifying subscribers makes React-Redux
// re-check every selector inside unstable_batchedUpdates, which flushes any
// pending render synchronously. Store updates that complete inside a faked
// timer callback or between two act scopes are otherwise only applied at the
// next act exit, which the steps below cannot wait for.
const FLUSH_TEST_RENDER = { type: '__test/flush' };

// One bounded retry step under fake timers. Each step is its own act so
// React's passive effects (where RTK Query dispatches subscription updates,
// e.g. starting or stopping the poll timer) flush at the step boundary;
// the flush dispatches recover renders whose store update landed outside
// the previous act. A fixed fake-time budget cannot bound the real
// event-loop turns the mocked chain (MSW -> undici -> connect -> RTK Query)
// needs, hence the observable-condition loop and the single final assertion.
async function waitUntil(cond: () => boolean, what: string, advanceMs = 500): Promise<void> {
  for (let i = 0; i < 80 && !cond(); i += 1) {
    await act(async () => {
      store.dispatch(FLUSH_TEST_RENDER);
      if (advanceMs > 0) {
        await vi.advanceTimersByTimeAsync(advanceMs);
      }
      await yieldRealTask();
      store.dispatch(FLUSH_TEST_RENDER);
    });
  }
  expect(cond(), what).toBe(true);
}

// Proves a negative over a window longer than two poll intervals, advancing
// in per-step acts so a terminal poll response can land and its render plus
// polling teardown complete between steps before the next interval tick.
async function advanceQuietly(expectNoChange: () => number): Promise<number> {
  const before = expectNoChange();
  for (let i = 0; i < 6 && before === expectNoChange(); i += 1) {
    await act(async () => {
      store.dispatch(FLUSH_TEST_RENDER);
      await vi.advanceTimersByTimeAsync(7_000);
      await yieldRealTask();
      store.dispatch(FLUSH_TEST_RENDER);
    });
  }
  return expectNoChange();
}

// Whether the run row whose ID cell contains `id` shows `status`. Scoped to
// the row so the <option> labels of the status filter (which share their
// text with run statuses) cannot satisfy a queryByText assertion.
function rowHasStatus(id: string, status: string): boolean {
  const row = screen.queryByText(id)?.closest('tr');
  return row != null && within(row).queryByText(status) !== null;
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
    // Small steps keep the mocked clock near startedAt + 30s so the elapsed
    // label stays in the "30s" bucket regardless of how many event-loop turns
    // the mocked chain needs to land the response.
    await waitUntil(() => screen.queryByText('30s') !== null, 'initial elapsed 30s rendered', 100);
    expect(screen.getByText('30s')).toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(31_000);
      await yieldRealTask();
      store.dispatch(FLUSH_TEST_RENDER);
    });
    await waitUntil(() => screen.queryByText('1m 01s') !== null, 'elapsed advanced to 1m 01s', 0);
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
    // Small steps keep the mocked clock near 08:35 so the relative label
    // stays in the "5 minutes ago" bucket while the response lands.
    await waitUntil(
      () => screen.queryByText('5 minutes ago') !== null,
      'relative started label rendered',
      100,
    );

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
    await waitUntil(() => bodies.length >= 1, 'initial ListRuns request sent');
    expect(bodies).toHaveLength(1);

    await waitUntil(() => bodies.length >= 2, 'first poll request sent');
    expect(bodies.length).toBeGreaterThanOrEqual(2);

    await waitUntil(() => bodies.length >= 3, 'second poll request sent');
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
    await waitUntil(() => bodies.length >= 1, 'initial ListRuns request sent');
    await waitUntil(() => screen.queryByText('run-1') !== null, 'initial run rendered');
    expect(bodies).toHaveLength(1);

    // From here on the server only reports terminal runs.
    serveRunning = false;
    await waitUntil(() => bodies.length >= 2, 'poll request sent while running');
    expect(bodies.length).toBeGreaterThanOrEqual(2);
    // Prove the terminal response landed and was applied before asserting
    // that nothing else is requested.
    await waitUntil(() => rowHasStatus('run-1', 'succeeded'), 'terminal run status rendered');

    const settledCallCount = bodies.length;
    const afterQuietWindow = await advanceQuietly(() => bodies.length);
    expect(afterQuietWindow).toBe(settledCallCount);
    expect(bodies.length).toBe(settledCallCount);
  });

  // Regression: RTK Query polls re-initiate a cache entry with its stored
  // originalArgs, so after "Load more" overwrote those args with the cursor,
  // polling silently refetched the cursor page and page 1 went stale.
  test('polls refresh page 1 after Load more appends a cursor page', async () => {
    vi.useFakeTimers();
    let serveFresh = false;
    const bodies = installListRuns((pageToken) => {
      if (pageToken !== '') return { runs: [run('run-2', 'failed')], nextPageToken: '' };
      if (!serveFresh) return { runs: [run('run-1', 'running')], nextPageToken: 'tok-2' };
      return {
        runs: [run('run-1', 'succeeded'), run('run-2', 'cancelled')],
        nextPageToken: '',
      };
    });

    renderPage();
    await waitUntil(() => bodies.length >= 1, 'initial ListRuns request sent');
    await waitUntil(() => screen.queryByText('run-1') !== null, 'page-1 run rendered');

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
    });
    await waitUntil(
      () => bodies.some((body) => bodyPageToken(body) === 'tok-2'),
      'cursor page requested',
    );
    await waitUntil(() => screen.queryByText('run-2') !== null, 'cursor row rendered');

    serveFresh = true;
    await waitUntil(
      () =>
        rowHasStatus('run-1', 'succeeded') &&
        bodyPageToken(bodies[bodies.length - 1]) === '',
      'page-1 refreshed by a poll after Load more',
    );

    expect(bodyPageToken(bodies[bodies.length - 1])).toBe('');
    expect(bodyStatus(bodies[bodies.length - 1])).toBe('');
    // The appended cursor row is still listed once, its stale copy replaced
    // by the fresh page-1 data.
    expect(screen.getAllByText('run-2')).toHaveLength(1);
    // Status-text assertions are scoped to the row: the status filter's
    // <option> labels share their text with run statuses.
    const run1Row = screen.getByText('run-1').closest('tr');
    expect(run1Row).not.toBeNull();
    expect(within(run1Row!).queryByText('running')).not.toBeInTheDocument();
    const run2Row = screen.getByText('run-2').closest('tr');
    expect(run2Row).not.toBeNull();
    expect(within(run2Row!).queryByText('failed')).not.toBeInTheDocument();
    expect(within(run2Row!).getByText('cancelled')).toBeInTheDocument();
  });

  test('shows an indicator when a background refresh fails', async () => {
    vi.useFakeTimers();
    let fail = false;
    const bodies = installListRuns(() => {
      if (fail) throw new Error('boom');
      return { runs: [run('run-1', 'running')], nextPageToken: '' };
    });

    renderPage();
    await waitUntil(() => bodies.length >= 1, 'initial ListRuns request sent');
    await waitUntil(() => screen.queryByText('run-1') !== null, 'initial run rendered');

    fail = true;
    await waitUntil(() => bodies.length >= 2, 'poll request sent');
    await waitUntil(
      () => screen.queryByText('Refresh failed. Showing the last loaded runs.') !== null,
      'refresh-failure indicator rendered',
    );
    expect(screen.getByText('Refresh failed. Showing the last loaded runs.')).toBeInTheDocument();
    expect(screen.getByText('run-1')).toBeInTheDocument();
  });

  test('does not poll while the tab is hidden', async () => {
    vi.useFakeTimers();
    const bodies = installListRuns(() => ({
      runs: [run('run-1', 'running')],
      nextPageToken: '',
    }));

    hideTab();
    renderPage();
    await waitUntil(() => bodies.length >= 1, 'initial ListRuns request sent');
    await waitUntil(() => screen.queryByText('run-1') !== null, 'initial run rendered');

    const hiddenCount = bodies.length;
    expect(hiddenCount).toBeGreaterThanOrEqual(1);
    const afterQuietWindow = await advanceQuietly(() => bodies.length);
    expect(afterQuietWindow).toBe(hiddenCount);
    expect(bodies.length).toBe(hiddenCount);

    await act(async () => {
      store.dispatch(FLUSH_TEST_RENDER);
      showTab();
    });
    await waitUntil(
      () => bodies.length > hiddenCount,
      'poll request sent after becoming visible',
    );
    expect(bodies.length).toBeGreaterThan(hiddenCount);
  });
});