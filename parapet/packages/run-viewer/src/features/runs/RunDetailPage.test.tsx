import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import {
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  test,
  vi,
} from 'vitest';
import { RunDetailPage } from './RunDetailPage';
import { NO_CONTROLS_TOOLTIP, NO_CONTROL_CAPABILITIES, type RunCapabilities } from './capabilities';
import { createRunViewerStore } from '../../store';
import { selectRunEvents, runsSlice } from './runsSlice';
import { server } from '../../test/mocks/server';
import { serverPath } from '../../test/mocks/handlers';
import wirePayload from './workflowGraph/fixtures/workflow_graphs_fd98126e.json';

vi.mock('./watchRun', () => ({
  startWatch: vi.fn().mockResolvedValue(undefined),
}));

import { startWatch } from './watchRun';

// One store instance per test file; RTK Query caches per store.
const store = createRunViewerStore();


// jsdom has no layout; react-virtual (via EventLog) reads offsetHeight and
// renders nothing when it measures 0. measureElement additionally reads
// getBoundingClientRect, so model the browser box contract faithfully: a
// committed inline height pins the border box (CSS: height wins over
// content); otherwise the box fits the content — payload text wraps at the
// container width (~104 12px-monospace chars at this 800px box; 16px per
// line + 9px row padding). A stub contradicting an element's committed
// height would let row-measurement tests pass on the row-pinning behaviour
// they exist to catch.
const PAYLOAD_CHARS_PER_LINE = 104;
const ROW_LINE_PX = 16;
const ROW_BASE_PX = 9;

beforeAll(() => {
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(800);
  // Tail logic reads the scroll metrics to detect "at bottom"; model the
  // scroller as a 600px viewport over its committed inner height.
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockImplementation(function (this: HTMLElement) {
    return this.getAttribute('data-testid') === 'event-log-scroll' ? 600 : 0;
  });
  vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockImplementation(function (this: HTMLElement) {
    if (this.getAttribute('data-testid') !== 'event-log-scroll') return 0;
    const inner = this.firstElementChild;
    return inner ? inner.getBoundingClientRect().height : 0;
  });
  vi.spyOn(
    HTMLElement.prototype,
    'getBoundingClientRect',
  ).mockImplementation(function (this: HTMLElement) {
    const pinned = Number.parseFloat(this.style.height);
    const textLength = this.textContent?.length ?? 0;
    const wrappedLines = Math.max(
      1,
      Math.ceil(textLength / PAYLOAD_CHARS_PER_LINE),
    );
    const height = Number.isNaN(pinned)
      ? ROW_BASE_PX + wrappedLines * ROW_LINE_PX
      : pinned;
    return {
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      right: 800,
      bottom: height,
      width: 800,
      height,
      toJSON: () => ({}),
    };
  });
});

// The drill-down tests vary the run's workflow source (a subworkflow
// declaration + a step targeting it); every test resets to this default
// chain so mutations never leak (CRI-257).
const DEFAULT_WORKFLOW_SOURCE =
  'workflow {\n  name = "hello"\n  initial_state = "build"\n}\nstep "build" {\n  outcome "success" { next = step.test }\n}\nstep "test" {\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';

// Mutable fixture so tests can vary run metadata (CRI-131) without a second
// module mock. UseGetRunQuery returns this object verbatim.
const fixture = vi.hoisted(() => ({
  error: undefined as unknown,
  refetch: undefined as unknown,
  data: {
    runId: 'run-1',
    criteriaId: 'ov-1',
    workflowName: 'hello',
    // Real criteria dialect: executable nodes are top-level blocks and
    // outcomes route via `next = <traversal>` (the workflowHash field
    // carries the full workflow source). beforeEach resets this to
    // DEFAULT_WORKFLOW_SOURCE, the single source of truth below.
    workflowHash: '',
    status: 'running',
    createdAt: new Date().toISOString(),
    finalState: '',
    failureReason: '',
  } as Record<string, unknown>,
}));

vi.mock('../../api/castleApi', async () => {
  const actual = await vi.importActual<typeof import('../../api/castleApi')>(
    '../../api/castleApi',
  );
  return {
    ...actual,
    useGetRunQuery: () => ({
      isLoading: false,
      error: fixture.error,
      // A real failure leaves the query without data, exactly as the page's
      // error branch (`run.error || !run.data`) expects.
      data: fixture.error ? undefined : fixture.data,
      // The stream's terminal-event refetch hook calls this; tests spy it.
      refetch: fixture.refetch ?? (() => {}),
    }),
  };
});

// Wire shape for ListRunEvents events: protojson flattens the payload
// oneof, so the case name is a top-level key.
function wireEvents(count: number) {
  return Array.from({ length: count }, (_, i) => ({
    schemaVersion: 1,
    runId: 'run-1',
    seq: String(i + 1),
    ts: new Date(0).toISOString(),
    correlationId: '',
    stepLog: { chunk: `chunk ${i + 1}` },
  }));
}

// A run.metadata envelope as the ListRunEvents wire delivers it (protojson
// flattens the payload oneof to its case name; message fields stay snake_case).
function wireMetadataEvent(seq: number, meta: { ticket?: string; repo_url?: string; pr_url?: string }) {
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq: String(seq),
    ts: new Date(0).toISOString(),
    correlationId: '',
    runMetadata: meta,
  };
}

