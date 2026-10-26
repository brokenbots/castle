// Shared visual vocabulary for run and agent status, built on the semantic
// design tokens (canvas/surface, ink, accent, success, warning, danger — see
// the host's index.css). Every status-bearing surface — list cells, the
// detail pill, graph node overlays, agent tables — reads these maps so a
// status looks the same everywhere.
//
// Status semantics: running/paused are transitional activity (warning amber),
// succeeded is healthy (success emerald), failed is danger (rose), stopped is
// parked/attention (violet), cancelled/pending are muted ink.

// Terminal run statuses, mirroring castle/internal/rpc/castle.go
// isTerminalRunStatus. A run in one of these states never transitions again,
// so the run list stops polling once every loaded run is terminal. `stopped`
// (CRI-207) is a first-class resumable state, not a terminal one — it is
// deliberately absent from RUN_TERMINAL_STATUSES.
export const RUN_TERMINAL_STATUSES = new Set(['succeeded', 'failed', 'cancelled']);

// The run-status values the project console's status filter offers: the full
// status vocabulary castle stores (castle/internal/store/store.go RunStatus*,
// including the awaiting_human human gate from KB-226), in presentation
// order, with the leading '' meaning "no status filter". The shipped
// run-list page keeps its own narrower STATUS_FILTERS (RunListPage.tsx);
// this list exists so console panels cannot drift from what castle's
// exact-match column filter can actually return.
export const RUN_STATUS_FILTER_VALUES = [
  '',
  'running',
  'pending',
  'paused',
  'succeeded',
  'failed',
  'stopped',
  'cancelled',
  'awaiting_human',
] as const;

// Compact tinted chip (used in tables and next to titles): a translucent
// status-colored fill with matching text.
export const RUN_STATUS_CHIP_CLASSES: Record<string, string> = {
  running: 'bg-warning/15 text-warning',
  succeeded: 'bg-success/15 text-success',
  failed: 'bg-danger/15 text-danger',
  pending: 'bg-surface-raised text-ink-muted',
  paused: 'bg-warning/15 text-warning',
  cancelled: 'bg-surface-raised text-ink-faint',
  stopped: 'bg-violet-500/15 text-violet-300',
};

// Plain text-color variant (table cells, dense rows). Kept exported under
// the legacy name — hosts already consume it.
export const RUN_STATUS_TEXT_COLORS: Record<string, string> = {
  running: 'text-warning',
  succeeded: 'text-success',
  failed: 'text-danger',
  pending: 'text-ink-muted',
  paused: 'text-warning',
  cancelled: 'text-ink-faint',
  stopped: 'text-violet-400',
};

// Saturated pill for the run-detail header: solid status-colored background
// with readable foreground, matching the previous pill look.
export const RUN_STATUS_PILL_CLASSES: Record<string, string> = {
  running: 'bg-warning text-canvas',
  succeeded: 'bg-success text-canvas',
  failed: 'bg-danger text-white',
  pending: 'bg-surface-raised text-ink',
  paused: 'bg-warning text-canvas',
  cancelled: 'bg-surface-raised text-ink-muted',
  stopped: 'bg-violet-600 text-white',
};

// Workflow-graph node borders by step status. Unvisited nodes stay fully
// opaque (the fill must block edges passing underneath); idle reads dimmer
// through border + text tones instead of node-level opacity, which would
// bleed edges through the card.
export const STEP_NODE_STATUS_CLASSES: Record<string, string> = {
  idle: 'border-line-strong',
  running: 'border-accent-strong ring-2 ring-accent-strong/40 animate-pulse',
  succeeded: 'border-success/70',
  failed: 'border-danger',
};

// Agent reachability.
export const AGENT_STATUS_TEXT_COLORS: Record<string, string> = {
  online: 'text-success',
  offline: 'text-ink-faint',
};