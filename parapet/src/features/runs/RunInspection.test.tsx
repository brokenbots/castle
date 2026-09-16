import { act, fireEvent, render, screen } from '@testing-library/react';
import { Provider } from 'react-redux';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test, vi } from 'vitest';
import {
  RUN_INSPECTION_POLL_INTERVAL_MS,
  RunInspection,
  adapterStateView,
} from './RunInspection';
import { castleApi } from '../../api/castleApi';
import { store } from '../../store';
import { server } from '../../test/mocks/server';
import { serverPath } from '../../test/mocks/handlers';

// Wire shape mirrors the protojson canonical form (snake_case, int64 as a
// string) that connect-web produces and parses.
function inspectionFixture(overrides: Record<string, unknown> = {}) {
  return {
    run_id: 'run-1',
    session_id: 'sess-7',
    adapter: 'claude-code',
    current_step: 'build',
    pending_permissions: '2',
    last_activity_at: '2026-09-16T17:00:00.000Z',
    state_json: '{"step":"build","ok":true}',
    ...overrides,
  };
}

// Serves the fixture and records every InspectRun request body so tests can
// count poll ticks.
function inspectHandler(fixture: Record<string, unknown>) {
  const requests: Array<Record<string, unknown>> = [];
  server.use(
    http.post(serverPath('InspectRun'), async ({ request }) => {
      requests.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
      return HttpResponse.json(fixture);
    }),
  );
  return requests;
}

// Resetting the api state per render keeps repeated renders of the same
// runId from reusing a prior test's cache entry instead of issuing its own
// fetch.
function renderPanel(status: string) {
  store.dispatch(castleApi.util.resetApiState());
  return render(
    <Provider store={store}>
      <RunInspection runId="run-1" status={status} />
    </Provider>,
  );
}

afterEach(() => {
  vi.useRealTimers();
  store.dispatch(castleApi.util.resetApiState());
});

describe('adapterStateView', () => {
  test('treats undefined, null and blank strings as empty', () => {
    expect(adapterStateView(undefined)).toEqual({ kind: 'empty', text: '' });
    expect(adapterStateView(null)).toEqual({ kind: 'empty', text: '' });
    expect(adapterStateView('')).toEqual({ kind: 'empty', text: '' });
    expect(adapterStateView('   \n\t')).toEqual({ kind: 'empty', text: '' });
  });

  test('pretty-prints well-formed JSON with two-space indentation', () => {
    const view = adapterStateView('{"step":"build","ok":true}');
    expect(view.kind).toBe('pretty');
    expect(view.text).toBe('{\n  "step": "build",\n  "ok": true\n}');
  });

  test('pretty-prints non-object JSON scalars without throwing', () => {
    expect(adapterStateView('5')).toEqual({ kind: 'pretty', text: '5' });
    expect(adapterStateView('"paused"')).toEqual({ kind: 'pretty', text: '"paused"' });
  });

  test('renders malformed JSON raw and never throws', () => {
    const view = adapterStateView('not json {bad');
    expect(view.kind).toBe('raw');
    expect(view.text).toBe('not json {bad');
  });
});