describe('RunDetailPage', () => {
  beforeEach(() => {
    fixture.error = undefined;
    fixture.refetch = undefined;
    // Live-tail affordances key off run status; make the shared fixture's
    // status explicit so tests that change it don't leak.
    fixture.data.status = 'running';
    fixture.data.workflowHash = DEFAULT_WORKFLOW_SOURCE;
    // CRI-284: clear any subscriber id an earlier test persisted, so the
    // insecure-origin render actually reaches the id generator instead of
    // silently reusing a sessionStorage-cached id.
    sessionStorage.clear();
  });

  test('starts WatchRun with sinceSeq=0 and subscriberId', async () => {
    // CRI-284: the subscriber id is built from crypto.getRandomValues, not
    // the secure-context-only crypto.randomUUID. A constant byte stream makes
    // the id deterministic: all-0xaa stamps to aaaaaaaa-aaaa-4aaa-8aaa-…
    const getRandomValues = vi
      .spyOn(crypto, 'getRandomValues')
      .mockImplementation((array) => {
        new Uint8Array(array.buffer, array.byteOffset, array.byteLength).fill(0xaa);
        return array;
      });

    try {
      render(
        <Provider store={store}>
          <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
            <Routes>
              <Route path="/runs/:id" element={<RunDetailPage />} />
            </Routes>
          </MemoryRouter>
        </Provider>,
      );

      expect(await screen.findByText('Workflow source')).toBeInTheDocument();
      // The watch starts once the event log is anchored at the newest page,
      // which resolves after the initial ListRunEvents walk.
      await vi.waitFor(() => expect(startWatch).toHaveBeenCalled());

      const firstCall = vi.mocked(startWatch).mock.calls[0];
      expect(firstCall[0]).toBe('run-1');
      expect(firstCall[1]).toBe(0);
      expect(firstCall[2]).toBe('aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa');
    } finally {
      getRandomValues.mockRestore();
    }
  });

  test('renders without crypto.randomUUID (insecure origin, CRI-284)', async () => {
    // Simulate a plain-HTTP ingress: secure-context-only randomUUID is
    // absent. The page used to throw "crypto.randomUUID is not a function"
    // during render and take the whole route down.
    const original = crypto.randomUUID;
    Object.defineProperty(crypto, 'randomUUID', { value: undefined, configurable: true });
    try {
      render(
        <Provider store={store}>
          <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
            <Routes>
              <Route path="/runs/:id" element={<RunDetailPage />} />
            </Routes>
          </MemoryRouter>
        </Provider>,
      );

      // The page renders and the watch still starts with a well-formed id.
      expect(await screen.findByText('Workflow source')).toBeInTheDocument();
      await vi.waitFor(() => expect(startWatch).toHaveBeenCalled());
      const subscriberId = vi.mocked(startWatch).mock.calls.at(-1)![2];
      expect(subscriberId).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
      );
    } finally {
      Object.defineProperty(crypto, 'randomUUID', { value: original, configurable: true });
    }
  });

  test('renders workflow source and graph', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    expect(await screen.findByText('Step graph')).toBeInTheDocument();
    expect((await screen.findAllByText(/build/)).length).toBeGreaterThan(0);
  });

  test('renders ticket, repo and PR link from run.metadata events', async () => {
    // CRI-131 re-home (KB-102): identity metadata is published as run.metadata
    // envelopes, not wire fields on the run row. The header derives it from
    // the event log with last-wins/non-empty promotion.
    server.use(
      http.post(serverPath('ListRunEvents'), () =>
        HttpResponse.json({
          events: [
            wireMetadataEvent(1, { ticket: 'CRI-131', repo_url: 'brokenbots/castle' }),
            wireMetadataEvent(2, { pr_url: 'https://github.com/brokenbots/castle/pull/42' }),
          ],
          last_seq: '2',
        }),
      ),
    );

    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    // Collapsed event summaries now surface metadata keys too; scope the
    // assertions to the static header, which owns the identity metadata.
    const header = await screen.findByTestId('run-detail-header');
    expect(await within(header).findByText('CRI-131')).toBeInTheDocument();
    expect(await within(header).findByText('brokenbots/castle')).toBeInTheDocument();
    const link = screen.getByRole('link', { name: 'PR' });
    expect(link.getAttribute('href')).toBe('https://github.com/brokenbots/castle/pull/42');
    expect(link.getAttribute('rel')).toBe('noreferrer');
  });

  test('does not render PR link for non-http metadata values', async () => {
    server.use(
      http.post(serverPath('ListRunEvents'), () =>
        HttpResponse.json({
          events: [
            wireMetadataEvent(1, { ticket: 'CRI-131', pr_url: 'javascript:alert(1)' }),
          ],
          last_seq: '1',
        }),
      ),
    );

    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    // Scope to the header: the event log's collapsed summaries also show
    // the ticket value now.
    const header = await screen.findByTestId('run-detail-header');
    expect(await within(header).findByText('CRI-131')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'PR' })).not.toBeInTheDocument();
  });

  test('supports both proto and json codec selection', async () => {
    const { getRuntimeCodec } = await import('../../api/client');

    // Test default codec (json)
    expect(getRuntimeCodec()).toBe('json');

    // Test window.__CRITERIA__.codec override (proto)
    window.__CRITERIA__ = { codec: 'proto' };
    expect(getRuntimeCodec()).toBe('proto');

    // Test meta tag fallback (json)
    window.__CRITERIA__ = undefined;
    const meta = document.createElement('meta');
    meta.name = 'criteria-codec';
    meta.content = 'json';
    document.head.appendChild(meta);
    expect(getRuntimeCodec()).toBe('json');

    // Cleanup
    document.head.removeChild(meta);
  });

  // The 1000-event walk plus virtualization is the heaviest integration
  // test here; the vitest 5s default only holds on an idle machine, so
  // these get explicit headroom for full-suite parallel runs.
  test('anchors at the newest page and lazy-loads older events', { timeout: 30_000 }, async () => {
    // A completed run: the live-tail affordances stay off so the log window
    // opens at the top of the loaded list.
    fixture.data.status = 'completed';
    // A 1000-event run served page-by-page from a stateful MSW handler.
    // The initial walk seeds every fetched page into the store (no silent
    // truncation); the anchor still sits at the newest page and older
    // history stays reachable through the "Load earlier events" control.
    // Wire shape: connect-web serializes request fields lowerCamelCase.
    const all = wireEvents(1000);
    const seen: string[] = [];
    server.use(
      http.post(serverPath('ListRunEvents'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as {
          run_id?: string;
          since_seq?: string;
          sinceSeq?: string;
          limit?: number;
        };
        const since = Number(body.sinceSeq ?? body.since_seq ?? '0');
        const limit = Number(body.limit ?? 500);
        seen.push(String(since));
        const events = all
          .filter((e) => Number(e.seq) > since)
          .slice(0, limit);
        const resp: Record<string, unknown> = {
          events,
          last_seq: events.length ? events[events.length - 1].seq : '0',
        };
        // Full page mid-history: continuation to the next (newer) page.
        if (events.length === limit && since + limit < 1000) {
          resp.next_since_seq = events[events.length - 1].seq;
        }
        return HttpResponse.json(resp);
      }),
    );

    render(
      <Provider store={store}>
        <MemoryRouter
          initialEntries={['/runs/run-1']}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    // The walk seeds the full history, so the oldest chunk renders
    // (windowed) immediately; the mid-run chunk 501 is off-window until
    // scrolled to.
    expect(await screen.findByText('chunk 1')).toBeInTheDocument();
    expect(screen.queryByText('chunk 501')).not.toBeInTheDocument();

    // Watch anchored at the newest seq (no full-history replay), after the
    // anchor walk resolves. Mock calls accumulate across tests in this file,
    // so inspect the last call.
    await vi.waitFor(() => expect(startWatch).toHaveBeenCalled());
    const lastWatchCall = vi.mocked(startWatch).mock.calls.at(-1);
    expect(lastWatchCall?.[0]).toBe('run-1');
    expect(lastWatchCall?.[1]).toBe(1000);

    // The store holds every walked event (seq 1..1000), not just the tail.
    expect(selectRunEvents('run-1')(store.getState()).map((e) => e.seq)).toEqual(
      Array.from({ length: 1000 }, (_, i) => i + 1),
    );

    // Only a window of the 1000 walked events is in the DOM (25px rows:
    // ~24 visible + 12 overscan).
    expect(
      screen.getAllByTestId('event-log-row').length,
    ).toBeLessThan(50);

    const loadEarlier = await screen.findByRole('button', {
      name: 'Load earlier events',
    });
    await userEvent.click(loadEarlier);

    // The earlier page merged in order with no duplicates or gaps.
    await vi.waitFor(() => {
      expect(selectRunEvents('run-1')(store.getState()).map((e) => e.seq)).toEqual(
        Array.from({ length: 1000 }, (_, i) => i + 1),
      );
    });
    await vi.waitFor(() =>
      expect(
        screen.queryByRole('button', { name: 'Load earlier events' }),
      ).not.toBeInTheDocument(),
    );

    // The load-earlier fetch is entirely duplicates (the walk already
    // seeded the full history), so nothing is prepended and the reader
    // does not move.
    const scroller = screen.getByTestId('event-log-scroll');
    expect(scroller.scrollTop).toBe(0);

    // Even with all 1000 events loaded, only a window is in the DOM
    // (25px rows: ~24 visible + 12 overscan).
    expect(screen.getAllByTestId('event-log-row').length).toBeLessThan(50);

    // Walk probes (since 0, continuation at 500) + the load-earlier seek
    // back to since 0.
    expect(seen).toEqual(['0', '500', '0']);
  });

  test('degrades to a watch replay from 0 when the anchor walk fails', async () => {
    // ListRunEvents errors even though GetRun serves a run: the log must
    // fall back to the pre-pagination behaviour — the watch starts at
    // sinceSeq 0, no pagination control is offered, and the failure is
    // surfaced. Prior tests unmount their page (clearing run-1 events),
    // so the store must stay empty.
    server.use(
      http.post(serverPath('ListRunEvents'), () => HttpResponse.error()),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.mocked(startWatch).mockClear();

    render(
      <Provider store={store}>
        <MemoryRouter
          initialEntries={['/runs/run-1']}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    await vi.waitFor(() => expect(startWatch).toHaveBeenCalled());
    const watchCall = vi.mocked(startWatch).mock.calls.at(-1);
    expect(watchCall?.[0]).toBe('run-1');
    // Degraded anchor: replay from the very beginning.
    expect(watchCall?.[1]).toBe(0);

    // The failed walk dispatched nothing into the store.
    expect(selectRunEvents('run-1')(store.getState())).toEqual([]);

    // No pagination control in the never-anchored state.
    expect(
      screen.queryByRole('button', { name: 'Load earlier events' }),
    ).not.toBeInTheDocument();

    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('listRunEvents failed for run run-1'),
      expect.anything(),
    );
    warn.mockRestore();
  });

  test('keeps the load-earlier control retryable when the older-page fetch fails', { timeout: 30_000 }, async () => {
    // The anchor walk succeeds (two pages), but every load-earlier request
    // errors: the control must come back enabled so the failure is
    // retryable, and the failed fetch must not touch the store.
    let sinceZeroCalls = 0;
    server.use(
      http.post(serverPath('ListRunEvents'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as {
          run_id?: string;
          sinceSeq?: string;
          since_seq?: string;
          limit?: number;
        };
        const since = Number(body.sinceSeq ?? body.since_seq ?? '0');
        const limit = Number(body.limit ?? 500);
        // The walk's first probe is the only successful since-0 call;
        // load-earlier seeks back to 0 and fails here.
        if (since === 0 && ++sinceZeroCalls > 1) {
          return HttpResponse.error();
        }
        const events = wireEvents(1000)
          .filter((e) => Number(e.seq) > since)
          .slice(0, limit);
        const resp: Record<string, unknown> = {
          events,
          last_seq: events.length ? events[events.length - 1].seq : '0',
        };
        if (events.length === limit && since + limit < 1000) {
          resp.next_since_seq = events[events.length - 1].seq;
        }
        return HttpResponse.json(resp);
      }),
    );
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    render(
      <Provider store={store}>
        <MemoryRouter
          initialEntries={['/runs/run-1']}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    await vi.waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Load earlier events' }),
      ).toBeEnabled(),
    );

    await userEvent.click(
      screen.getByRole('button', { name: 'Load earlier events' }),
    );
    await vi.waitFor(() =>
      expect(
        screen.queryByRole('button', { name: 'Loading earlier events…' }),
      ).not.toBeInTheDocument(),
    );

    // The failure is surfaced and the control is offered again, enabled.
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('loading earlier events failed for run run-1'),
      expect.anything(),
    );
    expect(
      screen.getByRole('button', { name: 'Load earlier events' }),
    ).toBeEnabled();

    // Still retryable on a second attempt.
    await userEvent.click(
      screen.getByRole('button', { name: 'Load earlier events' }),
    );
    // RTK's serializability middleware warns on slow dispatches under load;
    // count only the load-earlier failures the test is about.
    const loadEarlierWarns = () =>
      warn.mock.calls.filter(([msg]) => String(msg).includes('loading earlier events failed for run run-1'))
        .length;
    await vi.waitFor(() => expect(loadEarlierWarns()).toBe(2));
    expect(
      screen.getByRole('button', { name: 'Load earlier events' }),
    ).toBeEnabled();

    // The failed fetches dispatched nothing: the store still holds exactly
    // the walked history.
    expect(
      selectRunEvents('run-1')(store.getState()).map((e) => e.seq),
    ).toEqual(Array.from({ length: 1000 }, (_, i) => i + 1));
    warn.mockRestore();
  });

  test('jumps to the bottom when a running run loads with history', async () => {
    // The page loads an already-running run with existing history: the log
    // must end up pinned at the bottom (live tail), not at the top.
    server.use(
      http.post(serverPath('ListRunEvents'), async () => {
        const events = wireEvents(30);
        return HttpResponse.json({ events, last_seq: '30' });
      }),
    );

    render(
      <Provider store={store}>
        <MemoryRouter
          initialEntries={['/runs/run-1']}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    await vi.waitFor(() =>
      expect(selectRunEvents('run-1')(store.getState())).toHaveLength(30),
    );
    const scroller = screen.getByTestId('event-log-scroll');
    await vi.waitFor(() => expect(scroller.scrollTop).toBeGreaterThan(0));
    // Pinned at the bottom: no jump affordance.
    expect(screen.queryByTestId('jump-to-latest')).not.toBeInTheDocument();

    // The newest chunk is actually in view once the window follows the pin.
    act(() => {
      scroller.dispatchEvent(new Event('scroll'));
    });
    expect(await screen.findByText('chunk 30')).toBeInTheDocument();
  });

  test('does not live-tail a running-status run whose log holds a terminal event', async () => {
    // The run status can lag the event stream: a 'running' run whose log
    // already ends with a terminal event is finished, so the view must not
    // jump to the bottom.
    fixture.data.status = 'running';
    server.use(
      http.post(serverPath('ListRunEvents'), async () => {
        const events = [
          ...wireEvents(30),
          {
            schemaVersion: 1,
            runId: 'run-1',
            seq: '31',
            ts: new Date(0).toISOString(),
            correlationId: '',
            runCompleted: {},
          },
        ];
        return HttpResponse.json({ events, last_seq: '31' });
      }),
    );

    render(
      <Provider store={store}>
        <MemoryRouter
          initialEntries={['/runs/run-1']}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    await vi.waitFor(() =>
      expect(selectRunEvents('run-1')(store.getState())).toHaveLength(31),
    );
    const scroller = screen.getByTestId('event-log-scroll');
    // The terminal event disables live tailing despite the 'running' status:
    // no pin, no jump, and the log opens at its anchor (newest page, oldest
    // windowed chunk on top) with scrollTop still at 0.
    expect(scroller.scrollTop).toBe(0);
    expect(screen.getByText('chunk 2')).toBeInTheDocument();
    expect(screen.getByText('chunk 30')).toBeInTheDocument();
    expect(screen.queryByTestId('jump-to-latest')).not.toBeInTheDocument();
  });

  test('falls back to the text-edge list when the workflow source does not parse', async () => {
    // A stray token inside the transitions map makes the HCL parser reject
    // the source, while the legacy regex still finds the step blocks — the
    // panel degrades to the text-edge list instead of blanking.
    const originalSource = fixture.data.workflowHash;
    fixture.data.workflowHash =
      'workflow "hello" {\n  start_at = "build"\n  step "build" {\n    transitions = {\n      "success" = "test" oops\n}\n  step "test" {\n    transitions = {\n      "success" = "done"\n}\n  state "done" { terminal = true }\n}';

    try {
      render(
        <Provider store={store}>
          <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
            <Routes>
              <Route path="/runs/:id" element={<RunDetailPage />} />
            </Routes>
          </MemoryRouter>
        </Provider>,
      );

      expect(await screen.findByText('Step graph')).toBeInTheDocument();
      // No graph is rendered; the text-edge fallback keeps the panel populated.
      expect(document.querySelector('[data-testid="workflow-graph"]')).toBeNull();
      const edgeRows = document.querySelectorAll('[data-testid="graph-fallback-edges"] > div');
      expect(edgeRows).toHaveLength(2);
      expect(edgeRows[0].textContent).toBe('build --success--> test');
      expect(edgeRows[1].textContent).toBe('test --success--> done');
    } finally {
      fixture.data.workflowHash = originalSource;
    }
  });

  test('falls back to the empty text-edge notice on malformed real-dialect source', async () => {
    // A real-shaped source truncated mid-block: the parser rejects the
    // unclosed `step` block, and the regex fallback finds no `transitions`
    // maps in the current dialect — the panel renders the empty-state
    // notice instead of blanking.
    const originalSource = fixture.data.workflowHash;
    fixture.data.workflowHash =
      'workflow {\n  name = "hello"\n  initial_state = "build"\n}\nstep "build" {\n  outcome "success" { next = step.test }\n';

    try {
      render(
        <Provider store={store}>
          <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
            <Routes>
              <Route path="/runs/:id" element={<RunDetailPage />} />
            </Routes>
          </MemoryRouter>
        </Provider>,
      );

      expect(await screen.findByText('Step graph')).toBeInTheDocument();
      expect(screen.getByText('No step transitions found.')).toBeInTheDocument();
      expect(document.querySelector('[data-testid="workflow-graph"]')).toBeNull();
    } finally {
      fixture.data.workflowHash = originalSource;
    }
  });

  test('highlights the active step from the event stream and dims unvisited nodes', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    await vi.waitFor(() =>
      expect(document.querySelectorAll('[data-testid="graph-node"]')).toHaveLength(3),
    );
    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 101,
          type: 'stepEntered',
          correlationId: '',
          payload: { step: 'build' },
        }),
      );
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 102,
          type: 'stepOutcome',
          correlationId: '',
          payload: { step: 'test', outcome: 'success' },
        }),
      );
    });

    // The entered step pulses, the completed one shows success, and the
    // unvisited terminal node stays dimmed.
    const buildCard = document.querySelector('[data-node-id="build"]');
    const testCard = document.querySelector('[data-node-id="test"]');
    const doneCard = document.querySelector('[data-node-id="done"]');
    expect(buildCard?.className).toContain('animate-pulse');
    expect(testCard?.className).toContain('border-success/70');
    // The unvisited terminal node dims via its content, not the card (the
    // fill stays opaque so no edge bleeds through).
    expect(doneCard?.querySelector('p')?.className).toContain('opacity-60');
    expect(doneCard?.className).not.toContain('opacity-60');
    expect(screen.getByLabelText('status running')).toBeInTheDocument();
    expect(screen.getByLabelText('status succeeded')).toBeInTheDocument();
  });

  test('clicking a node filters the log to that step and the filter can clear', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    await vi.waitFor(() =>
      expect(document.querySelectorAll('[data-testid="graph-node"]')).toHaveLength(3),
    );
    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 201,
          type: 'stepOutcome',
          correlationId: '',
          payload: { step: 'build', outcome: 'success' },
        }),
      );
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 202,
          type: 'stepOutcome',
          correlationId: '',
          payload: { step: 'test', outcome: 'success' },
        }),
      );
    });
    // The typed summaries render the step/outcome parts of both outcomes.
    expect(screen.getByText('#201')).toBeInTheDocument();
    expect(screen.getByText('#202')).toBeInTheDocument();

    act(() => {
      fireEvent.click(document.querySelector('[data-node-id="test"]')!);
    });
    // The chip names the step and the log shows only that step's events.
    expect(screen.getByTestId('step-filter')).toHaveTextContent('Filtered to step: test');
    expect(screen.queryByText('#201')).not.toBeInTheDocument();
    expect(screen.getByText('#202')).toBeInTheDocument();
    // The selected node is highlighted in the graph.
    expect(document.querySelector('[data-node-id="test"]')?.className).toContain('ring-2');

    fireEvent.click(screen.getByTestId('clear-step-filter'));
    expect(screen.queryByTestId('step-filter')).not.toBeInTheDocument();
    expect(screen.getByText('#201')).toBeInTheDocument();

    // Clicking a node re-applies the filter; clicking the already-selected
    // node toggles it back off.
    act(() => {
      fireEvent.click(document.querySelector('[data-node-id="test"]')!);
    });
    expect(screen.getByTestId('step-filter')).toHaveTextContent('Filtered to step: test');
    act(() => {
      fireEvent.click(document.querySelector('[data-node-id="test"]')!);
    });
    expect(screen.queryByTestId('step-filter')).not.toBeInTheDocument();
    expect(screen.getByText('#201')).toBeInTheDocument();
  });

  test('clicking a node highlights its exact declaration in the workflow source', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    await vi.waitFor(() =>
      expect(document.querySelectorAll('[data-testid="graph-node"]')).toHaveLength(3),
    );

    const sourceView = screen.getByTestId('workflow-source-view');
    expect(sourceView.textContent).toContain('step "build"');

    // No highlight before a node is clicked.
    expect(screen.queryByTestId('workflow-source-highlight')).not.toBeInTheDocument();

    act(() => {
      fireEvent.click(document.querySelector('[data-node-id="test"]')!);
    });

    // The highlight slices back to exactly the `step "test" { … }` block of
    // the fixture source — ranges recorded at parse time, not re-scanned.
    const highlight = screen.getByTestId('workflow-source-highlight');
    expect(highlight.textContent).toContain('step "test"');
    expect(highlight.textContent?.startsWith('step "test"')).toBe(true);
    expect(highlight.textContent?.endsWith('}')).toBe(true);
    expect(highlight.textContent).not.toContain('step "build"');
    expect(highlight.textContent).not.toContain('state "done"');
    // The highlighted block is nested inside the full source view.
    expect(sourceView).toContainElement(highlight);

    // Deselecting the node removes the highlight again.
    act(() => {
      fireEvent.click(document.querySelector('[data-node-id="test"]')!);
    });
    expect(screen.queryByTestId('workflow-source-highlight')).not.toBeInTheDocument();
  });

  test('renders the Inspection section from the InspectRun response', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    // Values come from the default InspectRun MSW handler; the blank
    // adapter state degrades to the empty notice instead of a viewer.
    // The full panel lives in the right drawer; the static header carries
    // the compact summary of the same InspectRun data.
    const drawer = await screen.findByTestId('run-detail-drawer');
    expect(within(drawer).getByText('Inspection details')).toBeInTheDocument();
    expect(within(drawer).getByTestId('inspection-current-step')).toHaveTextContent('build');
    expect(within(drawer).getByText('local')).toBeInTheDocument();
    expect(within(drawer).getByText('sess-1')).toBeInTheDocument();
    expect(screen.getByTestId('inspection-summary-step')).toHaveTextContent('build');
    expect(screen.getByTestId('adapter-state-empty')).toBeInTheDocument();
  });

  test('shows the gathered scope & controls panel inside the page layout', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByTestId('scope-controls-panel')).toBeInTheDocument();

    const header = screen.getByTestId('run-detail-header');
    const panel = screen.getByTestId('scope-controls-panel');
    // The control buttons moved into the sticky page header; the drawer's
    // panel renders scope only (no duplicate control row).
    expect(within(header).getByRole('button', { name: 'Pause' })).toBeInTheDocument();
    expect(within(panel).queryByTestId('scope-controls-row')).not.toBeInTheDocument();
    // The gathered drawer content: the run scope view, and no duplicated
    // scope content or event log inside the panel.
    expect(within(panel).getByTestId('run-scope-panel')).toBeInTheDocument();
    expect(within(panel).queryByTestId('event-log-scroll')).not.toBeInTheDocument();
    expect(screen.getByTestId('run-detail-drawer')).toContainElement(panel);
    expect(within(screen.getByTestId('run-detail-layout')).getByTestId('event-log-scroll')).toBeInTheDocument();
  });

  test('collapses the scope & controls panel and restores its content on expand', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    const drawer = await screen.findByTestId('run-detail-drawer');
    const toggle = within(drawer).getByTestId('scope-controls-collapse');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(within(drawer).getByTestId('run-scope-panel')).toBeInTheDocument();

    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    // Collapsed hides the body but keeps the bar with its affordance.
    expect(within(drawer).queryByTestId('scope-controls-body')).not.toBeInTheDocument();
    expect(within(drawer).getByTestId('scope-controls-collapsed')).toBeInTheDocument();

    await userEvent.click(toggle);
    expect(within(drawer).getByTestId('scope-controls-body')).toBeInTheDocument();
    expect(within(drawer).getByTestId('run-scope-panel')).toBeInTheDocument();
  });
});

