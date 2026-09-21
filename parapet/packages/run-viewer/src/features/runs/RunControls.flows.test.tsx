import { http, HttpResponse } from 'msw';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { RunControls } from './RunControls';
import { castleApi } from '../../api/castleApi';
import { store } from '../../store';
import { server } from '../../test/mocks/server';
import { serverPath } from '../../test/mocks/handlers';

// Integration tests through the real castleApi slice and the connect-web
// transport: the mutation meta must actually transition (pending → success /
// error) for the feedback and inline-error behaviours under test.

function renderControls(runId = 'run-1') {
  return render(
    <Provider store={store}>
      <RunControls
        runId={runId}
        status="running"
        pauseState={{ isPaused: false, pauseEvent: null }}
      />
    </Provider>,
  );
}

function errorResponse(code: string, message: string) {
  return new HttpResponse(JSON.stringify({ code, message }), {
    status: 412,
    headers: { 'content-type': 'application/json' },
  });
}

beforeEach(() => {
  store.dispatch(castleApi.util.resetApiState());
});

afterEach(() => {
  store.dispatch(castleApi.util.resetApiState());
});

describe('RunControls end-to-end flows', () => {
  test('confirming the dialog issues StopRun and shows the stop-requested indication from issued_at', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(serverPath('StopRun'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({ issued_at: '2026-09-16T17:00:00.000Z' });
      }),
    );
    const user = userEvent.setup();
    renderControls();

    const stop = screen.getByRole('button', { name: 'Stop' });
    await user.click(stop);
    await user.click(screen.getByRole('button', { name: 'Stop run' }));

    expect(bodies).toHaveLength(1);
    expect(String(bodies[0].runId ?? bodies[0].run_id)).toBe('run-1');

    const note = await screen.findByTestId('stop-requested');
    expect(note).toHaveTextContent('Stop requested at 2026-09-16T17:00:00.000Z');
    expect(note).toHaveTextContent('status updates arrive via the event stream');

    // No duplicate cancels once a stop is in flight.
    expect(screen.getByRole('button', { name: 'Stop' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Stop' })).toHaveAttribute(
      'title',
      'Stop already requested',
    );
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  test('cancelling the dialog never issues StopRun', async () => {
    const stopCalls: number[] = [];
    server.use(
      http.post(serverPath('StopRun'), () => {
        stopCalls.push(1);
        return HttpResponse.json({ issued_at: new Date().toISOString() });
      }),
    );
    const user = userEvent.setup();
    renderControls();

    await user.click(screen.getByRole('button', { name: 'Stop' }));
    await user.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(stopCalls).toHaveLength(0);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  test('stop connect errors surface inline as readable text and stay retryable', async () => {
    server.use(
      http.post(serverPath('StopRun'), () =>
        errorResponse('failed_precondition', 'criteria agent not connected'),
      ),
    );
    const user = userEvent.setup();
    renderControls();

    await user.click(screen.getByRole('button', { name: 'Stop' }));
    await user.click(screen.getByRole('button', { name: 'Stop run' }));

    const error = await screen.findByTestId('stop-error');
    expect(error).toHaveTextContent(
      'Stop failed: criteria agent not connected (failed_precondition)',
    );
    // The dialog closed so the error is visible, and the control remains
    // available for a retry.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Stop' })).toBeEnabled();
  });

  test('pause connect errors surface inline', async () => {
    server.use(
      http.post(serverPath('PauseRun'), () =>
        errorResponse('failed_precondition', 'criteria agent not connected'),
      ),
    );
    const user = userEvent.setup();
    renderControls();

    await user.click(screen.getByRole('button', { name: 'Pause' }));

    expect(await screen.findByTestId('pause-error')).toHaveTextContent(
      'Pause failed: criteria agent not connected (failed_precondition)',
    );
    // The control recovers once the mutation settles.
    expect(await screen.findByRole('button', { name: 'Pause' })).toBeEnabled();
  });

  test('resume connect errors surface inline', async () => {
    server.use(
      http.post(serverPath('ResumeRun'), () =>
        errorResponse('failed_precondition', 'run is not paused'),
      ),
    );
    const user = userEvent.setup();
    render(
      <Provider store={store}>
        <RunControls
          runId="run-1"
          status="paused"
          pauseState={{ isPaused: true, pauseEvent: null }}
        />
      </Provider>,
    );

    await user.click(screen.getByRole('button', { name: 'Resume' }));

    expect(await screen.findByTestId('resume-error')).toHaveTextContent(
      'Resume failed: run is not paused (failed_precondition)',
    );
  });

  test('shows the optimistic pending state while the pause request is in flight', async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.post(serverPath('PauseRun'), async () => {
        await gate;
        return HttpResponse.json({ issued_at: '2026-09-16T17:00:00.000Z' });
      }),
    );
    const user = userEvent.setup();
    renderControls();
    try {
      await user.click(screen.getByRole('button', { name: 'Pause' }));

      const pending = await screen.findByRole('button', { name: 'Pausing…' });
      expect(pending).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Stop' })).toBeDisabled();
    } finally {
      release?.();
    }

    await vi.waitFor(() =>
      expect(
        screen.queryByRole('button', { name: 'Pausing…' }),
      ).not.toBeInTheDocument(),
    );
  });

  test('shows the optimistic pending state while the stop request is in flight', async () => {
    let release: (() => void) | undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.post(serverPath('StopRun'), async () => {
        await gate;
        return HttpResponse.json({ issued_at: '2026-09-16T17:00:00.000Z' });
      }),
    );
    const user = userEvent.setup();
    renderControls();
    try {
      await user.click(screen.getByRole('button', { name: 'Stop' }));
      await user.click(screen.getByRole('button', { name: 'Stop run' }));

      const pending = await screen.findByRole('button', { name: 'Stopping…' });
      expect(pending).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Cancel' })).toBeEnabled();
      expect(screen.queryByTestId('stop-requested')).not.toBeInTheDocument();
    } finally {
      release?.();
    }

    expect(await screen.findByTestId('stop-requested')).toHaveTextContent(
      'Stop requested at 2026-09-16T17:00:00.000Z',
    );
  });

  test('resets stop state when the runId changes', async () => {
    server.use(
      http.post(serverPath('StopRun'), () =>
        HttpResponse.json({ issued_at: '2026-09-16T17:00:00.000Z' }),
      ),
    );
    const user = userEvent.setup();
    const view = renderControls();
    await user.click(screen.getByRole('button', { name: 'Stop' }));
    await user.click(screen.getByRole('button', { name: 'Stop run' }));
    expect(await screen.findByTestId('stop-requested')).toBeInTheDocument();

    view.rerender(
      <Provider store={store}>
        <RunControls
          runId="run-2"
          status="running"
          pauseState={{ isPaused: false, pauseEvent: null }}
        />
      </Provider>,
    );

    expect(screen.queryByTestId('stop-requested')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Stop' })).toBeEnabled();
  });
});
