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
    ticket: '',
    repoUrl: '',
    prUrl: '',
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

describe('RunDetailPage', () => {
  beforeEach(() => {
    fixture.error = undefined;
    fixture.data.ticket = '';
    fixture.data.repoUrl = '';
    fixture.data.prUrl = '';
    // Live-tail affordances key off run status; make the shared fixture's
    // status explicit so tests that change it don't leak.
    fixture.data.status = 'running';
    fixture.data.workflowHash = DEFAULT_WORKFLOW_SOURCE;
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

    getRandomValues.mockRestore();
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

  test('renders ticket, repo and PR link for k8s-native runs', async () => {
    fixture.data.ticket = 'CRI-131';
    fixture.data.repoUrl = 'brokenbots/castle';
    fixture.data.prUrl = 'https://github.com/brokenbots/castle/pull/42';

    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('CRI-131')).toBeInTheDocument();
    expect(await screen.findByText('brokenbots/castle')).toBeInTheDocument();
    const link = screen.getByRole('link', { name: 'PR' });
    expect(link.getAttribute('href')).toBe('https://github.com/brokenbots/castle/pull/42');
    expect(link.getAttribute('rel')).toBe('noreferrer');
  });

  test('does not render PR link for non-http prUrl values', async () => {
    fixture.data.ticket = 'CRI-131';
    fixture.data.prUrl = 'javascript:alert(1)';

    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('CRI-131')).toBeInTheDocument();
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
      const graphSection = screen.getByText('Step graph').closest('section')!;
      const edgeRows = graphSection.querySelectorAll('div.bg-slate-900 > div');
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
    expect(testCard?.className).toContain('border-emerald-400/70');
    expect(doneCard?.className).toContain('opacity-60');
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
    expect(screen.getByText('{"step":"build","outcome":"success"}')).toBeInTheDocument();
    expect(screen.getByText('{"step":"test","outcome":"success"}')).toBeInTheDocument();

    act(() => {
      fireEvent.click(document.querySelector('[data-node-id="test"]')!);
    });
    // The chip names the step and the log shows only that step's events.
    expect(screen.getByTestId('step-filter')).toHaveTextContent('Filtered to step: test');
    expect(screen.queryByText('{"step":"build","outcome":"success"}')).not.toBeInTheDocument();
    expect(screen.getByText('{"step":"test","outcome":"success"}')).toBeInTheDocument();
    // The selected node is highlighted in the graph.
    expect(document.querySelector('[data-node-id="test"]')?.className).toContain('ring-2');

    fireEvent.click(screen.getByTestId('clear-step-filter'));
    expect(screen.queryByTestId('step-filter')).not.toBeInTheDocument();
    expect(screen.getByText('{"step":"build","outcome":"success"}')).toBeInTheDocument();

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
    expect(screen.getByText('{"step":"build","outcome":"success"}')).toBeInTheDocument();
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
    expect(await screen.findByText('Inspection')).toBeInTheDocument();
    expect(screen.getByTestId('inspection-current-step')).toHaveTextContent('build');
    expect(screen.getByText('local')).toBeInTheDocument();
    expect(screen.getByText('sess-1')).toBeInTheDocument();
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

    const layout = screen.getByTestId('run-detail-layout');
    const panel = screen.getByTestId('scope-controls-panel');
    // The panel is part of the main column layout, not a floating overlay.
    expect(layout).toContainElement(panel);
    // The gathered content: control button row, the run scope view, and
    // no duplicated scope content elsewhere.
    expect(within(panel).getByTestId('scope-controls-row')).toBeInTheDocument();
    expect(within(panel).getByTestId('run-scope-panel')).toBeInTheDocument();
    expect(within(panel).queryByTestId('event-log-scroll')).not.toBeInTheDocument();
    expect(within(layout).getByTestId('event-log-scroll')).toBeInTheDocument();

    // The control buttons moved out of the page header into the panel.
    expect(within(panel).getByRole('button', { name: 'Pause' })).toBeInTheDocument();
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

    const toggle = await screen.findByTestId('scope-controls-collapse');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('run-scope-panel')).toBeInTheDocument();

    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    // Collapsed hides the body but keeps the bar with its affordance.
    expect(screen.queryByTestId('scope-controls-body')).not.toBeInTheDocument();
    expect(screen.getByTestId('scope-controls-collapsed')).toBeInTheDocument();
    // The control buttons leave the DOM with the collapsed body.
    expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument();

    await userEvent.click(toggle);
    expect(screen.getByTestId('scope-controls-body')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument();
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

  test('expands the events panel fullscreen with live-tail continuity and collapses back', async () => {
    // Pre-seeded history gives the log a scrollable, bottom-pinned tail like
    // a live run; the event delivered below must still land in the expanded
    // panel.
    server.use(
      http.post(serverPath('ListRunEvents'), () =>
        HttpResponse.json({ events: wireEvents(30), last_seq: '30' }),
      ),
    );

    renderDetail();

    const scroller = await screen.findByTestId('event-log-scroll');
    await vi.waitFor(() => expect(selectRunEvents('run-1')(store.getState())).toHaveLength(30));
    await vi.waitFor(() => expect(scroller.scrollTop).toBeGreaterThan(0));

    const expand = screen.getByTestId('events-panel-expand');
    expect(expand).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(expand);

    const panel = screen.getByTestId('events-panel');
    // Fullscreen overlay above the page chrome.
    expect(panel.className).toContain('fixed');
    expect(panel).toHaveAttribute('data-expanded', 'true');
    expect(expand).toHaveAttribute('aria-expanded', 'true');
    // Same DOM node: expansion must not remount the log, so its scroll
    // anchoring and hooks survive.
    expect(screen.getByTestId('event-log-scroll')).toBe(scroller);
    // The fullscreen markup must give the log a definite height at every
    // level of its ancestor chain, so the log itself stays the scroll
    // container (percentage heights collapse to `auto` otherwise and the
    // overlay would scroll instead — jsdom has no layout, so this class
    // invariant is the meaningful guard, not a scrollTop assertion).
    for (const cls of [
      '[&_[data-testid=events-panel-body]]:flex-col',
      '[&_[data-testid=events-panel-body]]:flex-1',
      '[&_[data-testid=events-panel-body]>div]:flex-col',
      '[&_[data-testid=events-panel-body]>div]:flex-1',
      '[&_[data-testid=events-panel-body]>div>div:last-child]:flex-1',
      '[&_[data-testid=event-log-scroll]]:h-full',
    ]) {
      expect(panel.className).toContain(cls);
    }

    // A live event delivered while expanded flows through the still-mounted
    // hook and renders in the panel's tail.
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
    expect(await within(panel).findByText('{"step":"build","outcome":"success"}')).toBeInTheDocument();

    await userEvent.click(expand);
    expect(screen.getByTestId('events-panel').className).not.toContain('fixed');
    // Docked again: none of the fullscreen sizing variants remain.
    expect(screen.getByTestId('events-panel').className).not.toContain('events-panel-body');
    expect(screen.getByTestId('events-panel').className).not.toContain('event-log-scroll');
    expect(screen.getByTestId('events-panel')).toHaveAttribute('data-expanded', 'false');
    expect(screen.getByTestId('event-log-scroll')).toBe(scroller);
    // The watch hook started once for the page mount; expanding and
    // collapsing must not restart it.
    expect(startWatch).toHaveBeenCalledTimes(1);
  });

  test('Escape collapses the expanded events panel and returns focus to its expand control', async () => {
    renderDetail();

    const expand = screen.getByTestId('events-panel-expand');
    await userEvent.click(expand);
    expect(screen.getByTestId('events-panel').className).toContain('fixed');
    // Move focus away from the toggle to prove Escape restores it.
    expand.blur();
    expect(expand).not.toHaveFocus();

    await userEvent.keyboard('{Escape}');

    expect(screen.getByTestId('events-panel').className).not.toContain('fixed');
    expect(expand).toHaveAttribute('aria-expanded', 'false');
    expect(expand).toHaveFocus();
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
    // Top-bottom is the default orientation.
    expect(tb).toHaveAttribute('aria-pressed', 'true');
    expect(lr).toHaveAttribute('aria-pressed', 'false');
    const handlePositions = (container: HTMLElement) =>
      Array.from(container.querySelectorAll('.react-flow__handle')).map((el) =>
        el.getAttribute('data-handlepos'),
      );
    expect(handlePositions(screen.getByTestId('workflow-graph')).filter((pos) => pos === 'bottom').length).toBeGreaterThan(0);

    await userEvent.click(lr);
    expect(lr).toHaveAttribute('aria-pressed', 'true');
    expect(tb).toHaveAttribute('aria-pressed', 'false');
    // The orientation remounts the flow; handles move to the right side.
    await vi.waitFor(() => {
      const positions = handlePositions(screen.getByTestId('workflow-graph'));
      expect(positions.filter((pos) => pos === 'right').length).toBeGreaterThan(0);
      expect(positions.some((pos) => pos === 'bottom')).toBe(false);
    });

    await userEvent.click(tb);
    await vi.waitFor(() => {
      expect(handlePositions(screen.getByTestId('workflow-graph')).filter((pos) => pos === 'bottom').length).toBeGreaterThan(0);
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

  test('moves the source pane between the graph side and below it, collapsing per placement', async () => {
    renderDetail();

    // Default: source rendered inline under the graph.
    expect(await screen.findByTestId('source-pane-body')).toBeInTheDocument();
    expect(screen.queryByTestId('source-dock')).toBeNull();

    // Side placement swaps the inline body for the right-docked panel.
    await userEvent.click(screen.getByTestId('source-placement-side'));
    expect(screen.getByTestId('source-dock')).toBeInTheDocument();
    expect(within(screen.getByTestId('source-dock')).getByTestId('workflow-source-view')).toBeInTheDocument();
    expect(screen.queryByTestId('source-pane-body')).toBeNull();

    // The side view starts expanded, and its collapse state is its own:
    // closing the dock leaves the under placement's expanded state alone.
    await userEvent.click(within(screen.getByTestId('source-dock')).getByTestId('source-dock-close'));
    expect(screen.queryByTestId('source-dock')).toBeNull();
    expect(screen.getByTestId('source-pane-collapse')).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByTestId('source-pane-collapsed')).toBeInTheDocument();

    // Switching to Under keeps a separate state: it was never collapsed.
    await userEvent.click(screen.getByTestId('source-placement-under'));
    expect(screen.getByTestId('source-pane-body')).toBeInTheDocument();
    expect(screen.getByTestId('source-pane-collapse')).toHaveAttribute('aria-expanded', 'true');

    // Collapsing the under view keeps the bar and restores the body on expand.
    await userEvent.click(screen.getByTestId('source-pane-collapse'));
    expect(screen.getByTestId('source-pane-collapsed')).toBeInTheDocument();
    expect(screen.queryByTestId('source-pane-body')).toBeNull();
    await userEvent.click(screen.getByTestId('source-pane-collapse'));
    expect(screen.getByTestId('source-pane-body')).toBeInTheDocument();

    // Back to Side: the earlier side collapse is still remembered until
    // the side placement is expanded again.
    await userEvent.click(screen.getByTestId('source-placement-side'));
    expect(screen.queryByTestId('source-dock')).toBeNull();
    await userEvent.click(screen.getByTestId('source-pane-collapse'));
    expect(screen.getByTestId('source-dock')).toBeInTheDocument();
  });

  test('expands the inspection panel fullscreen and collapses back with data intact', async () => {
    renderDetail();

    const inspection = await screen.findByTestId('run-inspection');
    expect(await within(inspection).findByTestId('inspection-current-step')).toHaveTextContent('build');

    const expand = screen.getByTestId('inspection-panel-expand');
    await userEvent.click(expand);

    const panel = screen.getByTestId('inspection-panel');
    expect(panel.className).toContain('fixed');
    expect(panel).toHaveAttribute('data-expanded', 'true');
    expect(expand).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('run-inspection')).toBe(inspection);
    expect(within(inspection).getByTestId('inspection-current-step')).toHaveTextContent('build');

    await userEvent.click(expand);
    expect(screen.getByTestId('inspection-panel').className).not.toContain('fixed');
    expect(screen.getByTestId('run-inspection')).toBe(inspection);
  });

  test('expanding one panel collapses the previously expanded one and keeps both mounted', async () => {
    renderDetail();

    const eventsExpand = screen.getByTestId('events-panel-expand');
    await userEvent.click(eventsExpand);
    expect(screen.getByTestId('events-panel').className).toContain('fixed');
    const scroller = screen.getByTestId('event-log-scroll');

    const graphExpand = screen.getByTestId('graph-panel-expand');
    await userEvent.click(graphExpand);

    // Single-panel swap: events returns to the docked layout while the
    // graph takes fullscreen, and the log node survives the swap.
    expect(screen.getByTestId('events-panel').className).not.toContain('fixed');
    expect(screen.getByTestId('events-panel')).toHaveAttribute('data-expanded', 'false');
    expect(screen.getByTestId('graph-panel').className).toContain('fixed');
    expect(screen.getByTestId('graph-panel')).toHaveAttribute('data-expanded', 'true');
    expect(screen.getByTestId('event-log-scroll')).toBe(scroller);

    // Escape now targets the graph overlay and returns focus to its control.
    await userEvent.keyboard('{Escape}');
    expect(screen.getByTestId('graph-panel').className).not.toContain('fixed');
    expect(screen.getByTestId('graph-panel')).toHaveAttribute('data-expanded', 'false');
    expect(graphExpand).toHaveFocus();
  });

  test('panels stay docked by default with expand controls available', async () => {
    renderDetail();

    await screen.findByTestId('events-panel');

    for (const testId of ['events-panel', 'graph-panel', 'inspection-panel']) {
      const panel = screen.getByTestId(testId);
      expect(panel.className).not.toContain('fixed');
      expect(panel).toHaveAttribute('data-expanded', 'false');
    }
    for (const [panelId, expandId] of [
      ['events-panel', 'events-panel-expand'],
      ['graph-panel', 'graph-panel-expand'],
      ['inspection-panel', 'inspection-panel-expand'],
    ] as const) {
      const expand = screen.getByTestId(expandId);
      expect(expand).toHaveAttribute('aria-expanded', 'false');
      expect(expand).toHaveAttribute('aria-controls', panelId);
      expect(screen.getByTestId(panelId)).toHaveAttribute('id', panelId);
    }
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
      const sourceBody = screen.getByTestId('source-pane-body');
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
      // drill-down chrome is gone.
      expect(screen.queryByTestId('layer-breadcrumb')).not.toBeInTheDocument();
      expect(screen.queryByTestId('layer-thumbs')).not.toBeInTheDocument();
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
      const sourceBody = screen.getByTestId('source-pane-body');
      expect(sourceBody.textContent).toContain('step "triage"');
      // The highlight target is the layer's block, found inside the layer
      // source (the parent body has no such step).
      expect(sourceBody.textContent).not.toContain('initial_state = "build"');
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
    expect(screen.getByRole('button', { name: 'Pause' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Stop' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Stop' }).getAttribute('title')).not.toBe(NO_CONTROLS_TOOLTIP);
  });

  test('renders the control row grayed-out when the host has no control RPC', async () => {
    renderDetail(NO_CONTROL_CAPABILITIES);

    await screen.findByText('Workflow source');
    for (const name of ['Pause', 'Resume', 'Stop']) {
      const button = screen.getByRole('button', { name });
      // Grayed-out is the contract, not hidden: the row stays visible with
      // the shared tooltip while every control is disabled.
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    }
  });
});