describe('RunDetailPage navigation chrome', () => {
  function renderDetail() {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );
  }

  test('shows breadcrumbs with a Runs link and the workflow name as the current page', async () => {
    renderDetail();

    expect(await screen.findByTestId('breadcrumbs')).toBeInTheDocument();
    const breadcrumbs = screen.getByTestId('breadcrumbs');
    const runsCrumb = within(breadcrumbs).getByRole('link', { name: 'Runs' });
    expect(runsCrumb.getAttribute('href')).toBe('/runs');
    const current = within(breadcrumbs).getByText('hello');
    expect(current.getAttribute('aria-current')).toBe('page');
  });

  test('offers a back affordance to the run list', async () => {
    renderDetail();

    const back = await screen.findByTestId('run-back');
    expect(back.getAttribute('href')).toBe('/runs');
  });

  test('the document title reflects the current workflow name', async () => {
    renderDetail();

    await vi.waitFor(() =>
      expect(document.title).toBe('hello — Parapet — Castle'),
    );
  });
});

describe('RunDetailPage stream status', () => {
  afterEach(() => {
    store.dispatch(runsSlice.actions.runCleared('run-1'));
    vi.mocked(startWatch).mockClear();
  });

  async function renderDetail() {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );
    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
  }

  test('shows the reconnecting banner with bounded-backoff progress', async () => {
    store.dispatch(
      runsSlice.actions.watchStatusChanged({
        runId: 'run-1',
        status: { state: 'reconnecting', attempt: 2, maxAttempts: 5 },
      }),
    );

    await renderDetail();

    const banner = screen.getByTestId('stream-status');
    expect(banner).toHaveAttribute('data-state', 'reconnecting');
    expect(screen.getByTestId('stream-reconnecting')).toHaveTextContent('attempt 2/5');
    expect(screen.queryByTestId('stream-reconnect')).not.toBeInTheDocument();
  });

  test('re-fetches the run row once when the stream delivers the terminal event', async () => {
    // The header/summary bind to the GetRun cache, which fetches once per
    // mount: a run that fails while the page is open would keep reading
    // running until a manual refresh. The terminal event must trigger one
    // re-fetch (and only one — resend storms must not hammer Castle).
    const refetch = vi.fn();
    fixture.refetch = refetch;
    fixture.data.status = 'running';

    await renderDetail();
    expect(refetch).not.toHaveBeenCalled();

    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 900,
          type: 'runCompleted',
          correlationId: '',
          payload: { finalState: 'done' },
        }),
      );
    });
    expect(refetch).toHaveBeenCalledTimes(1);

    // A resent terminal event (or any further event after the end) does
    // not re-trigger.
    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 901,
          type: 'runCompleted',
          correlationId: '',
          payload: { finalState: 'done' },
        }),
      );
    });
    expect(refetch).toHaveBeenCalledTimes(1);
  });

  test('offers a manual reconnect that resumes from the newest delivered seq', async () => {
    store.dispatch(
      runsSlice.actions.eventReceived({
        schemaVersion: 1,
        runId: 'run-1',
        seq: 7,
        type: 'stepEntered',
        correlationId: '',
        payload: { step: 'build' },
      }),
    );
    store.dispatch(
      runsSlice.actions.watchStatusChanged({
        runId: 'run-1',
        status: { state: 'lost', attempt: 5, maxAttempts: 5 },
      }),
    );

    await renderDetail();

    expect(screen.getByTestId('stream-status')).toHaveAttribute('data-state', 'lost');
    fireEvent.click(screen.getByTestId('stream-reconnect'));

    const lastCall = vi.mocked(startWatch).mock.calls.at(-1);
    expect(lastCall?.[0]).toBe('run-1');
    expect(lastCall?.[1]).toBe(7);
  });

  test('marks the stream as stopped on an auth rejection', async () => {
    store.dispatch(
      runsSlice.actions.watchStatusChanged({
        runId: 'run-1',
        status: { state: 'unauthenticated', attempt: 1, maxAttempts: 5 },
      }),
    );

    await renderDetail();

    expect(screen.getByTestId('stream-status')).toHaveAttribute('data-state', 'unauthenticated');
    expect(screen.getByTestId('stream-status')).toHaveTextContent('sign in again to resume');
    expect(screen.queryByTestId('stream-reconnect')).not.toBeInTheDocument();
  });
});

