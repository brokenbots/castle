import { useMemo, useState, useEffect, useRef, type RefObject } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useSelector } from 'react-redux';
import { useGetRunQuery, type EventEnvelope } from '../../api/castleApi';
import { classifyError } from '../../api/errors';
import { selectPauseState } from './runsSlice';
import { useRunEventLog } from './eventLog/useRunEventLog';
import { useEventReplay } from './eventLog/useEventReplay';
import { EventLog } from './eventLog/EventLog';
import { StatusPill } from './StatusPill';
import { RunInspection, RunInspectionSummary } from './RunInspection';
import { RunControls } from './RunControls';
import { ScopeAndControlsPanel } from './ScopeAndControlsPanel';
import { CASTLE_RUN_CAPABILITIES, type RunCapabilities } from './capabilities';
import { ForEachStrip } from './eventLog/ForEachStrip';
import { PageState } from '../../components/PageState';
import { Breadcrumbs } from '../../components/Breadcrumbs';
import { DetailPanel } from '../../components/DetailPanel';
import { useDocumentTitle } from '../../shell/useDocumentTitle';
import { ReplayControls } from './ReplayControls';
import { extractTextEdges, parseWorkflowHcl, type WorkflowGraph as WorkflowGraphSpec } from './workflowGraph/parseWorkflowHcl';
import { buildSubworkflowLayers, followLayerPath, layerSourceText, selectWorkflowGraphs, type SubworkflowLayer } from './workflowGraph/layers';
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

