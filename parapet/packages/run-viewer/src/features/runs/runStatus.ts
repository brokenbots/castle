// Terminal run statuses, mirroring castle/internal/rpc/castle.go
// isTerminalRunStatus. A run in one of these states never transitions again,
// so the run list stops polling once every loaded run is terminal.
export const RUN_TERMINAL_STATUSES = new Set(['succeeded', 'failed', 'cancelled']);

// Text-color palette shared by run status cells (StatusPill uses the same
// background classes). `paused` is a real status surfaced by RunControls.
export const RUN_STATUS_TEXT_COLORS: Record<string, string> = {
  running: 'text-amber-400',
  succeeded: 'text-emerald-400',
  failed: 'text-rose-400',
  pending: 'text-slate-400',
  paused: 'text-amber-400',
  cancelled: 'text-slate-500',
};