describe('RunDetailPage getRun error handling', () => {
  afterEach(() => {
    fixture.error = undefined;
  });

  function renderDetail() {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );
  }

  test('prompts re-authentication when GetRun rejects with 401', async () => {
    fixture.error = { status: 'unauthenticated', data: 'token rejected' };

    renderDetail();

    // The auth failure must never fall through to "Run not found.".
    expect(await screen.findByText('Session expired')).toBeInTheDocument();
    expect(
      screen.getByText('Your token was rejected by Castle. Sign in again to continue.'),
    ).toBeInTheDocument();
    expect(screen.getByTestId('page-state-reauth')).toBeInTheDocument();
    expect(screen.queryByText('Run not found.')).not.toBeInTheDocument();
    expect(screen.queryByTestId('page-state-retry')).not.toBeInTheDocument();
    expect(screen.queryByText('Workflow source')).not.toBeInTheDocument();
  });

  test('renders Run not found without retry when GetRun rejects with 404', async () => {
    fixture.error = { status: 'not_found', data: 'no such run' };

    renderDetail();

    expect(await screen.findByText('Run not found.')).toBeInTheDocument();
    expect(screen.getByText("This run doesn't exist or was removed.")).toBeInTheDocument();
    // Retrying a nonexistent run is useless: retry is reserved for
    // recoverable (server/auth) failures — only back navigation is offered.
    expect(screen.queryByTestId('page-state-retry')).not.toBeInTheDocument();
    expect(screen.queryByTestId('page-state-reauth')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to runs' })).toBeInTheDocument();
  });
});