type ExpandablePanel = 'graph';

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
const GRAPH_FULLSCREEN_CLASSES = `${FULLSCREEN_PANEL_CLASSES} [&>[data-testid=workflow-graph]]:flex-1 [&>[data-testid=workflow-graph]]:min-h-0`;
const PANEL_ICON_BUTTON_CLASSES =
  'shrink-0 rounded-md border border-line-strong p-1.5 text-ink-muted hover:bg-surface-raised hover:text-ink';

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
  // Replay (agent-first offline debugging): walks the loaded history over
  // the graph while the live stream keeps flowing underneath. `events`
  // stays the live stream; `replay` carries the view window the page
  // renders (untouched when inactive).
  const replay = useEventReplay(events);
  const viewEvents = replay.active ? replay.replayed : events;
  const pauseState = useSelector(selectPauseState(id));
  const [selectedStep, setSelectedStep] = useState<{ runId: string; step: string } | null>(null);
  // Which panel (if any) is currently expanded to a fullscreen overlay.
  // Null keeps every panel docked; toggling only swaps classes on the
  // wrapper sections, so panel components and their hooks stay mounted.
  const [expandedPanel, setExpandedPanel] = useState<ExpandablePanel | null>(null);
  // Graph reading direction (CRI-257); left-right keeps the entry node
  // leftmost and terminals on the right — the agent-first reading order —
  // with top-bottom available via the orientation toggle.
  const [graphOrientation, setGraphOrientation] = useState<GraphOrientation>('left-right');
  // Follow mode: keep the viewport centered on the running step as the
  // event stream advances (CRI-257). Drag-pan and wheel-zoom stay live.
  const [followMode, setFollowMode] = useState(false);
  // Whether the right-side details drawer (inspection details, workflow
  // source, run scope) is open. The page renders all three columns in the
  // drawer by default; the toggle collapses the drawer so the graph and
  // events stretch to the full width.
  const [drawerOpen, setDrawerOpen] = useState(true);
  const graphExpandRef = useRef<HTMLButtonElement>(null);

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
      if (expandedPanel === 'graph') graphExpandRef.current?.focus();
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
  // Compiled-JSON bodies (CRI-294) render pretty-printed.
  const currentSource = topLayer ? layerSourceText(topLayer) : workflowSource;
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
  // A failed run's reached terminal node (lit by the runCompleted message)
  // is re-tinted failed — the message carries the final state, and the
  // overall failure verdict comes from the run itself.
  const overlay = useMemo(() => {
    const base = graph ? selectNodeOverlay(viewEvents) : { statuses: {}, forEach: {} };
    if (run.data?.status === 'failed') {
      for (let i = viewEvents.length - 1; i >= 0; i -= 1) {
        const e = viewEvents[i];
        if (e.type !== 'runCompleted' && e.type !== 'runFailed') continue;
        const p = e.payload as { finalState?: unknown; final_state?: unknown } | undefined;
        const finalState = typeof p?.finalState === 'string' ? p.finalState : typeof p?.final_state === 'string' ? p.final_state : null;
        if (finalState) {
          base.statuses[finalState] = 'failed';
          break;
        }
      }
    }
    // Subworkflow exits light the CHILD layer's reached terminal (a state
    // node never receives its own event): the parent step's outcome IS the
    // sub-workflow's exit verdict — success polarity → the child's success
    // terminal, anything else → the non-success one.
    if (graph) {
      for (const node of graph.nodes) {
        if (!node.subworkflow) continue;
        const last = [...viewEvents].reverse().find((e) => {
          if (e.type === 'stepOutcome') return (e.payload as { step?: unknown })?.step === node.id;
          if (e.type === 'stepTransition') return (e.payload as { from?: unknown })?.from === node.id;
          return false;
        });
        if (!last) continue;
        const child = layersByName.get(node.subworkflow);
        if (!child?.graph) continue;
        const outcome =
          last.type === 'stepOutcome'
            ? (last.payload as { outcome?: unknown })?.outcome
            : 'success'; // a transition implies a successful exit
        const wantSuccess = outcome === 'success' || outcome === 'ok';
        const terminal = child.graph.nodes.find(
          (n) => n.terminal && Boolean(n.success) === wantSuccess,
        );
        if (terminal && !base.statuses[terminal.id]) {
          base.statuses[terminal.id] = wantSuccess ? 'succeeded' : 'failed';
        }
      }
    }
    return base;
  }, [graph, viewEvents, run.data?.status, layersByName]);
  // The status pill is the run's ground truth from castle at all times —
  // replay animates the graph/event surface underneath and never fakes the
  // run row's state (a stopped/failed/succeeded run keeps its real pill;
  // replay just reads the loaded history, which has no live status of its
  // own).
  // Where the process is, per miniature (agent-first position cues): the
  // nodes of each layer graph currently running or failed, so the rail's
  // thumbnails show where in the graph the process is.
  const activeIdsByLayer = useMemo(() => {
    const map = new Map<string, Set<string>>();
    const byGraph = (g: WorkflowGraphSpec | null | undefined) => {
      if (!g) return new Set<string>();
      const ids = new Set<string>();
      for (const node of g.nodes) {
        const status = overlay.statuses[node.id];
        if (status === 'running' || status === 'failed') ids.add(node.id);
      }
      return ids;
    };
    map.set('', byGraph(graph));
    for (const layer of layersByName.values()) map.set(layer.name, byGraph(layer.graph));
    return map;
  }, [overlay, graph, layersByName]);
  // The node the engine (or the replay cursor, while replaying) is
  // currently inside — the newest stepEntered of the view window. The
  // follow target for the graph pan and the auto-traversal.
  const currentStepId = useMemo(() => {
    for (let i = viewEvents.length - 1; i >= 0; i -= 1) {
      const e = viewEvents[i];
      if (e.type !== 'stepEntered') continue;
      const step = (e.payload as { step?: unknown } | undefined)?.step;
      if (typeof step === 'string' && step !== '') return step;
    }
    return null;
  }, [viewEvents]);
  // Follow-along traversal (agent-first): when follow mode is on and the
  // engine's step runs inside a subworkflow, the stack auto-opens to the
  // layer containing it (following the last chain of subworkflow targets
  // that resolves it). User navigation wins afterwards — the effect only
  // acts while the live graph does not already contain the step.
  const autoTraversing = useRef(false);
  const followPath = useMemo(
    () => (followMode ? followLayerPath(graph, currentStepId, layersByName) : null),
    [followMode, graph, currentStepId, layersByName],
  );
  useEffect(() => {
    if (!followPath) return;
    if (
      followPath.length === layerStackLayers.length &&
      layerStackLayers.every((layer, i) => layer.name === followPath[i])
    ) {
      return;
    }
    autoTraversing.current = true;
    setLayerStack({ runId: run.data!.runId, names: followPath });
  }, [followPath, layerStackLayers, run.data]);
  const selected = selectedStep && selectedStep.runId === (run.data?.runId ?? '') ? selectedStep.step : null;
  // Drawer follow (CRI-2xx): the source and inspection panels can track the
  // view's current step (live engine step, or the replay cursor's while
  // replaying — currentStepId already reads the replay window) instead of
  // the manual node selection. Each panel follows independently; the source
  // panel defaults to following, the inspection one to manual.
  const viewStepId = currentStepId;
  const [followSource, setFollowSource] = useState(true);
  const [followInspection, setFollowInspection] = useState(false);
  const toggleFollowSource = () => setFollowSource((v) => !v);
  const toggleFollowInspection = () => setFollowInspection((v) => !v);
  // Node click drives source highlighting: the graph parser records each
  // node's exact declaration range, so the source pane highlights the block
  // (and scrolls to it) without re-scanning (CRI-257). Ranges index into
  // the source of the graph being viewed (root workflow or open layer).
  const selectedNode = useMemo(
    () => (currentGraph ? currentGraph.nodes.find((n) => n.id === selected) ?? null : null),
    [currentGraph, selected],
  );
  const highlightRange = selectedNode?.sourceRange ?? null;
  // The follow-linked source highlight: while the source panel follows, the
  // view's current step's declaration lights regardless of manual selection.
  const followHighlight = useMemo(() => {
    if (!followSource || !viewStepId) return null;
    const node = currentGraph?.nodes.find((n) => n.id === viewStepId);
    return node?.sourceRange ?? null;
  }, [followSource, viewStepId, currentGraph]);
  const sourceHighlight = followSource ? followHighlight ?? highlightRange : highlightRange;
  const visibleEvents = useMemo(
    () => (selected ? viewEvents.filter((e) => eventBelongsToStep(e, selected)) : viewEvents),
    [viewEvents, selected],
  );

  // CRI-131 re-home (KB-102): run identity metadata (ticket / repo / PR) no
  // longer rides wire fields on the run row — the producer publishes it as
  // run.metadata envelopes. Derive the display metadata from the event log
  // with the same promotion rules the castle store applies: last-wins per
  // field, non-empty values only. Payload keys are read in both protojson
  // spellings (camel jsonName and the proto field name).
  const runMeta = useMemo(() => {
    const meta: { ticket?: string; repo?: string; pr?: string } = {};
    const pick = (msg: Record<string, unknown>, camel: string, snake: string): string | undefined => {
      const v = msg[camel] ?? msg[snake];
      return typeof v === 'string' && v.length > 0 ? v : undefined;
    };
    for (const e of events) {
      if (e.type !== 'runMetadata') continue;
      const p = e.payload as Record<string, unknown> | null | undefined;
      if (!p) continue;
      meta.ticket = pick(p, 'ticket', 'ticket') ?? meta.ticket;
      meta.repo = pick(p, 'repoUrl', 'repo_url') ?? meta.repo;
      meta.pr = pick(p, 'prUrl', 'pr_url') ?? meta.pr;
    }
    return meta;
  }, [events]);

  // Only render the PR link for http(s) URLs; the publisher controls the
  // value and must not be able to inject javascript: hrefs.
  const prUrl =
    runMeta.pr?.startsWith('http://') || runMeta.pr?.startsWith('https://') ? runMeta.pr : undefined;

  // A run is live-tailing while its status is running and no terminal event
  // has arrived yet (the status can lag the event stream).
  const running = useMemo(() => {
    if (!run.data || run.data.status !== 'running') return false;
    return !events.some((e) => e.type === 'runCompleted' || e.type === 'runFailed');
  }, [run.data, events]);

  // The run row (status/final state) is fetched once; a run that ends while
  // the page is open would otherwise keep showing running until a manual
  // refresh. When the stream delivers the terminal message, re-fetch the
  // row once so the header/summary land on the terminal state without the
  // user reloading (the graph needs no re-fetch: it renders from the
  // stream's own terminal event).
  const streamEnded = useMemo(
    () => events.some((e) => e.type === 'runCompleted' || e.type === 'runFailed'),
    [events],
  );
  const endedRef = useRef(false);
  useEffect(() => {
    if (!streamEnded) {
      endedRef.current = false;
      return;
    }
    if (endedRef.current) return;
    endedRef.current = true;
    void run.refetch();
  }, [streamEnded, run]);

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
    <div className="flex h-full min-h-0 flex-col" data-testid="run-detail-layout">
      {/* Thin static header: identity, status and control row stay pinned
          above the scrollable workspace, so controls remain reachable while
          the graph and events scroll beneath. */}
      <header
        data-testid="run-detail-header"
        className="sticky top-0 z-20 shrink-0 border-b border-line bg-canvas/95 px-4 py-3 backdrop-blur-sm"
      >
        <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-2">
          <div className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1">
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
          <div data-testid="run-detail-actions" className="flex shrink-0 items-center gap-3">
            <RunControls
              runId={run.data.runId}
              status={run.data.status}
              pauseState={pauseState}
              capabilities={capabilities}
            />
            <button
              type="button"
              data-testid="run-drawer-toggle"
              aria-expanded={drawerOpen}
              aria-controls="run-detail-drawer"
              title={drawerOpen ? 'Hide the details drawer' : 'Show the details drawer'}
              onClick={() => setDrawerOpen((v) => !v)}
              className={`rounded-md border p-1.5 ${drawerOpen ? 'border-accent-strong bg-accent-strong/10 text-accent-strong' : 'border-line-strong text-ink-muted hover:bg-surface-raised hover:text-ink'}`}
            >
              <svg aria-hidden viewBox="0 0 24 24" className="h-4 w-4" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                <rect x="3" y="4" width="18" height="16" rx="2" />
                <path d="M15 4v16" />
              </svg>
            </button>
          </div>
        </div>
        <div className="mt-2 flex flex-wrap items-center justify-between gap-x-6 gap-y-2">
          <div className="flex min-w-0 items-center gap-4">
            <h2 className="text-title font-semibold text-ink">{run.data.workflowName}</h2>
            <StatusPill status={run.data.status} pauseEvent={pauseState.pauseEvent} />
            <RunInspectionSummary runId={id} status={run.data.status} />
            {run.data.finalState && (
              <span className="text-body text-ink-muted">
                final: <span className="font-mono">{run.data.finalState}</span>
              </span>
            )}
            {prUrl && (
              <a className="text-accent-strong hover:underline" href={prUrl} target="_blank" rel="noreferrer">
                PR
              </a>
            )}
          </div>
          <span className="font-mono text-meta text-ink-faint">{run.data.runId}</span>
        </div>
        {runMeta.ticket && (
          <div className="mt-1 text-body text-ink-muted">
            {/* CRI-131 re-home: run metadata rides run.metadata envelopes; the
                ticket is the operator-facing identity. */}
            ticket: <span className="font-mono">{runMeta.ticket}</span>
            {runMeta.repo && (
              <>
                {' · '}
                <span className="font-mono">{runMeta.repo}</span>
              </>
            )}
          </div>
        )}
        {/* watchRun liveness: surface stream loss to the user instead of
            only logging it. While reconnecting show the bounded-backoff
            progress; once retries are exhausted offer a manual reconnect. */}
        {watch && (watch.state === 'reconnecting' || watch.state === 'lost' || watch.state === 'unauthenticated') && (
          <div
            data-testid="stream-status"
            data-state={watch.state}
            role="status"
            className="mt-2 flex flex-wrap items-center gap-3 rounded-md border border-line bg-surface px-4 py-2 text-body"
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
      </header>
      <div className="flex min-h-0 flex-1 items-stretch">
        {/* The workspace pane fills the viewport below the header with no
            scrolling of its own: the graph takes the remaining space and
            pans/zooms internally, events pin to a fixed tail strip at the
            bottom. Tight margins keep the graph the star. */}
        <div className="flex min-w-0 flex-1 flex-col gap-2 p-2">
          {/* Graph first: the user asked for the workflow state front and
              center, directly below the overview header. */}
          <section
            id="graph-panel"
            data-testid="graph-panel"
            data-expanded={expandedPanel === 'graph'}
            className={expandedPanel === 'graph' ? GRAPH_FULLSCREEN_CLASSES : 'relative flex min-h-0 flex-1 flex-col'}
          >
            <DetailPanel
              id="graph-panel-inner"
              title="Step graph"
              collapsible={false}
              className="flex min-h-0 flex-1 flex-col"
              bodyClassName="flex min-h-0 flex-1 flex-col gap-2 p-2"
              toolbar={
                <>
                  <ReplayControls replay={replay} live={running} followEnabled={followMode} onToggleFollow={() => setFollowMode((v) => !v)} />
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
                  {/* Fullscreen is one of the graph's layout controls, in the
                      toolbar with the others; the same control collapses the
                      overlay (Escape also works, focus returns here). */}
                  <PanelExpandButton
                    title="Step graph"
                    testId="graph-panel-expand"
                    expanded={expandedPanel === 'graph'}
                    onToggle={() => togglePanel('graph')}
                    buttonRef={graphExpandRef}
                    className={PANEL_ICON_BUTTON_CLASSES}
                    controlsId="graph-panel"
                  />
                </>
              }
            >
              {layerStackLayers.length > 0 || exploreableLayers.size > 0 ? (
                <WorkflowLayerNav
                  rootName={run.data?.workflowName || 'Workflow'}
                  rootGraph={graph}
                  stack={layerStackLayers}
                  onNavigate={navigateLayers}
                  activeIdsByLayer={activeIdsByLayer as Map<string, ReadonlySet<string>>}
                />
              ) : null}
              {currentGraph ? (
                <WorkflowGraph
                  // Remount with the stack shape so a re-entered same-named
                  // step inside a re-opened layer re-triggers the follow pan
                  // (the memo inside WorkflowGraph otherwise suppresses it).
                  key={`${topLayer?.name ?? 'root'}:${layerStackLayers.length}:${graphOrientation}`}
                  graph={currentGraph}
                  statuses={overlay.statuses}
                  forEachProgress={overlay.forEach}
                  orientation={graphOrientation}
                  followStepId={followMode ? viewStepId : null}
                  selectedId={selected}
                  onSelect={(nodeId) =>
                    setSelectedStep(nodeId === null ? null : { runId: run.data!.runId, step: nodeId })
                  }
                  onExploreLayer={openLayer}
                  exploreableLayers={exploreableLayers}
                />
              ) : fallbackEdges.length === 0 ? (
                <p className="text-sm text-ink-muted">No step transitions found.</p>
              ) : (
                <div data-testid="graph-fallback-edges" className="bg-surface rounded p-3 text-xs font-mono text-ink-muted">
                  {fallbackEdges.map((edge, i) => (
                    <div key={`${edge.from}:${edge.via}:${edge.to}:${i}`} className="py-1 border-b last:border-b-0 border-line">
                      <span className="text-accent-strong">{edge.from}</span>
                      <span className="text-ink-faint"> --{edge.via}--&gt; </span>
                      <span className="text-success">{edge.to}</span>
                    </div>
                  ))}
                </div>
              )}
              {/* Events live inside the graph box as a collapsible bottom
                  strip; collapsing it hands the reclaimed height to the
                  graph. No fullscreen: the strip is docked furniture. */}
              <section id="events-panel" data-testid="events-panel" className="relative shrink-0">
                <DetailPanel
                  id="events-panel-inner"
                  idToggle="events-panel-collapse"
                  title="Events"
                >
                  <div data-testid="events-panel-body" className="space-y-2">
                    {selected && (
                      <div className="flex items-center gap-2 text-xs" data-testid="step-filter">
                        <span className="rounded bg-accent-soft px-2 py-1 text-accent-strong border border-accent-border font-mono">
                          Filtered to step: {selected}
                        </span>
                        <button
                          type="button"
                          data-testid="clear-step-filter"
                          onClick={() => setSelectedStep(null)}
                          className="text-ink-muted hover:text-ink"
                        >
                          Clear filter
                        </button>
                      </div>
                    )}
                    <EventLog
                      events={visibleEvents}
                      running={running}
                      hasEarlier={log.hasEarlier}
                      loadingEarlier={log.loadingEarlier}
                      onLoadEarlier={loadEarlier}
                      variant="strip"
                    />
                  </div>
                </DetailPanel>
              </section>
            </DetailPanel>
          </section>

          {forEachNodes.size > 0 && (
            <section>
              {Array.from(forEachNodes.entries()).map(([node, nodeEvents]) => (
                <ForEachStrip key={node} runId={id} events={nodeEvents} />
              ))}
            </section>
          )}
        </div>
        {/* Right drawer: inspection details, workflow source and the run's
            scope live off the main flow; collapsible via the header toggle.
            The source and inspection panels can follow the replayed step:
            source highlights its declaration, inspection expands its rows. */}
        {drawerOpen && (
          <aside
            id="run-detail-drawer"
            data-testid="run-detail-drawer"
            className="flex w-[28rem] min-w-[20rem] shrink-0 flex-col overflow-hidden border-l border-line bg-surface"
          >
            <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
              <DetailPanel
                id="inspection-drawer"
                title="Inspection details"
                followed={followInspection}
                followTestId="inspection-follow-toggle"
                onFollowToggle={toggleFollowInspection}
              >
                <RunInspection runId={id} status={run.data.status} />
              </DetailPanel>
              <DetailPanel
                id="source-drawer"
                title="Workflow source"
                followed={followSource}
                followTestId="source-follow-toggle"
                onFollowToggle={toggleFollowSource}
              >
                <WorkflowSourceView source={currentSource} highlight={sourceHighlight} />
              </DetailPanel>
              <ScopeAndControlsPanel
                runId={run.data.runId}
                status={run.data.status}
                pauseState={pauseState}
                events={events}
                capabilities={capabilities}
                showControls={false}
                onRefresh={() => {
                  void run.refetch();
                  refresh();
                }}
              />
            </div>
          </aside>
        )}
      </div>
    </div>
  );
}
