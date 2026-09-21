import { useMemo, useState, useEffect, useRef, type RefObject } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useSelector } from 'react-redux';
import { useGetRunQuery, type EventEnvelope } from '../../api/castleApi';
import { classifyError } from '../../api/errors';
import { selectPauseState } from './runsSlice';
import { useRunEventLog } from './eventLog/useRunEventLog';
import { EventLog } from './eventLog/EventLog';
import { StatusPill } from './StatusPill';
import { RunInspection } from './RunInspection';
import { ScopeAndControlsPanel } from './ScopeAndControlsPanel';
import { CASTLE_RUN_CAPABILITIES, type RunCapabilities } from './capabilities';
import { ForEachStrip } from './eventLog/ForEachStrip';
import { PageHeader } from '../../components/PageHeader';
import { PageState } from '../../components/PageState';
import { DockedPanel } from '../../components/DockedPanel';
import { Breadcrumbs } from '../../components/Breadcrumbs';
import { useDocumentTitle } from '../../shell/useDocumentTitle';
import { extractTextEdges, parseWorkflowHcl, type WorkflowGraph as WorkflowGraphSpec } from './workflowGraph/parseWorkflowHcl';
import { buildSubworkflowLayers, selectWorkflowGraphs, type SubworkflowLayer } from './workflowGraph/layers';
import { WorkflowLayerNav } from './workflowGraph/WorkflowLayerNav';
import { WorkflowGraph } from './workflowGraph/WorkflowGraph';
import { WorkflowSourceView } from './workflowGraph/WorkflowSourceView';
import type { GraphOrientation } from './workflowGraph/layout';
import { eventBelongsToStep, selectNodeOverlay } from './workflowGraph/nodeStatus';

/**
 * Parses the run's workflow HCL into the graph model. The `workflowHash` run
 * field carries the full workflow source (castle rpc mapping), so the graph
 * is derived entirely client-side. Any parse failure — or a source with no
 * parseable nodes — yields null and the page keeps the text-edge fallback,
 * so the panel never blanks.
 */
function parseGraph(source: string): WorkflowGraphSpec | null {
  if (!source) return null;
  try {
    const graph = parseWorkflowHcl(source);
    return graph.nodes.length > 0 ? graph : null;
  } catch {
    // Any failure (typed or not — e.g. a RangeError from pathologically
    // nested input) falls back; the page has no error boundary, so an
    // escaping throw would blank it.
    return null;
  }
}

type ExpandablePanel = 'events' | 'graph' | 'inspection';

// Fullscreen overlay for an expanded run-detail panel: a fixed layer filling
// the viewport above the page chrome on the opaque canvas. Expansion is
// state-local to this page and purely presentational — the wrapped panel
// components keep their docked markup and stay mounted, so the watch stream,
// log anchoring and scroll positions survive expand/collapse. The per-panel
// variants size the panel's own scroller to fill the overlay via arbitrary
// variants targeting the panel's inner testids.
//
// The events variants make the whole ancestor chain down to the log scroller
// definite-height (flex column at each level); percentage `h-full` computes
// to `auto` against any auto-height ancestor, which would leave the log
// content-sized and hand scrolling to the overlay instead of the log. This
// couples the variants to EventLog's internal DOM (root div → last-child
// relative wrapper → scroller); if EventLog's structure changes, the chain
// must be revisited.
const FULLSCREEN_PANEL_CLASSES =
  'fixed inset-0 z-50 flex flex-col overflow-y-auto bg-canvas p-4 sm:p-6';