describe('RunDetailPage panel fullscreen', () => {
  beforeEach(() => {
    fixture.error = undefined;
    // Live-tail behaviors (bottom-pinned log) assume a running run.
    fixture.data.status = 'running';
    vi.mocked(startWatch).mockClear();
  });

  function renderDetail() {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );
  }

  test('events render inside the graph box with the step filter and docked continuity', async () => {
    // Pre-seeded history gives the log a scrollable, bottom-pinned tail like
    // a live run; the event delivered below must still land in the docked
    // strip.
    server.use(
      http.post(serverPath('ListRunEvents'), () =>
        HttpResponse.json({ events: wireEvents(30), last_seq: '30' }),
      ),
    );

    renderDetail();

    const scroller = await screen.findByTestId('event-log-scroll');
    await vi.waitFor(() => expect(selectRunEvents('run-1')(store.getState())).toHaveLength(30));
    await vi.waitFor(() => expect(scroller.scrollTop).toBeGreaterThan(0));

    const panel = screen.getByTestId('events-panel');
    // No fullscreen machinery on events: it is docked furniture inside the
    // graph box.
    const graphPanel = screen.getByTestId('graph-panel');
    expect(graphPanel).toContainElement(panel);
    expect(panel.className).not.toContain('fixed');
    expect(screen.queryByTestId('events-panel-expand')).not.toBeInTheDocument();
    expect(screen.queryAllByRole('button', { name: 'Expand Events panel' })).toHaveLength(0);

    // A live event flows through the still-mounted hook and renders in the
    // strip's tail.
    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 31,
          type: 'stepOutcome',
          correlationId: '',
          payload: { step: 'build', outcome: 'success' },
        }),
      );
      scroller.dispatchEvent(new Event('scroll'));
    });
    expect(selectRunEvents('run-1')(store.getState())).toHaveLength(31);
    // The new stepOutcome row lands in the strip's tail (its seq cell).
    expect(await within(panel).findByText('#31')).toBeInTheDocument();
    expect(screen.getByTestId('event-log-scroll')).toBe(scroller);
    // The watch hook started once for the page mount.
    expect(startWatch).toHaveBeenCalledTimes(1);

    // Re-expanding the events panel renders the same virtualized window:
    // the strip is docked furniture, and the collapse/expand cycle keeps
    // the log's state in the page's expansion set.
    const toggle = screen.getByTestId('events-panel-collapse');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
  });

  test('the step filter renders inside the events panel and clears there', async () => {
    renderDetail();

    await screen.findByTestId('events-panel');
    // No filter initially.
    expect(screen.queryByTestId('step-filter')).not.toBeInTheDocument();

    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 202,
          type: 'stepOutcome',
          correlationId: '',
          payload: { step: 'test', outcome: 'success' },
        }),
      );
    });
    act(() => {
      fireEvent.click(document.querySelector('[data-node-id="test"]')!);
    });

    // The filter chip lives inside the events panel (its own control row).
    const eventsPanel = screen.getByTestId('events-panel');
    const filter = within(eventsPanel).getByTestId('step-filter');
    expect(filter).toHaveTextContent('Filtered to step: test');

    // Clearing removes it and widens the log again.
    fireEvent.click(within(filter).getByTestId('clear-step-filter'));
    expect(screen.queryByTestId('step-filter')).not.toBeInTheDocument();
    expect(within(eventsPanel).getByText('#202')).toBeInTheDocument();
  });

  test('expands the graph panel fullscreen and collapses back with the graph intact', async () => {
    renderDetail();

    const graphPanel = await screen.findByTestId('workflow-graph');
    const expand = screen.getByTestId('graph-panel-expand');
    await userEvent.click(expand);

    const panel = screen.getByTestId('graph-panel');
    expect(panel.className).toContain('fixed');
    expect(panel).toHaveAttribute('data-expanded', 'true');
    expect(expand).toHaveAttribute('aria-expanded', 'true');
    // Same component instance with all nodes still rendered while expanded.
    expect(screen.getByTestId('workflow-graph')).toBe(graphPanel);
    expect(within(graphPanel).getAllByTestId('graph-node')).toHaveLength(3);

    await userEvent.click(expand);
    expect(screen.getByTestId('graph-panel').className).not.toContain('fixed');
    expect(screen.getByTestId('workflow-graph')).toBe(graphPanel);
    expect(within(graphPanel).getAllByTestId('graph-node')).toHaveLength(3);
  });

  test('toggles the graph orientation and swaps the handle sides', async () => {
    renderDetail();

    await screen.findByTestId('workflow-graph');
    const tb = screen.getByTestId('orientation-top-bottom');
    const lr = screen.getByTestId('orientation-left-right');
    // Left-right is the default orientation (entry leftmost, terminals
    // right); top-bottom stays available via the toggle.
    expect(lr).toHaveAttribute('aria-pressed', 'true');
    expect(tb).toHaveAttribute('aria-pressed', 'false');
    const handlePositions = (container: HTMLElement) =>
      Array.from(container.querySelectorAll('.react-flow__handle')).map((el) =>
        el.getAttribute('data-handlepos'),
      );
    expect(handlePositions(screen.getByTestId('workflow-graph')).filter((pos) => pos === 'right').length).toBeGreaterThan(0);

    await userEvent.click(tb);
    expect(tb).toHaveAttribute('aria-pressed', 'true');
    expect(lr).toHaveAttribute('aria-pressed', 'false');
    // The orientation remounts the flow; handles move to the bottom side.
    await vi.waitFor(() => {
      const positions = handlePositions(screen.getByTestId('workflow-graph'));
      expect(positions.filter((pos) => pos === 'bottom').length).toBeGreaterThan(0);
      expect(positions.some((pos) => pos === 'right')).toBe(false);
    });

    await userEvent.click(lr);
    await vi.waitFor(() => {
      expect(handlePositions(screen.getByTestId('workflow-graph')).filter((pos) => pos === 'right').length).toBeGreaterThan(0);
    });
  });

  test('follow mode centers the viewport on the running step', async () => {
    renderDetail();

    await screen.findByTestId('workflow-graph');
    const follow = screen.getByTestId('graph-follow-toggle');
    expect(follow).toHaveAttribute('aria-pressed', 'false');
    // No step is running yet in the fixture, so enabling follow with no
    // current step must not crash and stays off-target (no transform churn).
    await userEvent.click(follow);
    expect(follow).toHaveAttribute('aria-pressed', 'true');

    // The engine enters a step; with follow on, the viewport recenters on it.
    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 110,
          type: 'stepEntered',
          ts: '',
          correlationId: '',
          payload: { step: 'test' },
        }),
      );
    });
    const graphPanel = screen.getByTestId('workflow-graph');
    const viewport = () => graphPanel.querySelector('.react-flow__viewport') as HTMLElement;
    const before = viewport().style.transform;
    await vi.waitFor(
      () => {
        expect(viewport().style.transform).not.toBe(before);
      },
      { timeout: 1500 },
    );
    // Let the follow transition (400ms) complete so the paused comparison
    // below is not racing a still-animating transform.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 500));
    });

    // Toggling follow off stops tracking: a later entry does not re-center.
    await userEvent.click(follow);
    const paused = viewport().style.transform;
    act(() => {
      store.dispatch(
        runsSlice.actions.eventReceived({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 111,
          type: 'stepEntered',
          ts: '',
          correlationId: '',
          payload: { step: 'build' },
        }),
      );
    });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 300));
    });
    expect(viewport().style.transform).toBe(paused);
  });

  test('the drawer provides the source view the graph node-clicks pair with', async () => {
    renderDetail();

    // Default: the source opens inside the drawer (single placement, no
    // side/under switching, no separate dock).
    const drawer = await screen.findByTestId('run-detail-drawer');
    expect(within(drawer).getByTestId('source-drawer')).toBeInTheDocument();
    expect(within(drawer).getByTestId('workflow-source-view')).toBeInTheDocument();
  });

  test('the layer body swaps in the drawer source when compiled JSON arrives', async () => {
    renderDetail();

    const drawer = await screen.findByTestId('run-detail-drawer');
    await within(drawer).findByTestId('workflow-source-view');
  });

  test('expands the inspection panel fullscreen and collapses back with data intact', async () => {
    renderDetail();

    const inspection = await screen.findByTestId('run-inspection');
    expect(await within(inspection).findByTestId('inspection-current-step')).toHaveTextContent('build');

    // Inspection is not an expandable panel — it lives as a DetailPanel
    // section inside the right drawer.
    const drawer = await screen.findByTestId('run-detail-drawer');
    expect(within(drawer).getByTestId('run-inspection')).toBe(inspection);
    expect(within(inspection).getByTestId('inspection-current-step')).toHaveTextContent('build');
  });

  test('the graph fullscreen swap leaves the docked events strip untouched', async () => {
    renderDetail();

    const scroller = screen.getByTestId('event-log-scroll');

    const graphExpand = screen.getByTestId('graph-panel-expand');
    await userEvent.click(graphExpand);

    // The graph takes fullscreen; the events strip stays docked inside it
    // with the same log node (single mounted tree, no overlay swap).
    expect(screen.getByTestId('graph-panel').className).toContain('fixed');
    expect(screen.getByTestId('graph-panel')).toHaveAttribute('data-expanded', 'true');
    expect(screen.getByTestId('event-log-scroll')).toBe(scroller);

    // Escape targets the graph overlay and returns focus to its control.
    await userEvent.keyboard('{Escape}');
    expect(screen.getByTestId('graph-panel').className).not.toContain('fixed');
    expect(screen.getByTestId('graph-panel')).toHaveAttribute('data-expanded', 'false');
    expect(graphExpand).toHaveFocus();
  });

  test('shows the workflow source and scope in a collapsible right drawer', async () => {
    renderDetail();

    // Default: drawer open, source rendered inside it.
    const drawer = await screen.findByTestId('run-detail-drawer');
    expect(within(drawer).getByTestId('source-drawer')).toBeInTheDocument();
    expect(within(drawer).getByText('Workflow source')).toBeInTheDocument();
    expect(within(drawer).getByTestId('scope-controls-panel')).toBeInTheDocument();

    // The header toggle collapses the drawer; source and scope leave the
    // DOM together with it.
    await userEvent.click(screen.getByTestId('run-drawer-toggle'));
    expect(screen.queryByTestId('run-detail-drawer')).toBeNull();
    expect(screen.queryByTestId('source-drawer')).toBeNull();
    expect(screen.queryByTestId('scope-controls-panel')).toBeNull();

    // Re-opening restores the full drawer.
    await userEvent.click(screen.getByTestId('run-drawer-toggle'));
    expect(screen.getByTestId('run-detail-drawer')).toBeInTheDocument();
    expect(screen.getByTestId('source-drawer')).toBeInTheDocument();
  });

  test('the graph stays the only fullscreen panel and events stay docked inside it', async () => {
    renderDetail();

    await screen.findByTestId('events-panel');

    // Only the graph carries fullscreen machinery; events are docked
    // furniture (collapse chevron, no expand affordance).
    const graphPanel = screen.getByTestId('graph-panel');
    const eventsPanel = screen.getByTestId('events-panel');
    expect(eventsPanel.className).not.toContain('fixed');
    expect(graphPanel).toContainElement(eventsPanel);
    const graphExpand = screen.getByTestId('graph-panel-expand');
    expect(graphExpand).toHaveAttribute('aria-expanded', 'false');
    expect(graphExpand).toHaveAttribute('aria-controls', 'graph-panel');
    expect(screen.queryByTestId('events-panel-expand')).not.toBeInTheDocument();
    expect(screen.getByTestId('events-panel-collapse')).toHaveAttribute('aria-expanded', 'true');
  });

  describe('subworkflow drill-down (CRI-257)', () => {
    // Parent module: a subworkflow declaration + a step whose target
    // crosses into it.
    const SUBWORKFLOW_SOURCE =
      'workflow {\n  name = "hello"\n  initial_state = "build"\n}\nsubworkflow "qa_triage" {\n  source = "../qa_triage_v1"\n}\nstep "build" {\n  outcome "success" { next = step.test }\n}\nstep "test" {\n  target = subworkflow.qa_triage\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';
    // Compiled layer body as the workflow.graphs event carries it.
    const LAYER_BODY =
      'workflow {\n  name = "qa_triage"\n  initial_state = "triage"\n}\nstep "triage" {\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';

    function renderSubworkflowPage() {
      fixture.data.workflowHash = SUBWORKFLOW_SOURCE;
      return renderDetail();
    }

    function dispatchGraphsEvent(seq: number) {
      act(() => {
        store.dispatch(
          runsSlice.actions.eventReceived({
            schemaVersion: 1,
            runId: 'run-1',
            seq,
            type: 'workflowGraphs',
            ts: new Date(0).toISOString(),
            correlationId: '',
            payload: {
              subworkflows: [{ name: 'qa_triage', sourcePath: '../qa_triage_v1', body: LAYER_BODY }],
            },
          }),
        );
      });
    }

    function nodeById(id: string): HTMLElement {
      const node = document.querySelector(`[data-testid="workflow-graph"] [data-node-id="${id}"]`);
      if (!node) throw new Error(`graph node "${id}" is not rendered`);
      return node as HTMLElement;
    }

    function visibleNodeIds(): string[] {
      return Array.from(
        document.querySelectorAll('[data-testid="workflow-graph"] [data-node-id]'),
      )      .map((n) => n.getAttribute('data-node-id'))
      .filter((id): id is string => id !== null);
    }

    test('steps targeting a subworkflow show a disabled explore affordance until the layer event arrives', async () => {
      renderSubworkflowPage();

      await screen.findByText('Workflow source');
      const affordance = within(nodeById('test')).getByTestId('graph-node-explore');
      // Grayed-out is the contract, not hidden: no workflow.graphs event
      // yet means no layer graph, so the affordance stays but disabled.
      expect(affordance).toBeDisabled();
      expect(affordance).toHaveAttribute(
        'title',
        'Subworkflow qa_triage graph not available yet',
      );
      expect(screen.queryByTestId('layer-breadcrumb')).not.toBeInTheDocument();
    });

    test('activating the affordance opens the layer graph with breadcrumb and thumbnail', async () => {
      renderSubworkflowPage();
      await screen.findByText('Workflow source');
      dispatchGraphsEvent(1);

      const affordance = within(nodeById('test')).getByTestId('graph-node-explore');
      expect(affordance).toBeEnabled();

      fireEvent.click(affordance);

      // The drill-down chrome appears: breadcrumb over the opened layer
      // and its thumbnail.
      expect(screen.getByTestId('layer-breadcrumb')).toBeInTheDocument();
      const crumbs = screen.getAllByTestId('layer-crumb');
      expect(crumbs).toHaveLength(1);
      expect(crumbs[0]).toHaveTextContent('qa_triage');
      expect(screen.getByTestId('layer-crumb-root')).toBeInTheDocument();
      const thumbs = screen.getAllByTestId('layer-thumb');
      expect(thumbs).toHaveLength(1);
      expect(within(thumbs[0]).getByRole('img', { name: 'qa_triage thumbnail' })).toBeInTheDocument();

      // The graph switches to the layer: its own nodes replace the
      // parent's (waiting for React Flow's measurement-driven passes).
      await vi.waitFor(
        () => {
          const ids = visibleNodeIds();
          expect(ids).toContain('triage');
          expect(ids).not.toContain('build');
        },
        { timeout: 1500 },
      );

      // The source pane now shows the layer's own module body.
      const sourceBody = within(screen.getByTestId('run-detail-drawer')).getByTestId('workflow-source-view');
      expect(sourceBody.textContent).toContain('initial_state = "triage"');
      expect(sourceBody.textContent).not.toContain('initial_state = "build"');
    });

    test('the breadcrumb returns to the parent workflow (and closes the drill-down)', async () => {
      renderSubworkflowPage();
      await screen.findByText('Workflow source');
      dispatchGraphsEvent(1);

      fireEvent.click(within(nodeById('test')).getByTestId('graph-node-explore'));
      await screen.findByTestId('layer-breadcrumb');

      fireEvent.click(screen.getByTestId('layer-crumb-root'));

      // Back at the top-level workflow: root nodes are back, the
      // drill-down chrome is gone (the always-present mini-graph rail
      // remains with the root thumbnail only).
      expect(screen.queryByTestId('layer-breadcrumb')).not.toBeInTheDocument();
      expect(screen.queryByTestId('layer-thumb')).not.toBeInTheDocument();
      expect(screen.getByTestId('layer-thumb-root')).toBeInTheDocument();
      await vi.waitFor(
        () => {
          const ids = visibleNodeIds();
          expect(ids).toContain('build');
          expect(ids).not.toContain('triage');
        },
        { timeout: 1500 },
      );
    });

    test('the layer source participates in node-click highlighting', async () => {
      renderSubworkflowPage();
      await screen.findByText('Workflow source');
      dispatchGraphsEvent(1);

      fireEvent.click(within(nodeById('test')).getByTestId('graph-node-explore'));
      await vi.waitFor(
        () => {
          expect(visibleNodeIds()).toContain('triage');
        },
        { timeout: 1500 },
      );

      // Selecting a layer node highlights its block inside the layer body.
      fireEvent.click(nodeById('triage'));
      const sourceBody = within(screen.getByTestId('run-detail-drawer')).getByTestId('workflow-source-view');
      expect(sourceBody.textContent).toContain('step "triage"');
      // The highlight target is the layer's block, found inside the layer
      // source (the parent body has no such step).
      expect(sourceBody.textContent).not.toContain('initial_state = "build"');
    });

    // CRI-294: the deployed emitter ships layer bodies as the compiled
    // module JSON (the Evidence-B payload shape), not HCL source — the
    // drill-down must still open and the pane must render the body
    // readably.
    test('follow mode auto-traverses into the layer containing the running step', async () => {
      renderSubworkflowPage();
      await screen.findByText('Workflow source');
      dispatchGraphsEvent(500);

      // Enable follow while the engine is still in the root.
      await userEvent.click(screen.getByTestId('graph-follow-toggle'));
      expect(screen.getByTestId('graph-follow-toggle')).toHaveAttribute('aria-pressed', 'true');
      act(() => {
        store.dispatch(
          runsSlice.actions.eventReceived({
            schemaVersion: 1,
            runId: 'run-1',
            seq: 501,
            type: 'stepEntered',
            ts: '',
            correlationId: '',
            payload: { step: 'build' },
          }),
        );
      });
      await vi.waitFor(() => {
        expect(visibleNodeIds()).toContain('build');
      });

      // The engine enters qa_triage's own step: the view auto-opens the
      // layer so the follower is never stranded at the parent level.
      act(() => {
        store.dispatch(
          runsSlice.actions.eventReceived({
            schemaVersion: 1,
            runId: 'run-1',
            seq: 502,
            type: 'stepEntered',
            ts: '',
            correlationId: '',
            payload: { step: 'triage' },
          }),
        );
      });
      await vi.waitFor(
        () => {
          expect(visibleNodeIds()).toContain('triage');
          expect(visibleNodeIds()).not.toContain('build');
        },
        { timeout: 1500 },
      );
      // The drill-down chrome reflects the auto-opened stack.
      expect(screen.getByTestId('layer-breadcrumb')).toBeInTheDocument();
    });

    test('a compiled-JSON layer body opens the drill-down and renders the module body', async () => {
      // Parent module whose step runs the `inner_task` layer (the
      // Evidence-B demo shape).
      fixture.data.workflowHash =
        'workflow {\n  name = "hello"\n  initial_state = "build"\n}\nsubworkflow "inner_task" {\n  source = "/tmp/cri286-fixture/subworkflows/inner"\n}\nstep "build" {\n  outcome "success" { next = state.done }\n}\nstep "test" {\n  target = subworkflow.inner_task\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';
      const compiledBody = JSON.stringify({
        name: 'inner_task',
        initial_state: 'execute',
        target_state: 'complete',
        adapters: [{ type: 'shell', name: 'default', on_crash: 'fail', config_keys: null }],
        steps: [
          {
            name: 'execute',
            adapter: 'shell.default',
            input_keys: ['command'],
            allow_tools: null,
            outcomes: [
              { name: 'failure', next: 'complete' },
              { name: 'success', next: 'complete' },
            ],
          },
        ],
        states: [{ name: 'complete', terminal: true, success: true }],
        outputs: [],
        switches: [],
        step_order: ['execute'],
        plugins_required: ['shell'],
        metadata: { schema_version: 1 },
      });
      renderDetail();
      await screen.findByText('Workflow source');
      act(() => {
        store.dispatch(
          runsSlice.actions.eventReceived({
            schemaVersion: 1,
            runId: 'run-1',
            seq: 1,
            type: 'workflowGraphs',
            ts: new Date(0).toISOString(),
            correlationId: '',
            payload: {
              subworkflows: [
                { name: 'inner_task', sourcePath: '/tmp/cri286-fixture/subworkflows/inner', body: compiledBody },
              ],
            },
          }),
        );
      });

      // The affordance on the subworkflow-carrying step is enabled.
      const affordance = within(nodeById('test')).getByTestId('graph-node-explore');
      expect(affordance).toBeEnabled();

      fireEvent.click(affordance);

      // The layer opens: breadcrumb plus the layer's own nodes.
      expect(screen.getByTestId('layer-breadcrumb')).toBeInTheDocument();
      await vi.waitFor(
        () => {
          const ids = visibleNodeIds();
          expect(ids).toContain('execute');
          expect(ids).toContain('complete');
          expect(ids).not.toContain('build');
        },
        { timeout: 1500 },
      );

      // The source pane shows the layer's compiled module body,
      // pretty-printed rather than the compact one-line blob the event
      // carries. The source lives in the right drawer now.
      const sourceBody = within(screen.getByTestId('run-detail-drawer')).getByTestId('workflow-source-view');
      expect(JSON.parse(sourceBody.textContent ?? '')).toEqual(JSON.parse(compiledBody));
      expect(sourceBody.textContent).toContain('\n  "name": "inner_task"');
      expect(sourceBody.textContent).not.toContain(compiledBody.slice(0, 40));
    });

    // CRI-296: nested layers ride inside a layer body's own `subworkflows`
    // key — drill-down must not stop one layer deep. Shaped like the
    // nesting observed on run fd98126e: handler inlines
    // pair_programming_loop, which renders as a depth-3 stack
    // (linear_develop_v1 > handler > pair_programming_loop).
    test('a nested layer inside an opened layer opens at depth 3', async () => {
      // Top-level module whose step runs the `handler` layer.
      fixture.data.workflowHash =
        'workflow {\n  name = "linear_develop_v1"\n  initial_state = "build"\n}\nsubworkflow "handler" {\n  source = "../handler"\n}\nstep "build" {\n  outcome "success" { next = step.test }\n}\nstep "test" {\n  target = subworkflow.handler\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';
      const pairBody = JSON.stringify({
        name: 'pair_programming_loop',
        initial_state: 'pair',
        steps: [{ name: 'pair', outcomes: [{ name: 'success', next: 'wrap' }] }],
        states: [{ name: 'wrap', terminal: true, success: true }],
      });
      // The handler layer's compiled body: a step runs the nested
      // `pair_programming_loop` layer, which the body inlines.
      const handlerBody = JSON.stringify({
        name: 'handler',
        initial_state: 'route',
        steps: [
          { name: 'route', subworkflow: 'pair_programming_loop', outcomes: [{ name: 'success', next: 'done_handler' }] },
        ],
        states: [{ name: 'done_handler', terminal: true, success: true }],
        subworkflows: [
          { name: 'pair_programming_loop', source_path: '../pair_programming_loop_v1', body: pairBody },
        ],
      });
      renderDetail();
      await screen.findByText('Workflow source');
      act(() => {
        store.dispatch(
          runsSlice.actions.eventReceived({
            schemaVersion: 1,
            runId: 'run-1',
            seq: 1,
            type: 'workflowGraphs',
            ts: new Date(0).toISOString(),
            correlationId: '',
            payload: {
              subworkflows: [{ name: 'handler', sourcePath: '../handler', body: handlerBody }],
            },
          }),
        );
      });

      // Open the handler layer from the top-level workflow.
      fireEvent.click(within(nodeById('test')).getByTestId('graph-node-explore'));
      await vi.waitFor(
        () => {
          expect(visibleNodeIds()).toContain('route');
        },
        { timeout: 1500 },
      );

      // INSIDE the handler layer, the nested affordance is enabled (the
      // regression: nested layers must land in the built layer map).
      const nestedAffordance = within(nodeById('route')).getByTestId('graph-node-explore');
      expect(nestedAffordance).toBeEnabled();
      fireEvent.click(nestedAffordance);

      // The breadcrumb reads linear_develop_v1 > handler >
      // pair_programming_loop, and the graph shows the nested layer's own
      // nodes.
      const crumbs = screen.getAllByTestId('layer-crumb');
      expect(crumbs).toHaveLength(2);
      expect(crumbs[0]).toHaveTextContent('handler');
      expect(crumbs[1]).toHaveTextContent('pair_programming_loop');
      expect(crumbs[1]).toHaveAttribute('aria-current', 'page');
      await vi.waitFor(
        () => {
          expect(visibleNodeIds()).toContain('pair');
          expect(visibleNodeIds()).not.toContain('route');
        },
        { timeout: 1500 },
      );

      // Back navigation returns through the stack: one level up first...
      fireEvent.click(crumbs[0]);
      await vi.waitFor(
        () => {
          expect(visibleNodeIds()).toContain('route');
        },
        { timeout: 1500 },
      );
      expect(screen.getAllByTestId('layer-crumb')).toHaveLength(1);

      // ...and the root crumb closes the drill-down.
      fireEvent.click(screen.getByTestId('layer-crumb-root'));
      await vi.waitFor(
        () => {
          expect(visibleNodeIds()).toContain('build');
        },
        { timeout: 1500 },
      );
      expect(screen.queryByTestId('layer-breadcrumb')).not.toBeInTheDocument();
    });

    // CRI-297: the wire shape captured on run fd98126e (WorkflowGraphs
    // seq 1, replayed verbatim from the fixture) differs from the
    // string-body shape above — the emitter stringifies only top-level
    // layer bodies, so the nested entries inside a body's `subworkflows`
    // key ride as INLINE OBJECTS with snake_case source_path. The CRI-296
    // walk skipped those entries entirely; every nested affordance inside
    // the opened handler layer stayed grayed out with "graph not
    // available yet".
    test('the fd98126e wire shape (inline object nested bodies) drills to depth 3', async () => {
      fixture.data.workflowHash =
        'workflow {\n  name = "linear_develop_v1"\n  initial_state = "build"\n}\nsubworkflow "handler" {\n  source = "../handler"\n}\nstep "build" {\n  outcome "success" { next = step.test }\n}\nstep "test" {\n  target = subworkflow.handler\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';
      renderDetail();
      await within(screen.getByTestId('run-detail-drawer')).findByTestId('workflow-source-view');
      act(() => {
        store.dispatch(
          runsSlice.actions.eventReceived({
            schemaVersion: 1,
            runId: 'run-1',
            seq: 1,
            type: 'workflowGraphs',
            ts: new Date(0).toISOString(),
            correlationId: '',
            payload: wirePayload,
          }),
        );
      });

      // Open the handler layer from the top-level workflow. The captured
      // handler module (workstream_handler_v1) starts at read_workstream.
      fireEvent.click(within(nodeById('test')).getByTestId('graph-node-explore'));
      await vi.waitFor(
        () => {
          expect(visibleNodeIds()).toContain('read_workstream');
        },
        { timeout: 1500 },
      );

      // INSIDE the handler layer, ALL FOUR nested affordances are enabled —
      // the reported symptom had every one grayed out ("graph not
      // available yet") because inline-object bodies were skipped. The
      // captured handler module's subworkflow-bearing steps:
      // run_branch_manager -> branch_manager, run_pr_reviewer_loop ->
      // pr_reviewer_loop, run_pair_programming_loop ->
      // pair_programming_loop, run_pair_programming_loop_with_feedback ->
      // pair_programming_loop_feedback.
      for (const step of [
        'run_branch_manager',
        'run_pr_reviewer_loop',
        'run_pair_programming_loop',
        'run_pair_programming_loop_with_feedback',
      ]) {
        expect(within(nodeById(step)).getByTestId('graph-node-explore')).toBeEnabled();
      }

      // Opening pair_programming_loop renders its own graph at depth 3.
      fireEvent.click(within(nodeById('run_pair_programming_loop')).getByTestId('graph-node-explore'));
      const crumbs = screen.getAllByTestId('layer-crumb');
      expect(crumbs).toHaveLength(2);
      expect(crumbs[0]).toHaveTextContent('handler');
      expect(crumbs[1]).toHaveTextContent('pair_programming_loop');
      expect(crumbs[1]).toHaveAttribute('aria-current', 'page');
      await vi.waitFor(
        () => {
          // The captured pair_loop module's real 15 steps render.
          expect(visibleNodeIds()).toEqual(
            expect.arrayContaining([
              'get_branch_name',
              'read_workstream',
              'checkout_branch',
              'check_working_tree',
              'develop',
              'push_checkpoint',
              'write_work_log',
              'commit_work_log',
              'push_wip_checkpoint',
              'verify_committed',
              'ci_gate',
              'review',
              'write_review_log',
              'commit_review_log',
              'push_review_checkpoint',
            ]),
          );
          // The drill-down replaced the handler view: none of the handler
          // graph's own nodes stay on screen.
          expect(visibleNodeIds()).not.toContain('route');
          expect(visibleNodeIds()).not.toContain('run_pair_programming_loop');
        },
        { timeout: 1500 },
      );
    });
  });
});

