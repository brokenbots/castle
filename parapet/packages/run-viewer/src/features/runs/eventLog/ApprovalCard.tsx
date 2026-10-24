import { useState } from 'react';
import { useResumeMutation } from '../../../api/castleApi';
import { NO_CONTROLS_TOOLTIP, hasControls, type RunCapabilities } from '../capabilities';

interface ApprovalCardProps {
  node: string;
  runId: string;
  approvers: string[];
  reason: string;
  /**
   * Capability probe result (CRI-186 guards matrix extension). Without a
   * control RPC the Approve/Reject actions stay visible but disabled with
   * the capability tooltip — grayed-out, not hidden.
   */
  capabilities?: RunCapabilities;
}

export function ApprovalCard({ node, runId, approvers, reason, capabilities }: ApprovalCardProps) {
  const [resume, { isLoading, error, isSuccess }] = useResumeMutation();
  const [decision, setDecision] = useState<string | null>(null);
  // Capability axis dominates the action matrix; the castle default keeps
  // the buttons enabled per the existing status matrix.
  const noControls = !hasControls(capabilities);

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
    <div className="bg-surface rounded px-4 py-3 border border-accent-border/60 text-ink">
      <p className="text-sm font-semibold text-accent-strong mb-1">Approval Required</p>
      {reason && (
        <p className="text-sm text-ink-muted mb-2">
          <span className="font-semibold">Reason:</span> {reason}
        </p>
      )}
      {approvers.length > 0 && (
        <p className="text-xs text-ink-muted mb-3">
          <span className="font-semibold">Approvers:</span> {approvers.join(', ')}
        </p>
      )}

      {!isSuccess && !error && (
        <div className="flex gap-2">
          <button
            onClick={handleApprove}
            disabled={isLoading || noControls}
            title={noControls ? NO_CONTROLS_TOOLTIP : undefined}
            className="px-4 py-2 bg-success/80 hover:bg-success disabled:bg-surface-raised disabled:cursor-not-allowed rounded text-sm font-semibold text-canvas"
          >
            {isLoading && decision === 'approved' ? 'Approving…' : 'Approve'}
          </button>
          <button
            onClick={handleReject}
            disabled={isLoading || noControls}
            title={noControls ? NO_CONTROLS_TOOLTIP : undefined}
            className="px-4 py-2 bg-danger hover:bg-danger/80 disabled:bg-surface-raised disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
          >
            {isLoading && decision === 'rejected' ? 'Rejecting…' : 'Reject'}
          </button>
        </div>
      )}

      {isLoading && (
        <p className="text-xs text-ink-muted mt-2">Submitting {decision}…</p>
      )}

      {isSuccess && (
        <p className="text-sm text-success mt-2">
          ✓ {decision === 'approved' ? 'Approved' : 'Rejected'} — run resuming
        </p>
      )}

      {error && (
        <p className="text-sm text-danger mt-2">
          ✗ Error: {error && typeof error === 'object' && 'data' in error ? String(error.data) : 'Failed to resume'}
        </p>
      )}
    </div>
  );
}
