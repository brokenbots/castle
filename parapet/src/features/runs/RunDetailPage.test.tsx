import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import {
  beforeAll,
  beforeEach,
  describe,
  expect,
  test,
  vi,
} from 'vitest';
import { RunDetailPage } from './RunDetailPage';
import { store } from '../../store';
import { selectRunEvents } from './runsSlice';
import { server } from '../../test/mocks/server';
import { serverPath } from '../../test/mocks/handlers';

vi.mock('./watchRun', () => ({
  startWatch: vi.fn().mockResolvedValue(undefined),
}));

import { startWatch } from './watchRun';

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

// Mutable fixture so tests can vary run metadata (CRI-131) without a second
// module mock. UseGetRunQuery returns this object verbatim.
const fixture = vi.hoisted(() => ({
  data: {
    runId: 'run-1',
    criteriaId: 'ov-1',
    workflowName: 'hello',
    workflowHash:
      'workflow "hello" {\n  start_at = "build"\n  step "build" {\n    transitions = {\n      "success" = "test"\n    }\n  }\n  step "test" {\n    transitions = {\n      "success" = "done"\n    }\n  }\n  state "done" { terminal = true }\n}',
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
      error: undefined,
      data: fixture.data,
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
    fixture.data.ticket = '';
    fixture.data.repoUrl = '';
    fixture.data.prUrl = '';
    // Live-tail affordances key off run status; make the shared fixture's
    // status explicit so tests that change it don't leak.
    fixture.data.status = 'running';
  });

  test('starts WatchRun with sinceSeq=0 and subscriberId', async () => {
    const randomUUID = vi
      .spyOn(crypto, 'randomUUID')
      .mockReturnValue('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa');

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
    expect(firstCall[2]).toBe('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa');

    randomUUID.mockRestore();
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

  test('anchors at the newest page and lazy-loads older events', async () => {
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

  test('keeps the load-earlier control retryable when the older-page fetch fails', async () => {
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
    await vi.waitFor(() => expect(warn).toHaveBeenCalledTimes(2));
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
});
