import { useState } from 'react';
import type { FormEvent } from 'react';
import { useResumeMutation } from '../../../api/castleApi';
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
    navigator.clipboard.writeText(curlExample);
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
    <div className="bg-slate-800 rounded px-4 py-3 border border-amber-700/50" data-testid="pending-signal-card">
      <p className="text-sm text-slate-300 mb-2">
        <span className="text-amber-400 font-semibold">Waiting for signal</span>: <span className="font-mono">{signal}</span>
      </p>

      {isStaleSignalError(error) ? (
        <div data-testid="pending-signal-stale">
          <p className="text-sm text-emerald-400 font-semibold">Signal already satisfied</p>
          <p className="text-xs text-slate-400 mt-1">
            The signal is no longer pending — the run already moved past this wait.
          </p>
          <button
            onClick={onRefresh}
            data-testid="pending-signal-refresh"
            className="mt-2 px-3 py-1.5 bg-slate-700 hover:bg-slate-600 rounded text-xs font-semibold text-white"
          >
            Refresh
          </button>
        </div>
      ) : isSuccess ? (
        <p className="text-sm text-green-400 mt-2" data-testid="pending-signal-success">
          ✓ Signal delivered — run resuming
        </p>
      ) : error ? (
        <p className="text-sm text-rose-400 mt-2" data-testid="pending-signal-error">
          ✗ Error: {error && typeof error === 'object' && 'data' in error ? String(error.data) : 'Failed to deliver signal'}
        </p>
      ) : (
        <form onSubmit={handleSubmit} data-testid="pending-signal-form" className="mt-3">
          <label className="block text-xs text-slate-400 mb-1" htmlFor="pending-signal-signal">
            Signal
          </label>
          <input
            id="pending-signal-signal"
            data-testid="pending-signal-signal"
            type="text"
            value={signal}
            readOnly
            disabled={isLoading || noControls}
            className="w-full bg-slate-900 border border-slate-700 rounded px-2 py-1.5 text-sm font-mono text-slate-200 mb-2"
          />
          <label className="block text-xs text-slate-400 mb-1" htmlFor="pending-signal-note">
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
            className="w-full bg-slate-900 border border-slate-700 rounded px-2 py-1.5 text-sm text-slate-200"
          />
          <button
            type="submit"
            disabled={isLoading || noControls}
            title={noControls ? NO_CONTROLS_TOOLTIP : undefined}
            data-testid="pending-signal-submit"
            className="mt-2 px-4 py-2 bg-amber-600 hover:bg-amber-700 disabled:bg-slate-600 disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
          >
            {isLoading ? 'Delivering...' : 'Deliver signal'}
          </button>
          {isLoading && (
            <p className="text-xs text-slate-400 mt-2">Delivering {signal}...</p>
          )}

          <details className="text-xs text-slate-400 mt-3">
            <summary className="cursor-pointer hover:text-slate-300">Resume via curl</summary>
            <div className="mt-2 relative">
              <pre className="bg-slate-900 rounded p-2 overflow-x-auto text-xs">{curlExample}</pre>
              <button
                type="button"
                onClick={handleCopy}
                className="absolute top-2 right-2 px-2 py-1 bg-slate-700 hover:bg-slate-600 rounded text-xs"
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