const EVENTS_FULLSCREEN_CLASSES = `${FULLSCREEN_PANEL_CLASSES} [&_[data-testid=events-panel-body]]:flex [&_[data-testid=events-panel-body]]:flex-col [&_[data-testid=events-panel-body]]:flex-1 [&_[data-testid=events-panel-body]]:min-h-0 [&_[data-testid=events-panel-body]>div]:flex [&_[data-testid=events-panel-body]>div]:flex-col [&_[data-testid=events-panel-body]>div]:flex-1 [&_[data-testid=events-panel-body]>div]:min-h-0 [&_[data-testid=events-panel-body]>div>div:last-child]:flex-1 [&_[data-testid=events-panel-body]>div>div:last-child]:min-h-0 [&_[data-testid=event-log-scroll]]:h-full`;
const GRAPH_FULLSCREEN_CLASSES = `${FULLSCREEN_PANEL_CLASSES} [&>[data-testid=workflow-graph]]:flex-1 [&>[data-testid=workflow-graph]]:min-h-0`;
const INSPECTION_FULLSCREEN_CLASSES = `${FULLSCREEN_PANEL_CLASSES} [&_[data-testid=run-inspection]]:flex-1 [&_[data-testid=run-inspection]]:min-h-0 [&_[data-testid=run-inspection]]:overflow-y-auto`;
const PANEL_ICON_BUTTON_CLASSES =
  'shrink-0 rounded-md border border-line-strong p-1.5 text-ink-muted hover:bg-surface-raised hover:text-ink';

function panelButtonClass(expanded: boolean): string {
  return expanded
    // Pinned to the viewport so the collapse control stays reachable even
    // when the panel body scrolls.
    ? `${PANEL_ICON_BUTTON_CLASSES} fixed right-4 top-4 sm:right-6 sm:top-6`
    : `${PANEL_ICON_BUTTON_CLASSES} absolute right-0 top-0`;
}

interface PanelExpandButtonProps {
  title: string;
  testId: string;
  expanded: boolean;
  onToggle: () => void;
  buttonRef: RefObject<HTMLButtonElement>;
  className: string;
  controlsId: string;
}

// Single expand/collapse affordance per panel: a corner icon button toggling
// the panel's fullscreen overlay. aria-expanded/aria-controls plus the swap
// between the outward/inward arrow icons convey the state.
function PanelExpandButton({
  title,
  testId,
  expanded,
  onToggle,
  buttonRef,
  className,
  controlsId,
}: PanelExpandButtonProps) {
  return (
    <button
      ref={buttonRef}
      type="button"
      data-testid={testId}
      aria-expanded={expanded}
      aria-label={`${expanded ? 'Collapse' : 'Expand'} ${title} panel`}
      aria-controls={controlsId}
      title={expanded ? `${title} panel — collapse (Escape)` : `${title} panel — expand`}
      onClick={onToggle}
      className={className}
    >
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        className="h-4 w-4"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        {expanded ? (
          <path d="M10 14H4v6M4 14l7 7M14 10h6V4M14 10l7-7" />
        ) : (
          <path d="M15 3h6v6M21 3l-7 7M9 21H3v-6M3 21l7-7" />
        )}
      </svg>
    </button>
  );
}

