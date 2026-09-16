import { render, screen } from '@testing-library/react';
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
// renders nothing when it measures 0.
beforeAll(() => {
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(800);
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

describe('RunDetailPage', () => {
  beforeEach(() => {
    fixture.data.ticket = '';
    fixture.data.repoUrl = '';
    fixture.data.prUrl = '';
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
    // A 1000-event run served page-by-page from a stateful MSW handler.
    // The initial walk must retain only the newest page; older history is
    // fetched on demand through the "Load earlier events" control.
    // Wire shape: protojson flattens the payload oneof, so the case name is
    // a top-level key; connect-web serializes request fields lowerCamelCase.
    const all = Array.from({ length: 1000 }, (_, i) => ({
      schemaVersion: 1,
      runId: 'run-1',
      seq: String(i + 1),
      ts: new Date(0).toISOString(),
      correlationId: '',
      stepLog: { chunk: `chunk ${i + 1}` },
    }));
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

    // Tail page only: the newest page is present in the DOM (windowed), the
    // oldest page is not loaded yet.
    expect(await screen.findByText('chunk 501')).toBeInTheDocument();
    expect(screen.queryByText('chunk 1')).not.toBeInTheDocument();

    // Watch anchored at the newest seq (no full-history replay), after the
    // anchor walk resolves. Mock calls accumulate across tests in this file,
    // so inspect the last call.
    await vi.waitFor(() => expect(startWatch).toHaveBeenCalled());
    const lastWatchCall = vi.mocked(startWatch).mock.calls.at(-1);
    expect(lastWatchCall?.[0]).toBe('run-1');
    expect(lastWatchCall?.[1]).toBe(1000);

    // The store holds exactly the retained tail page (seq 501..1000).
    expect(selectRunEvents('run-1')(store.getState()).map((e) => e.seq)).toEqual(
      Array.from({ length: 500 }, (_, i) => i + 501),
    );

    // Only a window of the 500 loaded events is in the DOM.
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

    // The reader's position is preserved across the prepend: the scroll
    // offset shifted down by the estimated height of the 500 new head rows.
    const scroller = screen.getByTestId('event-log-scroll');
    expect(scroller.scrollTop).toBeGreaterThan(0);

    // Even with all 1000 events loaded, only a window is in the DOM.
    expect(screen.getAllByTestId('event-log-row').length).toBeLessThan(50);

    // Walk probes (since 0, continuation at 500) + the load-earlier seek
    // back to since 0.
    expect(seen).toEqual(['0', '500', '0']);
  });
});
