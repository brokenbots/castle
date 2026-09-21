import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Provider } from 'react-redux';
import { describe, expect, test, vi } from 'vitest';
import { ApprovalCard } from './ApprovalCard';
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

describe('ApprovalCard', () => {
  test('renders approvers and reason', () => {
    render(
      <Provider store={store}>
        <ApprovalCard
          node="deploy"
          runId="run-1"
          approvers={['alice', 'bob']}
          reason="Deploy to production"
        />
      </Provider>,
    );

    expect(screen.getByText(/Approval Required/i)).toBeInTheDocument();
    expect(screen.getByText(/Deploy to production/i)).toBeInTheDocument();
    expect(screen.getByText(/alice, bob/i)).toBeInTheDocument();
  });

  test('calls Resume with approved decision on Approve click', async () => {
    const mockResume = vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } });
    vi.mocked(useResumeMutation).mockReturnValue([
      mockResume,
      { isLoading: false, error: null, isSuccess: false },
    ] as any);

    render(
      <Provider store={store}>
        <ApprovalCard node="deploy" runId="run-1" approvers={[]} reason="" />
      </Provider>,
    );

    const approveButton = screen.getByRole('button', { name: /Approve/i });
    fireEvent.click(approveButton);

    await waitFor(() => {
      expect(mockResume).toHaveBeenCalledWith({
        runId: 'run-1',
        signal: 'deploy',
        payload: { decision: 'approved' },
      });
    });
  });

  test('calls Resume with rejected decision on Reject click', async () => {
    const mockResume = vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } });
    vi.mocked(useResumeMutation).mockReturnValue([
      mockResume,
      { isLoading: false, error: null, isSuccess: false },
    ] as any);

    render(
      <Provider store={store}>
        <ApprovalCard node="deploy" runId="run-1" approvers={[]} reason="" />
      </Provider>,
    );

    const rejectButton = screen.getByRole('button', { name: /Reject/i });
    fireEvent.click(rejectButton);

    await waitFor(() => {
      expect(mockResume).toHaveBeenCalledWith({
        runId: 'run-1',
        signal: 'deploy',
        payload: { decision: 'rejected' },
      });
    });
  });

  test('disables buttons while loading', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: true, error: null, isSuccess: false },
    ] as any);

    render(
      <Provider store={store}>
        <ApprovalCard node="deploy" runId="run-1" approvers={[]} reason="" />
      </Provider>,
    );

    const approveButton = screen.getByRole('button', { name: /Approve/i });
    expect(approveButton).toBeDisabled();
    const rejectButton = screen.getByRole('button', { name: /Reject/i });
    expect(rejectButton).toBeDisabled();
  });

  test('renders success message on success', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: false, error: null, isSuccess: true },
    ] as any);

    render(
      <Provider store={store}>
        <ApprovalCard node="deploy" runId="run-1" approvers={[]} reason="" />
      </Provider>,
    );

    expect(screen.getByText(/run resuming/i)).toBeInTheDocument();
  });

  test('renders error message on failure', () => {
    vi.mocked(useResumeMutation).mockReturnValue([
      vi.fn(),
      { isLoading: false, error: { status: 'PERMISSION_DENIED', data: 'Access denied' }, isSuccess: false },
    ] as any);

    render(
      <Provider store={store}>
        <ApprovalCard node="deploy" runId="run-1" approvers={[]} reason="" />
      </Provider>,
    );

    expect(screen.getByText(/Error:/i)).toBeInTheDocument();
  });

  test('keeps Approve and Reject visible but disabled with the capability tooltip when the host has no control RPC', () => {
    const mockResume = vi.fn().mockResolvedValue({ data: { accepted: true, reason: 'ok' } });
    vi.mocked(useResumeMutation).mockReturnValue([
      mockResume,
      { isLoading: false, error: null, isSuccess: false },
    ] as any);

    render(
      <Provider store={store}>
        <ApprovalCard
          node="deploy"
          runId="run-1"
          approvers={[]}
          reason=""
          capabilities={NO_CONTROL_CAPABILITIES}
        />
      </Provider>,
    );

    // Grayed-out is the contract, not hidden: both actions stay present.
    for (const name of [/Approve/i, /Reject/i]) {
      const button = screen.getByRole('button', { name });
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    }
    // A disabled action never reaches the resume mutation.
    fireEvent.click(screen.getByRole('button', { name: /Approve/i }));
    expect(mockResume).not.toHaveBeenCalled();
  });

  test('keeps Approve and Reject enabled for the castle host (default capabilities)', () => {
    render(
      <Provider store={store}>
        <ApprovalCard node="deploy" runId="run-1" approvers={[]} reason="" />
      </Provider>,
    );

    const approve = screen.getByRole('button', { name: /Approve/i });
    const reject = screen.getByRole('button', { name: /Reject/i });
    expect(approve).toBeEnabled();
    expect(reject).toBeEnabled();
    expect(approve).not.toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    expect(reject).not.toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
  });
});