export function RunDetailPage({
  capabilities = CASTLE_RUN_CAPABILITIES,
}: {
  /**
   * Capability probe result (CRI-186 guards matrix extension). The castle
   * host keeps the default; the standalone viewer passes a host without
   * control RPCs so the control row renders grayed-out with the tooltip.
   */
  capabilities?: RunCapabilities;
} = {}) {
  const { id = '' } = useParams();
  const run = useGetRunQuery(id);
  const { events, log, loadEarlier, watch, reconnect, refresh } = useRunEventLog(id);
  const pauseState = useSelector(selectPauseState(id));
  const [selectedStep, setSelectedStep] = useState<{ runId: string; step: string } | null>(null);
  // Which panel (if any) is currently expanded to a fullscreen overlay.
  // Null keeps every panel docked; toggling only swaps classes on the
  // wrapper sections, so panel components and their hooks stay mounted.
  const [expandedPanel, setExpandedPanel] = useState<ExpandablePanel | null>(null);
  // Graph reading direction (CRI-257); top-bottom preserves the original
  // rendering, left-right transposes the layered layout.
  const [graphOrientation, setGraphOrientation] = useState<GraphOrientation>('top-bottom');
  // Follow mode: keep the viewport centered on the running step as the
  // event stream advances (CRI-257). Drag-pan and wheel-zoom stay live.
  const [followMode, setFollowMode] = useState(false);
  // Source pane placement (right of the graph / under it) with collapse
  // state remembered per placement (CRI-257): collapsing the under view
  // does not force the side view collapsed, and vice versa.
  const [sourcePlacement, setSourcePlacement] = useState<'side' | 'under'>('under');
  const [sourceCollapsed, setSourceCollapsed] = useState<Record<'side' | 'under', boolean>>({
    side: false,
    under: false,
  });
  const toggleSourceCollapsed = () =>
    setSourceCollapsed((state) => ({ ...state, [sourcePlacement]: !state[sourcePlacement] }));
  const eventsExpandRef = useRef<HTMLButtonElement>(null);
  const graphExpandRef = useRef<HTMLButtonElement>(null);
  const inspectionExpandRef = useRef<HTMLButtonElement>(null);

  const togglePanel = (panel: ExpandablePanel) => {
    setExpandedPanel((current) => (current === panel ? null : panel));
  };

  // Escape leaves fullscreen; focus returns to the affordance that opened
  // the overlay so keyboard users are not dropped at an arbitrary page
  // position.
  useEffect(() => {
    if (!expandedPanel) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      setExpandedPanel(null);
      if (expandedPanel === 'events') eventsExpandRef.current?.focus();
      else if (expandedPanel === 'graph') graphExpandRef.current?.focus();
      else inspectionExpandRef.current?.focus();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [expandedPanel]);

  // The tab title reflects the current run's workflow name; while loading it
  // falls back to the base title.
  useDocumentTitle(run.data?.workflowName);

  const workflowSource = run.data?.workflowHash ?? '';
  const graph = useMemo(() => parseGraph(workflowSource), [workflowSource]);
  const fallbackEdges = useMemo(
    () => (graph ? [] : workflowSource ? extractTextEdges(workflowSource) : []),
    [graph, workflowSource],
  );
  // Subworkflow drill-down (CRI-257): workflow.graphs events carry the
  // compiled subworkflow layers of the run's workflow; steps whose target
  // crosses a subworkflow open their layer graph here. The stack is
  // guarded by the run id (same pattern as selectedStep), so switching
  // runs starts at the top-level workflow again.
  const [layerStack, setLayerStack] = useState<{ runId: string; names: string[] }>({
    runId: '',
    names: [],
  });
  const graphsPayload = useMemo(() => selectWorkflowGraphs(events), [events]);
  const layersByName = useMemo(() => {
    const map = new Map<string, SubworkflowLayer>();
    if (graphsPayload) {
      for (const layer of buildSubworkflowLayers(graphsPayload)) map.set(layer.name, layer);
    }
    return map;
  }, [graphsPayload]);
  const layerStackLayers = useMemo(() => {
    const names = layerStack.runId === (run.data?.runId ?? '') ? layerStack.names : [];
    // A stack entry whose layer graph is missing or unparsable drops out;
    // the breadcrumb shows only layers that can actually be rendered.
    return names
      .map((name) => layersByName.get(name))
      .filter((layer): layer is SubworkflowLayer => Boolean(layer?.graph));
  }, [layerStack, layersByName, run.data]);
  const topLayer = layerStackLayers[layerStackLayers.length - 1] ?? null;
  const currentGraph = topLayer?.graph ?? graph;
  // Subworkflow names whose graphs resolved — nodes carrying one of these
  // render an enabled explore affordance; the rest stay grayed out.
  const exploreableLayers = useMemo(() => {
    const names = new Set<string>();
    for (const layer of layersByName.values()) if (layer.graph) names.add(layer.name);
    return names;
  }, [layersByName]);
  // A subworkflow layer's source pane shows the layer's own module body
  // (carried by the workflow.graphs event), not the parent's source.
  const currentSource = topLayer?.body ?? workflowSource;
  const openLayer = (name: string) => {
    const layer = layersByName.get(name);
    if (!layer?.graph || !run.data) return;
    setLayerStack((state) => {
      const names = state.runId === run.data!.runId ? state.names : [];
      if (names[names.length - 1] === name) return state;
      return { runId: run.data!.runId, names: [...names, name] };
    });
    // Node ids can repeat across layers; drop the parent selection.
    setSelectedStep(null);
  };
  const navigateLayers = (depth: number) => {
    if (!run.data) return;
    setLayerStack({ runId: run.data.runId, names: stackNames(depth) });
    setSelectedStep(null);
  };
  // Names behind the navigation target: the current stack (run-guarded)
  // truncated to the requested depth.
  function stackNames(depth: number): string[] {
    const names = layerStack.runId === run.data!.runId ? layerStack.names : [];
    return names.slice(0, depth);
  }
  // The overlay maps the event stream onto graph nodes: running (pulsing)
  // for the active step, succeeded/failed for finished ones, per-iteration
  // for_each progress from ForEachStrip data, and unvisited nodes dimmed.
  const overlay = useMemo(
    () => (graph ? selectNodeOverlay(events) : { statuses: {}, forEach: {} }),
    [graph, events],
  );
  // The node the engine is currently inside (last stepEntered wins per
  // selectNodeOverlay) — the follow target when follow mode is on.
  const currentStepId = useMemo(() => {
    const running = Object.entries(overlay.statuses).find(([, status]) => status === 'running');
    return running?.[0] ?? null;
  }, [overlay]);
  const selected = selectedStep && selectedStep.runId === (run.data?.runId ?? '') ? selectedStep.step : null;
  const visibleEvents = useMemo(
    () => (selected ? events.filter((e) => eventBelongsToStep(e, selected)) : events),
    [events, selected],
  );
  // Node click drives source highlighting: the graph parser records each
  // node's exact declaration range, so the source pane highlights the block
  // (and scrolls to it) without re-scanning (CRI-257). Ranges index into
  // the source of the graph being viewed (root workflow or open layer).
  const selectedNode = useMemo(
    () => (currentGraph ? currentGraph.nodes.find((n) => n.id === selected) ?? null : null),
    [currentGraph, selected],
  );
  const highlightRange = selectedNode?.sourceRange ?? null;

  // Only render the PR link for http(s) URLs; the publisher controls the
  // value and must not be able to inject javascript: hrefs.
  const prUrl = run.data?.prUrl?.startsWith('http://') || run.data?.prUrl?.startsWith('https://') ? run.data.prUrl : undefined;

  // A run is live-tailing while its status is running and no terminal event
  // has arrived yet (the status can lag the event stream).
  const running = useMemo(() => {
    if (!run.data || run.data.status !== 'running') return false;
    return !events.some((e) => e.type === 'runCompleted' || e.type === 'runFailed');
  }, [run.data, events]);

  // Group events by for_each node
  const forEachNodes = useMemo(() => {
    const nodes = new Map<string, EventEnvelope[]>();
    for (const e of events) {
      if (e.type === 'forEachEntered' || e.type === 'stepIterationStarted' || e.type === 'stepIterationCompleted') {
        const payload = e.payload as Record<string, unknown> | undefined;
        const node = (payload?.node as string) ?? '';
        if (node) {
          if (!nodes.has(node)) nodes.set(node, []);
          nodes.get(node)!.push(e);
        }
      }
    }
    return nodes;
  }, [events]);

  if (run.isLoading) {
    return <PageState loading title="Loading run…" testId="run-detail-loading" />;
  }

  if (run.error || !run.data) {
    const kind = run.error ? classifyError(run.error) : 'unknown';
    // Auth failures prompt re-authentication (and flip the app gate) instead
    // of a dead-end failure; everything else distinguishes not-found from a
    // server failure and offers retry/back navigation.
    if (kind === 'unauthenticated') {
      return <PageState error kind="unauthenticated" />;
    }
    return (
      <PageState
        error
        kind={kind}
        title={kind === 'not_found' ? 'Run not found.' : 'Failed to load this run.'}
        detail={
          kind === 'not_found'
            ? "This run doesn't exist or was removed."
            : 'Castle is unreachable or failed to answer. Try again.'
        }
        onRetry={kind === 'not_found' ? undefined : () => void run.refetch()}
        action={
          <Link
            to="/runs"
            className="text-body text-ink-muted hover:text-ink hover:underline"
          >
            Back to runs
          </Link>
        }
      />
    );
  }

  return (
    <div className="flex h-full min-h-0 items-stretch gap-4" data-testid="run-detail-layout">
      <div className="flex min-w-0 flex-1 flex-col gap-4 overflow-y-auto pr-1">
        {/* Breadcrumb trail plus a back affordance: deep-linked runs exit to
            the run list without relying on browser back. */}
        <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-2">
          <Breadcrumbs
            items={[{ label: 'Runs', to: '/runs' }, { label: run.data.workflowName }]}
          />
          <Link
            to="/runs"
            data-testid="run-back"
            className="inline-flex shrink-0 items-center gap-1 text-body text-ink-muted hover:text-ink hover:underline"
          >
            <svg aria-hidden viewBox="0 0 16 16" className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth="1.5">
              <path d="M10 3 5 8l5 5" />
            </svg>
            Back to runs
          </Link>
        </div>
        <PageHeader
          title={run.data.workflowName}
          meta={run.data.runId}
          actions={
            <>
              <StatusPill status={run.data.status} pauseEvent={pauseState.pauseEvent} />
            </>
          }
        >
          <div className="flex flex-wrap items-center gap-4 text-body">
            {run.data.ticket && (
              <span>
                ticket: <span className="font-mono">{run.data.ticket}</span>
              </span>
            )}
            {run.data.repoUrl && (
              <span>
                repo: <span className="font-mono">{run.data.repoUrl}</span>
              </span>
            )}
            {run.data.finalState && (
              <span>
                final: <span className="font-mono">{run.data.finalState}</span>
              </span>
            )}
            {prUrl && (
              <a className="text-accent-strong hover:underline" href={prUrl} target="_blank" rel="noreferrer">
                PR
              </a>
            )}
          </div>
        </PageHeader>

        {/* watchRun liveness: surface stream loss to the user instead of
            only logging it. While reconnecting show the bounded-backoff
            progress; once retries are exhausted offer a manual reconnect. */}
        {watch && (watch.state === 'reconnecting' || watch.state === 'lost' || watch.state === 'unauthenticated') && (
          <div
            data-testid="stream-status"
            data-state={watch.state}
            role="status"
            className="flex flex-wrap items-center gap-3 rounded-md border border-line bg-surface px-4 py-2 text-body"
          >
            {watch.state === 'reconnecting' && (
              <span className="text-ink-muted" data-testid="stream-reconnecting">
                Live tail lost — reconnecting (attempt {watch.attempt}/{watch.maxAttempts})…
              </span>
            )}
            {watch.state === 'lost' && (
              <>
                <span className="text-danger">Live tail lost — reconnect attempts failed.</span>
                <button
                  type="button"
                  data-testid="stream-reconnect"
                  className="rounded-md border border-line-strong px-3 py-1.5 text-body text-ink hover:bg-surface-raised"
                  onClick={reconnect}
                >
                  Reconnect
                </button>
              </>
            )}
            {watch.state === 'unauthenticated' && (
              <span className="text-danger">Live tail stopped — sign in again to resume.</span>
            )}
          </div>
        )}

        <section
          id="inspection-panel"
          data-testid="inspection-panel"
          data-expanded={expandedPanel === 'inspection'}
          className={expandedPanel === 'inspection' ? INSPECTION_FULLSCREEN_CLASSES : 'relative'}
        >
          <RunInspection runId={id} status={run.data.status} />
          <PanelExpandButton
            title="Inspection"
            testId="inspection-panel-expand"
            expanded={expandedPanel === 'inspection'}
            onToggle={() => togglePanel('inspection')}
            buttonRef={inspectionExpandRef}
            className={panelButtonClass(expandedPanel === 'inspection')}
            controlsId="inspection-panel"
          />
        </section>

        {forEachNodes.size > 0 && (
          <section>
            {Array.from(forEachNodes.entries()).map(([node, nodeEvents]) => (
              <ForEachStrip key={node} runId={id} events={nodeEvents} />
            ))}
          </section>
        )}

        {selected && (
          <div className="flex items-center gap-2 text-xs" data-testid="step-filter">
            <span className="rounded bg-sky-950 px-2 py-1 text-sky-300 border border-sky-800 font-mono">
              Filtered to step: {selected}
            </span>
            <button
              type="button"
              data-testid="clear-step-filter"
              onClick={() => setSelectedStep(null)}
              className="text-slate-400 hover:text-slate-200"
            >
              Clear filter
            </button>
          </div>
        )}

        <section
          id="events-panel"
          data-testid="events-panel"
          data-expanded={expandedPanel === 'events'}
          className={expandedPanel === 'events' ? EVENTS_FULLSCREEN_CLASSES : 'relative'}
        >
          <h3 className="text-lg font-semibold mb-2">Events</h3>
          <div data-testid="events-panel-body">
            <EventLog
              events={visibleEvents}
              running={running}
              hasEarlier={log.hasEarlier}
              loadingEarlier={log.loadingEarlier}
              onLoadEarlier={loadEarlier}
            />
          </div>
          <PanelExpandButton
            title="Events"
            testId="events-panel-expand"
            expanded={expandedPanel === 'events'}
            onToggle={() => togglePanel('events')}
            buttonRef={eventsExpandRef}
            className={panelButtonClass(expandedPanel === 'events')}
            controlsId="events-panel"
          />
        </section>
        <section data-testid="source-pane">
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-lg font-semibold">Workflow source</h3>
            <div className="flex items-center gap-2">
              <div
                role="group"
                aria-label="Source pane placement"
                data-testid="source-placement-toggle"
                className="flex overflow-hidden rounded-md border border-line-strong"
              >
                <button
                  type="button"
                  data-testid="source-placement-side"
                  aria-pressed={sourcePlacement === 'side'}
                  title="Show the source beside the step graph"
                  onClick={() => setSourcePlacement('side')}
                  className={`px-2 py-1 text-xs ${sourcePlacement === 'side' ? 'bg-surface-raised text-ink' : 'text-ink-muted hover:bg-surface-raised'}`}
                >
                  Side
                </button>
                <button
                  type="button"
                  data-testid="source-placement-under"
                  aria-pressed={sourcePlacement === 'under'}
                  title="Show the source under the step graph"
                  onClick={() => setSourcePlacement('under')}
                  className={`px-2 py-1 text-xs ${sourcePlacement === 'under' ? 'bg-surface-raised text-ink' : 'text-ink-muted hover:bg-surface-raised'}`}
                >
                  Under
                </button>
              </div>
              <button
                type="button"
                data-testid="source-pane-collapse"
                aria-expanded={!sourceCollapsed[sourcePlacement]}
                aria-controls="source-pane-body"
                onClick={toggleSourceCollapsed}
                className="rounded-md border border-line-strong px-2 py-1 text-xs text-ink-muted hover:bg-surface-raised hover:text-ink"
              >
                {sourceCollapsed[sourcePlacement] ? 'Expand' : 'Collapse'}
              </button>
            </div>
          </div>
          {sourceCollapsed[sourcePlacement] ? (
            <p className="text-sm text-ink-muted" data-testid="source-pane-collapsed">
              Source pane collapsed.
            </p>
          ) : sourcePlacement === 'side' ? (
            <p className="text-sm text-ink-muted" data-testid="source-pane-side-note">
              Shown beside the step graph.
            </p>
          ) : (
            <div id="source-pane-body" data-testid="source-pane-body">
              <WorkflowSourceView source={currentSource} highlight={highlightRange} />
            </div>
          )}
        </section>
        <section
          id="graph-panel"
          data-testid="graph-panel"
          data-expanded={expandedPanel === 'graph'}
          className={expandedPanel === 'graph' ? GRAPH_FULLSCREEN_CLASSES : 'relative'}
        >
          <div className="flex items-center justify-between mb-2">
            <h3 className="text-lg font-semibold">Step graph</h3>
            <div className="flex items-center gap-2">
              <button
                type="button"
                data-testid="graph-follow-toggle"
                aria-pressed={followMode}
                title="Follow the running step"
                onClick={() => setFollowMode((v) => !v)}
                className={`px-2 py-1 text-xs rounded-md border ${followMode ? 'border-sky-400 bg-sky-400/10 text-sky-300' : 'border-line-strong text-ink-muted hover:bg-surface-raised'}`}
              >
                Follow
              </button>
              <div
                role="group"
                aria-label="Graph orientation"
                data-testid="graph-orientation-toggle"
                className="flex overflow-hidden rounded-md border border-line-strong"
              >
                {(['top-bottom', 'left-right'] as const).map((value) => (
                  <button
                    key={value}
                    type="button"
                    data-testid={`orientation-${value}`}
                    aria-pressed={graphOrientation === value}
                    title={`Flow ${value === 'top-bottom' ? 'top to bottom' : 'left to right'}`}
                    onClick={() => setGraphOrientation(value)}
                    className={`px-2 py-1 text-xs ${graphOrientation === value ? 'bg-surface-raised text-ink' : 'text-ink-muted hover:bg-surface-raised'}`}
                  >
                    <svg aria-hidden="true" viewBox="0 0 24 24" className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                      {value === 'top-bottom' ? <path d="M12 3v18M12 21l4-4M12 21l-4-4" /> : <path d="M3 12h18M21 12l-4-4M21 12l-4 4" />}
                    </svg>
                    <span className="sr-only">{value === 'top-bottom' ? 'Top to bottom' : 'Left to right'}</span>
                  </button>
                ))}
              </div>
            </div>
          </div>
          {layerStackLayers.length > 0 && (
            <div className="mb-2">
              <WorkflowLayerNav
                rootName={run.data?.workflowName || 'Workflow'}
                stack={layerStackLayers}
                onNavigate={navigateLayers}
              />
            </div>
          )}
          {currentGraph ? (
            <WorkflowGraph
              graph={currentGraph}
              statuses={overlay.statuses}
              forEachProgress={overlay.forEach}
              orientation={graphOrientation}
              followStepId={followMode ? currentStepId : null}
              selectedId={selected}
              onSelect={(nodeId) =>
                setSelectedStep(nodeId === null ? null : { runId: run.data!.runId, step: nodeId })
              }
              onExploreLayer={openLayer}
              exploreableLayers={exploreableLayers}
            />
          ) : fallbackEdges.length === 0 ? (
            <p className="text-sm text-slate-400">No step transitions found.</p>
          ) : (
            <div className="bg-slate-900 rounded p-3 text-xs font-mono">
              {fallbackEdges.map((edge, i) => (
                <div key={`${edge.from}:${edge.via}:${edge.to}:${i}`} className="py-1 border-b last:border-b-0 border-slate-800">
                  <span className="text-sky-300">{edge.from}</span>
                  <span className="text-slate-500"> --{edge.via}--&gt; </span>
                  <span className="text-emerald-300">{edge.to}</span>
                </div>
              ))}
            </div>
          )}
          <PanelExpandButton
            title="Step graph"
            testId="graph-panel-expand"
            expanded={expandedPanel === 'graph'}
            onToggle={() => togglePanel('graph')}
            buttonRef={graphExpandRef}
            className={panelButtonClass(expandedPanel === 'graph')}
            controlsId="graph-panel"
          />
        </section>
        <ScopeAndControlsPanel
          runId={run.data.runId}
          status={run.data.status}
          pauseState={pauseState}
          events={events}
          capabilities={capabilities}
          onRefresh={() => {
            void run.refetch();
            refresh();
          }}
        />
      </div>
      {sourcePlacement === 'side' && !sourceCollapsed.side && (
        <DockedPanel
          title="Workflow source"
          testId="source-dock"
          onClose={() => setSourceCollapsed((state) => ({ ...state, side: true }))}
        >
          <WorkflowSourceView source={currentSource} highlight={highlightRange} />
        </DockedPanel>
      )}
    </div>
  );
}
