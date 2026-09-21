import { useState } from 'react';
import { useResumeMutation } from '../../../api/castleApi';

interface ApprovalCardProps {
  node: string;
  runId: string;
  approvers: string[];
  reason: string;
}

export function ApprovalCard({ node, runId, approvers, reason }: ApprovalCardProps) {
  const [resume, { isLoading, error, isSuccess }] = useResumeMutation();
  const [decision, setDecision] = useState<string | null>(null);

  const handleApprove = async () => {
    setDecision('approved');
    await resume({
      runId,
      signal: node,
      payload: { decision: 'approved' },
    });
  };

  const handleReject = async () => {
    setDecision('rejected');
    await resume({
      runId,
      signal: node,
      payload: { decision: 'rejected' },
    });
  };

  return (
    <div className="bg-slate-800 rounded px-4 py-3 border border-purple-700/50">
      <p className="text-sm font-semibold text-purple-400 mb-1">Approval Required</p>
      {reason && (
        <p className="text-sm text-slate-300 mb-2">
          <span className="font-semibold">Reason:</span> {reason}
        </p>
      )}
      {approvers.length > 0 && (
        <p className="text-xs text-slate-400 mb-3">
          <span className="font-semibold">Approvers:</span> {approvers.join(', ')}
        </p>
      )}

      {!isSuccess && !error && (
        <div className="flex gap-2">
          <button
            onClick={handleApprove}
            disabled={isLoading}
            className="px-4 py-2 bg-green-600 hover:bg-green-700 disabled:bg-slate-600 disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
          >
            {isLoading && decision === 'approved' ? 'Approving...' : 'Approve'}
          </button>
          <button
            onClick={handleReject}
            disabled={isLoading}
            className="px-4 py-2 bg-rose-600 hover:bg-rose-700 disabled:bg-slate-600 disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
          >
            {isLoading && decision === 'rejected' ? 'Rejecting...' : 'Reject'}
          </button>
        </div>
      )}

      {isLoading && (
        <p className="text-xs text-slate-400 mt-2">Submitting {decision}...</p>
      )}

      {isSuccess && (
        <p className="text-sm text-green-400 mt-2">
          ✓ {decision === 'approved' ? 'Approved' : 'Rejected'} — run resuming
        </p>
      )}

      {error && (
        <p className="text-sm text-rose-400 mt-2">
          ✗ Error: {error && typeof error === 'object' && 'data' in error ? String(error.data) : 'Failed to resume'}
        </p>
      )}
    </div>
  );
}
