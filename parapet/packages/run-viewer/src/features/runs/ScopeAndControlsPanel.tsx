import { useState } from 'react';
import type { EventEnvelope } from '../../api/castleApi';
import { DetailPanel } from '../../components/DetailPanel';
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
  /**
   * When the host page renders RunControls elsewhere (e.g. the static run
   * detail header), the panel renders the pause affordance and scope only —
   * duplicating the control row would put two copies of Pause/Resume/Stop
   * on the page.
   */
  showControls?: boolean;
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
  showControls = true,
}: ScopeAndControlsPanelProps) {
  const [expanded, setExpanded] = useState(true);
  return (
    <DetailPanel
      id="scope-controls-panel"
      idToggle="scope-controls-collapse"
      title="Scope & controls"
      collapsed={!expanded}
      onToggle={() => setExpanded((v) => !v)}
      bodyIdOverride="scope-controls-body"
      collapsedTestId="scope-controls-collapsed"
      collapsedNote="Scope & controls collapsed."
    >
      <div className="space-y-3">
        {showControls && (
          <div data-testid="scope-controls-row">
            <RunControls runId={runId} status={status} pauseState={pauseState} capabilities={capabilities} />
          </div>
        )}
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
    </DetailPanel>
  );
}