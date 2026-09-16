import { useEffect, useRef, useState } from 'react';
import {
  usePauseRunMutation,
  useResumeMutation,
  useStopRunMutation,
  type EventEnvelope,
} from '../../api/castleApi';
import { RUN_TERMINAL_STATUSES } from './runStatus';

interface RunControlsProps {
  runId: string;
  status: string;
  pauseState: { isPaused: boolean; pauseEvent: EventEnvelope | null };
}

type ControlError = { status: string; data: string } | undefined;

// toError() in castleApi maps ConnectErrors onto this shape; render the
// server's raw message plus the gRPC code so failures like an unattached
// agent (FAILED_PRECONDITION) are readable instead of silent.
function describeError(action: string, err: ControlError): string {
  if (!err) return `${action}.`;
  const detail = err.data ? `${err.data} (${err.status})` : err.status;
  return `${action}: ${detail}`;
}

export function RunControls({ runId, status, pauseState }: RunControlsProps) {
  const [pause, pauseMeta] = usePauseRunMutation();
  const [resume, resumeMeta] = useResumeMutation();
  const [stop, stopMeta] = useStopRunMutation();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [stopRequestedAt, setStopRequestedAt] = useState<string | null>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);

  // Reset per-run feedback when the page switches to a different run.
  useEffect(() => {
    setConfirmOpen(false);
    setStopRequestedAt(null);
  }, [runId]);

  // The event stream is the authoritative pause signal (waits, approvals);
  // the run status can lag it, and a control-paused run reports "paused"
  // before any wait event lands, so accept either.
  const isPaused = pauseState.isPaused || status === 'paused';
  const terminal = RUN_TERMINAL_STATUSES.has(status);
  const busy = pauseMeta.isLoading || resumeMeta.isLoading || stopMeta.isLoading;

  const canPause = status === 'running' && !isPaused;
  const canResume = isPaused;
  const canStop =
    (status === 'running' || status === 'pending' || isPaused) &&
    !stopRequestedAt;

  const terminalTitle = `Run is ${status} — controls unavailable`;

  const closeConfirm = () => setConfirmOpen(false);

  const handleConfirmStop = async () => {
    const res = await stop({ runId });
    if (res.data?.issuedAt) setStopRequestedAt(res.data.issuedAt);
    // Close the dialog either way so an inline error under the controls
    // becomes visible (the mutation meta carries the error).
    closeConfirm();
  };

  useEffect(() => {
    if (!confirmOpen) return;
    cancelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closeConfirm();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [confirmOpen]);

  return (
    <div className="flex flex-col items-start gap-1">
      <div className="flex items-center gap-2">
        <button
          type="button"
          onClick={() => void pause({ runId })}
          disabled={!canPause || busy}
          title={
            terminal
              ? terminalTitle
              : busy
                ? 'A control action is in flight'
                : isPaused
                  ? 'Run is paused'
                  : status !== 'running'
                    ? 'Run has not started yet'
                    : 'Pause the run'
          }
          className="px-3 py-1.5 bg-slate-700 hover:bg-slate-600 disabled:bg-slate-800 disabled:text-slate-500 disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
        >
          {pauseMeta.isLoading ? 'Pausing…' : 'Pause'}
        </button>
        <button
          type="button"
          onClick={() => void resume({ runId })}
          disabled={!canResume || busy}
          title={
            terminal
              ? terminalTitle
              : busy
                ? 'A control action is in flight'
                : canResume
                  ? 'Resume the run'
                  : 'Run is not paused'
          }
          className="px-3 py-1.5 bg-emerald-700 hover:bg-emerald-600 disabled:bg-slate-800 disabled:text-slate-500 disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
        >
          {resumeMeta.isLoading ? 'Resuming…' : 'Resume'}
        </button>
        <button
          type="button"
          onClick={() => setConfirmOpen(true)}
          disabled={!canStop || busy}
          title={
            terminal
              ? terminalTitle
              : busy
                ? 'A control action is in flight'
                : stopRequestedAt
                  ? 'Stop already requested'
                  : canStop
                    ? 'Stop the run'
                    : 'Run has not started yet'
          }
          className="px-3 py-1.5 bg-rose-700 hover:bg-rose-600 disabled:bg-slate-800 disabled:text-slate-500 disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
        >
          Stop
        </button>
      </div>

      {stopRequestedAt && (
        <p className="text-xs text-amber-400" data-testid="stop-requested">
          Stop requested at {stopRequestedAt} — waiting for the run to cancel;
          status updates arrive via the event stream.
        </p>
      )}

      {pauseMeta.error && (
        <p className="text-xs text-rose-400" data-testid="pause-error">
          {describeError('Pause failed', pauseMeta.error as ControlError)}
        </p>
      )}
      {resumeMeta.error && (
        <p className="text-xs text-rose-400" data-testid="resume-error">
          {describeError('Resume failed', resumeMeta.error as ControlError)}
        </p>
      )}
      {stopMeta.error && (
        <p className="text-xs text-rose-400" data-testid="stop-error">
          {describeError('Stop failed', stopMeta.error as ControlError)}
        </p>
      )}

      {confirmOpen && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60"
          data-testid="stop-confirm-overlay"
        >
          <div
            role="dialog"
            aria-modal="true"
            aria-labelledby="stop-run-confirm-title"
            className="bg-slate-800 border border-rose-700/60 rounded-lg p-5 max-w-md w-full mx-4 shadow-xl"
          >
            <h4
              id="stop-run-confirm-title"
              className="text-lg font-semibold text-white"
            >
              Stop run?
            </h4>
            <p className="text-sm text-slate-300 mt-2">
              This cancels run <span className="font-mono">{runId}</span>. Any
              in-flight work is lost and the run cannot be resumed. The run may
              take a moment to observe the cancel.
            </p>
            <div className="mt-4 flex justify-end gap-2">
              <button
                ref={cancelRef}
                type="button"
                onClick={closeConfirm}
                className="px-3 py-1.5 bg-slate-700 hover:bg-slate-600 rounded text-sm font-semibold text-white"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={() => void handleConfirmStop()}
                disabled={stopMeta.isLoading}
                className="px-3 py-1.5 bg-rose-700 hover:bg-rose-600 disabled:bg-slate-600 disabled:cursor-not-allowed rounded text-sm font-semibold text-white"
              >
                {stopMeta.isLoading ? 'Stopping…' : 'Stop run'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
