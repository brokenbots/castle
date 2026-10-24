import { useState } from 'react';
import type { FormEvent } from 'react';
import { useResumeMutation } from '../../../api/castleApi';
import { copyTextToClipboard } from './clipboard';
import { NO_CONTROLS_TOOLTIP, hasControls, type RunCapabilities } from '../capabilities';

interface PendingSignalCardProps {
  signal: string;
  runId: string;
  /** Re-fetches the run and re-anchors the event log; offered from the stale-signal state. */
  onRefresh: () => void;
  /**
   * Capability probe result (CRI-186 guards matrix extension). Without a
   * control RPC the delivery form stays visible but disabled with the
   * capability tooltip — grayed-out, not hidden.
   */
  capabilities?: RunCapabilities;
}

// ResumeRun failures surface as RTK errors shaped { status: connectCodeName, data: message }.
// failed_precondition covers several causes. Only the stale-view ones — the signal is
// no longer pending because the run already moved past this wait — are informational.
// Delivery failures (agent offline, control backlog full) and other mismatches
// (e.g. a terminal run) must surface as errors consistent with ApprovalCard.
const STALE_SIGNAL_CAUSES = new Set([
  'run is not paused',
  'run has no pending signal',
  "signal does not match the run's pending signal",
]);

function isStaleSignalError(error: unknown): boolean {
  if (error === null || typeof error !== 'object') return false;
  const { status, data } = error as { status?: unknown; data?: unknown };
  return status === 'failed_precondition' && typeof data === 'string' && STALE_SIGNAL_CAUSES.has(data);
}

export function PendingSignalCard({ signal, runId, onRefresh, capabilities }: PendingSignalCardProps) {
  const [resume, { isLoading, error, isSuccess }] = useResumeMutation();
  const [note, setNote] = useState('');
  // Capability axis dominates the action matrix; the castle default keeps
  // the form enabled per the existing status matrix.
  const noControls = !hasControls(capabilities);

  const curlExample = `curl -X POST http://localhost:8080/criteria.v1.CriteriaService/Resume \\
  -H "Content-Type: application/json" \\
  -d '{"run_id":"${runId}","signal":"${signal}"}'`;

  const handleCopy = () => {
    void copyTextToClipboard(curlExample);
  };

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (noControls) return;
    const trimmed = note.trim();
    await resume({
      runId,
      signal,
      payload: trimmed ? { note: trimmed } : undefined,
    });
  };

  return (
    <div className="bg-surface rounded px-4 py-3 border border-warning/40 text-ink" data-testid="pending-signal-card">
      <p className="text-sm text-ink-muted mb-2">
        <span className="text-warning font-semibold">Waiting for signal</span>: <span className="font-mono">{signal}</span>
      </p>

      {isStaleSignalError(error) ? (
        <div data-testid="pending-signal-stale">
          <p className="text-sm text-success font-semibold">Signal already satisfied</p>
          <p className="text-xs text-ink-muted mt-1">
            The signal is no longer pending — the run already moved past this wait.
          </p>
          <button
            onClick={onRefresh}
            data-testid="pending-signal-refresh"
            className="mt-2 px-3 py-1.5 bg-surface-raised hover:bg-line-strong rounded text-xs font-semibold text-ink"
          >
            Refresh
          </button>
        </div>
      ) : isSuccess ? (
        <p className="text-sm text-success mt-2" data-testid="pending-signal-success">
          ✓ Signal delivered — run resuming
        </p>
      ) : error ? (
        <p className="text-sm text-danger mt-2" data-testid="pending-signal-error">
          ✗ Error: {error && typeof error === 'object' && 'data' in error ? String(error.data) : 'Failed to deliver signal'}
        </p>
      ) : (
        <form onSubmit={handleSubmit} data-testid="pending-signal-form" className="mt-3">
          <label className="block text-xs text-ink-muted mb-1" htmlFor="pending-signal-signal">
            Signal
          </label>
          <input
            id="pending-signal-signal"
            data-testid="pending-signal-signal"
            type="text"
            value={signal}
            readOnly
            disabled={isLoading || noControls}
            className="w-full bg-canvas border border-line-strong rounded px-2 py-1.5 text-sm font-mono text-ink mb-2"
          />
          <label className="block text-xs text-ink-muted mb-1" htmlFor="pending-signal-note">
            Note (optional)
          </label>
          <input
            id="pending-signal-note"
            data-testid="pending-signal-note"
            type="text"
            value={note}
            onChange={(event) => setNote(event.target.value)}
            disabled={isLoading || noControls}
            placeholder="Optional note delivered with the signal"
            className="w-full bg-canvas border border-line-strong rounded px-2 py-1.5 text-sm text-ink"
          />
          <button
            type="submit"
            disabled={isLoading || noControls}
            title={noControls ? NO_CONTROLS_TOOLTIP : undefined}
            data-testid="pending-signal-submit"
            className="mt-2 px-4 py-2 bg-warning hover:bg-warning/80 disabled:bg-surface-raised disabled:cursor-not-allowed rounded text-sm font-semibold text-canvas"
          >
            {isLoading ? 'Delivering…' : 'Deliver signal'}
          </button>
          {isLoading && (
            <p className="text-xs text-ink-muted mt-2">Delivering {signal}…</p>
          )}

          <details className="text-xs text-ink-muted mt-3">
            <summary className="cursor-pointer hover:text-ink">Resume via curl</summary>
            <div className="mt-2 relative">
              <pre className="bg-canvas rounded p-2 overflow-x-auto text-xs">{curlExample}</pre>
              <button
                type="button"
                onClick={handleCopy}
                className="absolute top-2 right-2 px-2 py-1 bg-surface-raised hover:bg-line-strong rounded text-xs text-ink"
              >
                Copy
              </button>
            </div>
          </details>
        </form>
      )}
    </div>
  );
}
