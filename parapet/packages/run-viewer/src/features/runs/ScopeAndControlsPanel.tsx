import { useState } from 'react';
import type { EventEnvelope } from '../../api/castleApi';
import { RunControls } from './RunControls';
import { PauseAffordance } from './eventLog/PauseAffordance';
import { RunScopePanel } from './scopePanel/RunScopePanel';
import { CASTLE_RUN_CAPABILITIES, type RunCapabilities } from './capabilities';

interface ScopeAndControlsPanelProps {
  runId: string;
  status: string;
  pauseState: { isPaused: boolean; pauseEvent: EventEnvelope | null };
  events: EventEnvelope[];
  /** Re-fetches the run and re-anchors the event log (used by pending signals). */
  onRefresh: () => void;
  /**
   * Capability probe result; defaults to the castle host where the control
   * RPCs exist. Hosts without them render the controls grayed-out.
   */
  capabilities?: RunCapabilities;
}

/**
 * The run's scope & controls panel (CRI-257): one collapsible section that
 * gathers the control button row (pause/resume/stop, capability-gated),
 * the approval/signal affordance for the pending pause, and the run scope
 * view (variables and per-step outputs).
 */
export function ScopeAndControlsPanel({
  runId,
  status,
  pauseState,
  events,
  onRefresh,
  capabilities = CASTLE_RUN_CAPABILITIES,
}: ScopeAndControlsPanelProps) {
  const [expanded, setExpanded] = useState(true);
  return (
    <section data-testid="scope-controls-panel">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-lg font-semibold">Scope &amp; controls</h3>
        <button
          type="button"
          data-testid="scope-controls-collapse"
          aria-expanded={expanded}
          aria-controls="scope-controls-body"
          onClick={() => setExpanded((v) => !v)}
          className="rounded-md border border-line-strong px-2 py-1 text-xs text-ink-muted hover:bg-surface-raised hover:text-ink"
        >
          {expanded ? 'Collapse' : 'Expand'}
        </button>
      </div>
      {expanded ? (
        <div id="scope-controls-body" data-testid="scope-controls-body" className="space-y-3">
          <div data-testid="scope-controls-row">
            <RunControls runId={runId} status={status} pauseState={pauseState} capabilities={capabilities} />
          </div>
          {pauseState.isPaused && pauseState.pauseEvent && (
            <PauseAffordance
              runId={runId}
              pauseEvent={pauseState.pauseEvent}
              onRefresh={onRefresh}
              capabilities={capabilities}
            />
          )}
          <RunScopePanel events={events} />
        </div>
      ) : (
        <p className="text-sm text-ink-muted" data-testid="scope-controls-collapsed">
          Scope &amp; controls collapsed.
        </p>
      )}
    </section>
  );
}