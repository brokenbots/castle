import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { describe, expect, test } from 'vitest';
import { store } from '../../store';
import { ScopeAndControlsPanel } from './ScopeAndControlsPanel';
import { NO_CONTROLS_TOOLTIP, NO_CONTROL_CAPABILITIES } from './capabilities';
import type { EventEnvelope } from '../../api/castleApi';

const pauseState = { isPaused: false, pauseEvent: null };

function renderPanel(overrides: Partial<Parameters<typeof ScopeAndControlsPanel>[0]> = {}) {
  const props: Parameters<typeof ScopeAndControlsPanel>[0] = {
    runId: 'run-1',
    status: 'running',
    pauseState,
    events: [] as EventEnvelope[],
    onRefresh: () => {},
    ...overrides,
  };
  return render(
    <Provider store={store}>
      <ScopeAndControlsPanel {...props} />
    </Provider>,
  );
}

describe('ScopeAndControlsPanel', () => {
  test('gathers the control button row and the run scope view in one panel', () => {
    renderPanel();

    const panel = screen.getByTestId('scope-controls-panel');
    expect(within(panel).getByTestId('scope-controls-row')).toBeInTheDocument();
    // The full control matrix stays wired in castle mode: all three
    // actions present and enabled for a running run.
    expect(within(panel).getByRole('button', { name: /Paus/ })).toBeEnabled();
    expect(within(panel).getByRole('button', { name: 'Resume' })).toBeDisabled();
    expect(within(panel).getByRole('button', { name: 'Stop' })).toBeEnabled();
    expect(within(panel).getByTestId('run-scope-panel')).toBeInTheDocument();
  });

  test('renders pause affordances inside the panel while the run is paused', () => {
    renderPanel({
      pauseState: {
        isPaused: true,
        pauseEvent: {
          schemaVersion: 1,
          runId: 'run-1',
          seq: 1,
          type: 'waitEntered',
          ts: new Date(Date.now() + 60_000).toISOString(),
          correlationId: '',
          payload: { mode: 'duration', duration: '5s' },
        },
      },
    });

    const panel = screen.getByTestId('scope-controls-panel');
    // The wait affordance (countdown for a duration wait) lives in the
    // gathered panel, not in a standalone page section.
    expect(within(panel).getByText(/resuming in/)).toBeInTheDocument();
  });

  test('no-control capabilities disable the button row with the tooltip', () => {
    renderPanel({ capabilities: NO_CONTROL_CAPABILITIES });

    for (const name of [/Paus/, 'Resume', 'Stop']) {
      const button = screen.getByRole('button', { name });
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    }
    // The scope view stays readable in standalone mode.
    expect(screen.getByTestId('run-scope-panel')).toBeInTheDocument();
  });

  test('collapses to a bar and restores the body on expand', async () => {
    const user = userEvent.setup();
    renderPanel();

    const toggle = screen.getByTestId('scope-controls-collapse');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('scope-controls-body')).toBeInTheDocument();

    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByTestId('scope-controls-body')).not.toBeInTheDocument();
    expect(screen.getByTestId('scope-controls-collapsed')).toBeInTheDocument();

    await user.click(toggle);
    expect(screen.getByTestId('scope-controls-body')).toBeInTheDocument();
    expect(screen.queryByTestId('scope-controls-collapsed')).not.toBeInTheDocument();
  });
});