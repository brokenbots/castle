import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Provider } from 'react-redux';
import { describe, expect, test, vi } from 'vitest';
import { PendingSignalCard } from './PendingSignalCard';
import { store } from '../../../store';

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

function renderCard(overrides: Partial<{ signal: string; runId: string; onRefresh: () => void }> = {}) {
  const props = {
    signal: 'deploy-signal',
    runId: 'run-1',
    onRefresh: vi.fn(),
    ...overrides,
  };
  render(
    <Provider store={store}>
      <PendingSignalCard signal={props.signal} runId={props.runId} onRefresh={props.onRefresh} />
    </Provider>,
  );
  return props;
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

  test('renders other errors as an error message', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: false, error: { status: 'permission_denied', data: 'Access denied' }, isSuccess: false },
    ] as any);
    renderCard();

    expect(screen.getByText(/✗ Error: Access denied/i)).toBeInTheDocument();
    expect(screen.queryByTestId('pending-signal-stale')).not.toBeInTheDocument();
  });
});