// The capabilities prop (CRI-257) lets a host hand the page its capability
// probe result; castle mode stays the default.
describe('RunDetailPage capabilities', () => {
  function renderDetail(capabilities?: RunCapabilities) {
    render(
      <Provider store={createRunViewerStore()}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage capabilities={capabilities} />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );
  }

  test('castle capabilities are the default, so the control row stays wired', async () => {
    renderDetail();

    await screen.findByText('Workflow source');
    // Scope to the header control row: the graph toolbar now hosts the
    // view-level motion toggle (Pause/Resume), whose labels would collide
    // with the engine control row's buttons in a screen-wide query.
    const actions = within(screen.getByTestId('run-detail-actions'));
    expect(actions.getByRole('button', { name: 'Pause' })).toBeEnabled();
    expect(actions.getByRole('button', { name: 'Stop' })).toBeEnabled();
    expect(actions.getByRole('button', { name: 'Stop' }).getAttribute('title')).not.toBe(NO_CONTROLS_TOOLTIP);
  });

  test('renders the control row grayed-out when the host has no control RPC', async () => {
    renderDetail(NO_CONTROL_CAPABILITIES);

    await screen.findByText('Workflow source');
    const actions = within(screen.getByTestId('run-detail-actions'));
    for (const name of ['Pause', 'Resume', 'Stop']) {
      const button = actions.getByRole('button', { name });
      // Grayed-out is the contract, not hidden: the row stays visible with
      // the shared tooltip while every control is disabled.
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    }
  });
});
