import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Provider } from 'react-redux';
import { describe, expect, test, vi } from 'vitest';
import { PendingSignalCard } from './PendingSignalCard';
import { createRunViewerStore } from '../../../store';
import { NO_CONTROLS_TOOLTIP, NO_CONTROL_CAPABILITIES } from '../capabilities';

// One store instance per test file; RTK Query caches per store.
const store = createRunViewerStore();

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

import { useResumeMutation } from '../../../api/castleApi';

function renderCard(overrides: Partial<{ signal: string; runId: string; onRefresh: () => void; capabilities: typeof NO_CONTROL_CAPABILITIES | undefined }> = {}) {
  const props = {
    signal: 'deploy-signal',
    runId: 'run-1',
    onRefresh: vi.fn(),
    ...overrides,
  };
  const result = render(
    <Provider store={store}>
      <PendingSignalCard
        signal={props.signal}
        runId={props.runId}
        onRefresh={props.onRefresh}
        capabilities={props.capabilities}
      />
    </Provider>,
  );
  return { ...result, ...props };
}

describe('PendingSignalCard', () => {
  test('renders the pending signal name and the delivery form', () => {
    renderCard();

    expect(screen.getByText(/Waiting for signal/i)).toBeInTheDocument();
    expect(screen.getByTestId('pending-signal-form')).toBeInTheDocument();
    // Signal is pre-filled from the pending event, read-only.
    const signalInput = screen.getByTestId('pending-signal-signal') as HTMLInputElement;
    expect(signalInput.value).toBe('deploy-signal');
    expect(signalInput.readOnly).toBe(true);
    expect(screen.getByTestId('pending-signal-note')).toBeInTheDocument();
    expect(screen.getByTestId('pending-signal-submit')).toHaveTextContent('Deliver signal');
    // Secondary affordance for API users stays available alongside the form.
    expect(screen.getByText(/Resume via curl/i)).toBeInTheDocument();
  });

  test('submits the resume mutation with the signal and no payload when the note is empty', async () => {
    const mockResume = vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } });
    vi.mocked(useResumeMutation).mockReturnValue([
      mockResume,
      { isLoading: false, error: null, isSuccess: false },
    ] as any);
    renderCard();

    fireEvent.click(screen.getByTestId('pending-signal-submit'));

    await waitFor(() => {
      expect(mockResume).toHaveBeenCalledWith({
        runId: 'run-1',
        signal: 'deploy-signal',
        payload: undefined,
      });
    });
  });

  test('submits the resume mutation with the note mapped to payload.note', async () => {
    const mockResume = vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } });
    vi.mocked(useResumeMutation).mockReturnValue([
      mockResume,
      { isLoading: false, error: null, isSuccess: false },
    ] as any);
    renderCard();

    fireEvent.change(screen.getByTestId('pending-signal-note'), { target: { value: '  go ahead  ' } });
    fireEvent.click(screen.getByTestId('pending-signal-submit'));

    await waitFor(() => {
      expect(mockResume).toHaveBeenCalledWith({
        runId: 'run-1',
        signal: 'deploy-signal',
        payload: { note: 'go ahead' },
      });
    });
  });

  test('disables the form while delivering', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: true, error: null, isSuccess: false },
    ] as any);
    renderCard();

    expect(screen.getByTestId('pending-signal-submit')).toBeDisabled();
    expect(screen.getByTestId('pending-signal-submit')).toHaveTextContent('Delivering...');
    expect(screen.getByText(/Delivering deploy-signal/i)).toBeInTheDocument();
  });

  test('renders a success message on delivery', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: false, error: null, isSuccess: true },
    ] as any);
    renderCard();

    expect(screen.getByText(/Signal delivered — run resuming/i)).toBeInTheDocument();
    // The form and curl affordances make way for the outcome.
    expect(screen.queryByTestId('pending-signal-form')).not.toBeInTheDocument();
  });

  test('renders a stale failed_precondition as informational with a refresh affordance', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: false, error: { status: 'failed_precondition', data: 'run is not paused' }, isSuccess: false },
    ] as any);
    const { onRefresh } = renderCard();

    expect(screen.getByText(/Signal already satisfied/i)).toBeInTheDocument();
    expect(screen.queryByTestId('pending-signal-error')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pending-signal-form')).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId('pending-signal-refresh'));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  test('treats the other stale-view failed_precondition causes as informational too', () => {
    // ServerService/ResumeRun's stale-view messages: the run moved past this
    // wait (pause gone, or the pending signal changed), so the operator's view
    // is outdated and the card offers the informational state, not an error.
    const staleCauses = [
      'run has no pending signal',
      "signal does not match the run's pending signal",
    ];
    for (const data of staleCauses) {
      vi.mocked(useResumeMutation).mockReturnValue([
        vi.fn(),
        { isLoading: false, error: { status: 'failed_precondition', data }, isSuccess: false },
      ] as any);
      const { unmount } = renderCard();

      expect(screen.getByText(/Signal already satisfied/i)).toBeInTheDocument();
      expect(screen.queryByTestId('pending-signal-error')).not.toBeInTheDocument();
      unmount();
    }
  });

  test('renders failed_precondition delivery failures as errors, not as a satisfied signal', () => {
    // failed_precondition also covers causes where the signal was NOT delivered:
    // the agent is offline or its control backlog is full (the run stays paused
    // awaiting input), and a terminal run can never resume. These must stay
    // error states so the operator knows their input is still needed.
    const nonStaleCauses = [
      'criteria agent not connected',
      'control backlog full',
      'run is terminal',
    ];
    for (const data of nonStaleCauses) {
      vi.mocked(useResumeMutation).mockReturnValue([
        vi.fn(),
        { isLoading: false, error: { status: 'failed_precondition', data }, isSuccess: false },
      ] as any);
      const { unmount } = renderCard();

      expect(screen.getByTestId('pending-signal-error')).toHaveTextContent(`✗ Error: ${data}`);
      expect(screen.queryByTestId('pending-signal-stale')).not.toBeInTheDocument();
      unmount();
    }
  });

  test('renders other errors as an error message', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: false, error: { status: 'permission_denied', data: 'Access denied' }, isSuccess: false },
    ] as any);
    renderCard();

    expect(screen.getByText(/✗ Error: Access denied/i)).toBeInTheDocument();
    expect(screen.queryByTestId('pending-signal-stale')).not.toBeInTheDocument();
  });

  test('keeps the delivery form visible but disabled with the capability tooltip when the host has no control RPC', () => {
    const mockResume = vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } });
    vi.mocked(useResumeMutation).mockReturnValue([
      mockResume,
      { isLoading: false, error: null, isSuccess: false },
    ] as any);
    renderCard({ capabilities: NO_CONTROL_CAPABILITIES });

    // Grayed-out is the contract, not hidden: the form stays present.
    expect(screen.getByTestId('pending-signal-form')).toBeInTheDocument();
    const submit = screen.getByTestId('pending-signal-submit');
    expect(submit).toBeDisabled();
    expect(submit).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    expect(screen.getByTestId('pending-signal-note')).toBeDisabled();
    // A disabled form never reaches the resume mutation.
    fireEvent.click(submit);
    expect(mockResume).not.toHaveBeenCalled();
  });

  test('leaves the deliver action enabled for the castle host (default capabilities)', () => {
    renderCard();

    const submit = screen.getByTestId('pending-signal-submit');
    expect(submit).toBeEnabled();
    expect(submit).not.toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    expect(screen.getByTestId('pending-signal-note')).toBeEnabled();
  });
});