describe('RunInspection', () => {
  test('renders current step, pending permissions, last activity, adapter and session', async () => {
    inspectHandler(
      inspectionFixture({
        last_activity_at: new Date(Date.now() - 150_000).toISOString(),
      }),
    );
    renderPanel('running');

    expect(await screen.findByTestId('inspection-current-step')).toHaveTextContent('build');
    expect(screen.getByTestId('inspection-pending-permissions')).toHaveTextContent('2');
    const activity = await screen.findByText('2 minutes ago');
    expect(activity).toBeInTheDocument();
    expect(screen.getByText('claude-code')).toBeInTheDocument();
    expect(screen.getByText('sess-7')).toBeInTheDocument();
  });

  test('shows a dash for fields the server did not populate', async () => {
    inspectHandler(inspectionFixture({ current_step: '', adapter: '', session_id: '' }));
    renderPanel('running');

    expect(await screen.findByTestId('inspection-current-step')).toHaveTextContent('—');
    // Current step, adapter and session all share the empty placeholder.
    expect(screen.getAllByText('—')).toHaveLength(3);
  });

  test('renders pretty-printed adapter state behind a collapsed toggle', async () => {
    inspectHandler(inspectionFixture());
    renderPanel('running');

    const toggle = await screen.findByTestId('adapter-state-toggle');
    expect(toggle).toHaveTextContent('Adapter state');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByTestId('adapter-state-json')).not.toBeInTheDocument();

    fireEvent.click(toggle);

    expect(screen.getByTestId('adapter-state-json').textContent).toBe(
      JSON.stringify({ step: 'build', ok: true }, null, 2),
    );
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
  });

  test('renders malformed adapter state raw without crashing', async () => {
    inspectHandler(inspectionFixture({ state_json: 'not json {bad' }));
    renderPanel('running');

    const toggle = await screen.findByTestId('adapter-state-toggle');
    expect(toggle).toHaveTextContent('Adapter state (invalid JSON — raw value)');
    fireEvent.click(toggle);

    expect(screen.getByTestId('adapter-state-json').textContent).toBe('not json {bad');
  });

  test('shows an empty notice instead of a toggle when state_json is blank', async () => {
    for (const stateJson of ['', '   ']) {
      inspectHandler(inspectionFixture({ state_json: stateJson }));
      const view = renderPanel('running');

      expect(await screen.findByTestId('adapter-state-empty')).toBeInTheDocument();
      expect(screen.queryByTestId('adapter-state-toggle')).not.toBeInTheDocument();

      view.unmount();
      store.dispatch(castleApi.util.resetApiState());
    }
  });

  test('shows a muted notice when the query fails', async () => {
    server.use(
      http.post(
        serverPath('InspectRun'),
        () =>
          new HttpResponse(
            JSON.stringify({ code: 'unavailable', message: 'criteria store offline' }),
            { status: 503, headers: { 'content-type': 'application/json' } },
          ),
      ),
    );
    renderPanel('running');

    expect(await screen.findByText('Inspection unavailable.')).toBeInTheDocument();
  });

  test('polls on an interval while the run is active', async () => {
    const requests = inspectHandler(inspectionFixture());
    vi.useFakeTimers();
    renderPanel('running');

    await act(async () => {});
    expect(requests).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_INSPECTION_POLL_INTERVAL_MS);
    });
    expect(requests.length).toBeGreaterThanOrEqual(2);
  });

  test('fetches once for terminal runs without interval polling', async () => {
    const requests = inspectHandler(inspectionFixture());
    vi.useFakeTimers();
    renderPanel('succeeded');

    await act(async () => {});
    expect(requests).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_INSPECTION_POLL_INTERVAL_MS * 3);
    });
    expect(requests).toHaveLength(1);
  });

  test('fetches once for pending runs, which are not active', async () => {
    const requests = inspectHandler(inspectionFixture());
    vi.useFakeTimers();
    renderPanel('pending');

    await act(async () => {});
    expect(requests).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_INSPECTION_POLL_INTERVAL_MS * 3);
    });
    expect(requests).toHaveLength(1);
  });

  test('stops polling once the run transitions to a terminal status', async () => {
    const requests = inspectHandler(inspectionFixture());
    vi.useFakeTimers();
    const view = renderPanel('running');

    await act(async () => {});
    expect(requests).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_INSPECTION_POLL_INTERVAL_MS);
    });
    expect(requests.length).toBeGreaterThanOrEqual(2);

    view.rerender(
      <Provider store={store}>
        <RunInspection runId="run-1" status="succeeded" />
      </Provider>,
    );
    const atTerminal = requests.length;

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_INSPECTION_POLL_INTERVAL_MS * 3);
    });
    expect(requests.length).toBe(atTerminal);
  });
});