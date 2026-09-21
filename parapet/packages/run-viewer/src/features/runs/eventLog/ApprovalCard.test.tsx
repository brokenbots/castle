import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Provider } from 'react-redux';
import { describe, expect, test, vi } from 'vitest';
import { ApprovalCard } from './ApprovalCard';
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
});
