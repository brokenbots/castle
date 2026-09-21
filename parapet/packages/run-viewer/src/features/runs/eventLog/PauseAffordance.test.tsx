import { render, screen, fireEvent } from '@testing-library/react';
import { Provider } from 'react-redux';
import { describe, expect, test, vi, beforeEach } from 'vitest';
import { PauseAffordance } from './PauseAffordance';
import type { EventEnvelope } from '../../../api/castleApi';
import { store } from '../../../store';
import { useResumeMutation } from '../../../api/castleApi';

vi.mock('../../../api/castleApi', async () => {
  const actual = await vi.importActual<typeof import('../../../api/castleApi')>(
    '../../../api/castleApi',
  );
  return {
    ...actual,
    useResumeMutation: vi.fn(() => {
      const resume = vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } });
      return [resume, { isLoading: false, error: null, isSuccess: false }];
    }),
  };
});

beforeEach(() => {
  // Restore the default (no-error) hook state; tests that need specific
  // states set their own mockReturnValue before rendering.
  vi.mocked(useResumeMutation).mockClear();
  vi.mocked(useResumeMutation).mockImplementation(
    () =>
      [vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } }), { isLoading: false, error: null, isSuccess: false }] as any,
  );
});

function envelope(type: string, payload: unknown): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq: 1,
    type,
    ts: '2024-01-01T00:00:00Z',
    correlationId: 'corr-1',
    payload,
  };
}

function renderAffordance(pauseEvent: EventEnvelope, onRefresh = vi.fn()) {
  render(
    <Provider store={store}>
      <PauseAffordance runId="run-1" pauseEvent={pauseEvent} onRefresh={onRefresh} />
    </Provider>,
  );
  return onRefresh;
}

describe('PauseAffordance', () => {
  test('renders the pending signal card for a signal wait and forwards onRefresh', () => {
    // Stale failed_precondition puts the card in its informational state, which
    // is where the refresh affordance lives.
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: false, error: { status: 'failed_precondition', data: 'run is not paused' }, isSuccess: false },
    ] as any);
    const onRefresh = renderAffordance(
      envelope('waitEntered', { mode: 'signal', signal: 'deploy-signal' }),
    );

    expect(screen.getByTestId('pending-signal-card')).toBeInTheDocument();
    expect(screen.getByText(/deploy-signal/i)).toBeInTheDocument();

    fireEvent.click(screen.getByTestId('pending-signal-refresh'));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  test('renders the duration countdown for a duration wait', () => {
    renderAffordance(envelope('waitEntered', { mode: 'duration', duration: '10s' }));

    expect(screen.queryByTestId('pending-signal-card')).not.toBeInTheDocument();
  });

  test('renders the approval card for an approval request', () => {
    renderAffordance(
      envelope('approvalRequested', { node: 'deploy', approvers: ['alice'], reason: 'ship it' }),
    );

    expect(screen.getByText(/Approval Required/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Approve/i })).toBeInTheDocument();
  });
});