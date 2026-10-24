import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties, MouseEvent } from 'react';
import {
  Background,
  BaseEdge,
  EdgeLabelRenderer,
  Handle,
  Panel,
  Position,
  ReactFlow,
  useReactFlow,
  useStore,
  type Edge,
  type Node,
  type NodeProps,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import type { WorkflowGraph, WorkflowGraphNode, WorkflowNodeKind } from './parseWorkflowHcl';
import { STEP_NODE_STATUS_CLASSES } from '../runStatus';
import { classifyEdges, type GraphOrientation, type NodePosition } from './layout';
import { layoutWithElk } from './elkLayout';
import type { ForEachProgress, StepNodeStatus } from './nodeStatus';

// React Flow styles edges/backgrounds via inline SVG props (no class names),
// so the palette here mirrors the host's CSS variables (index.css):
// canvas #020617, surface #0f172a, line #1e293b, line-strong #334155,
// ink-muted #94a3b8, accent-strong #0ea5e9, success #34d399, danger #fb7185.
export const GRAPH_LINE = '#475569';
export const GRAPH_LOOP = '#a78bfa';
export const GRAPH_BACK = '#64748b';

export interface WorkflowGraphProps {
  graph: WorkflowGraph;
  /** Live overlay state per node id (absent = unvisited). */
  statuses?: Record<string, StepNodeStatus>;
  /** Per-iteration progress per for_each node id. */
  forEachProgress?: Record<string, ForEachProgress>;
  /** Currently selected node id, if any. */
  selectedId?: string | null;
  /** Graph reading direction; top-bottom is the default. */
  orientation?: GraphOrientation;
  /**
   * When set (follow mode), the viewport centers on this node whenever it
   * changes — the running step in practice. Keyed on the step id, so a
   * loop re-entering the same node does not re-trigger the pan.
   */
  followStepId?: string | null;
  /**
   * Called with the clicked node id, or with null when the already-selected
   * node is clicked again (toggling the selection off).
   */
  onSelect?: (nodeId: string | null) => void;
  /**
   * Called when a node's subworkflow explore affordance is activated
   * (CRI-257 drill-down). Only fires for layers that actually resolved to
   * a parsed graph.
   */
  onExploreLayer?: (name: string) => void;
  /**
   * Names of subworkflow layers that resolved to a parsed graph (from the
   * workflow.graphs event). A node whose subworkflow is not in the set
   * renders its affordance disabled — the layer graph is not available
   * (no event yet, or an unparsable body). Absent means nothing is
   * available yet.
   */
  exploreableLayers?: Set<string>;
}

interface WorkflowNodeData extends Record<string, unknown> {
  node: WorkflowGraphNode;
  status: StepNodeStatus;
  /** for_each iteration badge text, e.g. "2/5". */
  badge?: string;
  /**
   * Targets of this node's loop legs (edges classified as back + cycle),
   * e.g. ["build"] — rendered as the collapsed loop badge "↺ loops back
   * to build". Cycles are the designed norm in workflows, so the badge
   * reads as a first-class feature rather than edge noise.
   */
  loopsTo?: string[];
  /** Currently selected node (highlight ring). */
  selected: boolean;
  /** Graph reading direction; drives handle sides. */
  orientation: GraphOrientation;
  /** The subworkflow layer this step runs, when its target crosses one. */
  explore?: {
    name: string;
    /** True when the layer resolved to a parsed graph. */
    available: boolean;
    onExplore?: (name: string) => void;
  };
}

type WorkflowFlowNode = Node<WorkflowNodeData, 'workflow'>;

const STATUS_CLASS: Record<StepNodeStatus, string> = STEP_NODE_STATUS_CLASSES;

const KIND_LABEL: Record<WorkflowGraphNode['kind'], string> = {
  step: 'step',
  switch: 'switch',
  wait: 'wait',
  approval: 'approval',
  state: 'state',
  target: 'target',
};

const KIND_CLASS: Record<WorkflowNodeKind, string> = {
  step: 'text-accent-strong',
  switch: 'text-warning',
  wait: 'text-ink-muted',
  approval: 'text-ink-muted',
  state: 'text-success/80',
  target: 'text-ink-faint',
};

const STATUS_MARK: Record<StepNodeStatus, string> = {
  idle: '○',
  running: '●',
  succeeded: '✓',
  failed: '✗',
};

/** Target/source handle sides per orientation: flow enters top (TB) or
 * left (LR) and exits bottom (TB) or right (LR). */
const HANDLE_POSITION: Record<GraphOrientation, { target: Position; source: Position }> = {
  'top-bottom': { target: Position.Top, source: Position.Bottom },
  'left-right': { target: Position.Left, source: Position.Right },
};

/** The node card's fixed geometry — MUST equal the size ELK lays out
 * (ELK_NODE_W/H in elkLayout.ts): handles, edge anchors and rendered box
 * then coincide by construction, no measurement feedback loop. Long names
 * ellipsize inside the box; boxes stay uniform across the graph. */
const NODE_BOX_W = 208;
const NODE_BOX_H = 96;

function WorkflowNodeView({ data }: NodeProps<WorkflowFlowNode>) {
  const { node, status, badge, selected, orientation, explore, loopsTo } = data;
  const handle = HANDLE_POSITION[orientation];
  const selectedClass = selected ? ' ring-2 ring-accent-strong' : '';
  // Unvisited nodes dim their TEXT (the fill stays opaque to block edges
  // passing underneath — node-level opacity bled them through).
  const dim = status === 'idle' ? 'opacity-60' : '';
  return (
    <div
      data-testid="graph-node"
      data-node-id={node.id}
      // Fixed box: the layout engine declared exactly this size, and the
      // handles anchor at its edges — routes connect by construction. The
      // opaque literal surface matches the graph palette (#0f172a); text
      // dims for unvisited nodes, never the box itself (card-level opacity
      // bled edges through).
      style={{ width: NODE_BOX_W, height: NODE_BOX_H, background: '#0f172a' }}
      className={`rounded-lg border px-3 py-2 text-center shadow flex flex-col justify-center overflow-hidden ${STATUS_CLASS[status]}${selectedClass}`}
    >
      <Handle type="target" position={handle.target} className="!h-1.5 !w-1.5 !border-0 !bg-line-strong" />
      <p className={`font-mono text-sm text-ink truncate ${dim}`} title={node.id}>{node.id}</p>
      <p className={`mt-0.5 text-[11px] uppercase tracking-wide font-semibold ${dim} ${KIND_CLASS[node.kind]}`}>
        {KIND_LABEL[node.kind]}
      </p>
      {badge && <p className="mt-0.5 text-[10px] font-mono text-ink-muted">{badge}</p>}
      {loopsTo && loopsTo.length > 0 && (
        <p data-testid="graph-node-loop-badge" className="mt-0.5 text-[10px] font-mono text-[#a78bfa] truncate">
          ↺ loops back to {truncate(loopsTo.join(', '))}
        </p>
      )}
      {explore && (
        <button
          type="button"
          data-testid="graph-node-explore"
          title={
            explore.available
              ? `Open subworkflow ${explore.name}`
              : `Subworkflow ${explore.name} graph not available yet`
          }
          disabled={!explore.available}
          onClick={(event) => {
            // The affordance opens the layer; it must not toggle the
            // node selection underneath (ReactFlow's node click).
            event.stopPropagation();
            if (explore.available) explore.onExplore?.(explore.name);
          }}
          className="mt-1 w-full truncate rounded border border-accent-strong/40 px-1 py-0.5 text-[10px] font-medium text-accent-strong hover:bg-accent-strong/10 disabled:cursor-not-allowed disabled:border-line-strong disabled:text-ink-faint"
        >
          ⤷ {explore.name}
        </button>
      )}
      <p className="text-xs" aria-label={`status ${status}`}>
        <span
          className={
            status === 'succeeded'
              ? 'text-success'
              : status === 'failed'
                ? 'text-danger'
                : status === 'running'
                  ? 'text-accent-strong'
                  : 'text-ink-faint'
          }
        >
          {STATUS_MARK[status]}
        </span>
      </p>
      <Handle type="source" position={handle.source} className="!h-1.5 !w-1.5 !border-0 !bg-line-strong" />
    </div>
  );
}

const nodeTypes = { workflow: WorkflowNodeView };

//
// ELK polyline edge: renders the orthogonal route the layout engine
// computed (guaranteed node-avoiding) plus the via label at the route's
// midpoint. Registered as the `elk` edge type for every edge.
//
interface ElkEdgeData extends Record<string, unknown> {
  points?: Array<{ x: number; y: number }>;
  label?: string;
}

// Edge component per EdgeTypes: props carry the standard edge subset —
// typing the data via the node data generic keeps the index signature happy.
function ElkPolylineEdge({
  id,
  data,
  style,
  markerEnd,
}: {
  id: string;
  data: ElkEdgeData;
  style?: CSSProperties;
  markerEnd?: string;
}) {
  const points = data?.points ?? [];
  const path = useMemo(() => {
    if (points.length < 2) return '';
    let d = `M ${points[0].x} ${points[0].y}`;
    for (let i = 1; i < points.length; i += 1) d += ` L ${points[i].x} ${points[i].y}`;
    return d;
  }, [points]);
  // Label anchors at the polyline's true midpoint (walk the segments to the
  // half-length, since a label averaged between the endpoints floats off
  // the routed path entirely on orthogonal detours).
  const labelX = useMemo(() => {
    if (points.length < 2) return 0;
    let total = 0;
    for (let i = 1; i < points.length; i += 1) total += Math.hypot(points[i].x - points[i - 1].x, points[i].y - points[i - 1].y);
    let walked = 0;
    for (let i = 1; i < points.length; i += 1) {
      const seg = Math.hypot(points[i].x - points[i - 1].x, points[i].y - points[i - 1].y);
      if (walked + seg >= total / 2) {
        const t = (total / 2 - walked) / (seg || 1);
        return points[i - 1].x + t * (points[i].x - points[i - 1].x);
      }
      walked += seg;
    }
    return points[points.length - 1].x;
  }, [points]);
  const labelY = useMemo(() => {
    if (points.length < 2) return 0;
    let total = 0;
    for (let i = 1; i < points.length; i += 1) total += Math.hypot(points[i].x - points[i - 1].x, points[i].y - points[i - 1].y);
    let walked = 0;
    for (let i = 1; i < points.length; i += 1) {
      const seg = Math.hypot(points[i].x - points[i - 1].x, points[i].y - points[i - 1].y);
      if (walked + seg >= total / 2) {
        const t = (total / 2 - walked) / (seg || 1);
        return points[i - 1].y + t * (points[i].y - points[i - 1].y);
      }
      walked += seg;
    }
    return points[points.length - 1].y;
  }, [points]);
  if (!path) return null;
  return (
    <>
      <BaseEdge id={id} path={path} style={style} markerEnd={markerEnd} />
      {data.label && (
        <EdgeLabelRenderer>
          <div
            className="nodrag nopan"
            style={{
              position: 'absolute',
              transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`,
              fontSize: 10,
              padding: '1px 4px',
              borderRadius: 3,
              pointerEvents: 'none',
              ...((data.labelStyle ?? {}) as CSSProperties),
              background: (data.labelBgStyle as CSSProperties | undefined)?.background ?? '#0f172a',
            }}
          >
            {data.label}
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}

const edgeTypes = { elk: ElkPolylineEdge };

/**
 * In-graph behavior mounted inside <ReactFlow> so it can reach the store:
 * pans/zooms to a context window around the followed node — the running
 * step with up to `hops` predecessors and successors in frame (about 3
 * steps out) so the operator sees where the flow came from and where it
 * goes — and offers the reset control that restores the full-graph framing.
 */
/** Fixed follow zoom: the active node renders identically sized in every
 * graph, regardless of context or graph size (the operator tracks the
 * process by the centered card, not the window width). */
const FOLLOW_ZOOM = 1;
// Center fallback when RF has not measured yet (jsdom); equals the node
// box geometry.
const FOLLOW_NODE_FALLBACK_W = 208;
const FOLLOW_NODE_FALLBACK_H = 96;

function GraphBehavior({ followStepId }: { followStepId: string | null; focusIds: string[] }) {
  const { setCenter, getNode, fitView } = useReactFlow();
  const followedRef = useRef<string | null>(null);
  // Follow centering: the active node sits dead-center at the fixed
  // FOLLOW_ZOOM (never a content fit — the frame size varies with nothing).
  // Centering is staged/refired on nodesInitialized because fresh mounts
  // (orientation/layer changes) have unmeasured nodes; the mount-time prop
  // fit is disabled while following (fitView={!followStepId}) so nothing
  // races the follow frame.
  const centerFollow = useCallback(
    (duration: number) => {
      const id = followedRef.current;
      if (!id) return;
      const node = getNode(id);
      if (!node) return;
      const width = node.measured?.width || FOLLOW_NODE_FALLBACK_W;
      const height = node.measured?.height || FOLLOW_NODE_FALLBACK_H;
      void setCenter(node.position.x + width / 2, node.position.y + height / 2, {
        zoom: FOLLOW_ZOOM,
        duration,
      });
    },
    [getNode, setCenter],
  );
  useEffect(() => {
    if (!followStepId) return;
    followedRef.current = followStepId;
    centerFollow(400);
    const raf = requestAnimationFrame(() => centerFollow(300));
    const settled = setTimeout(() => centerFollow(300), 350);
    return () => {
      cancelAnimationFrame(raf);
      clearTimeout(settled);
    };
  }, [followStepId, centerFollow]);
  // The deterministic moment: node bounds just became readable.
  const nodesInitialized = useStore((state) => state.nodesInitialized);
  useEffect(() => {
    if (nodesInitialized && followedRef.current) centerFollow(250);
  }, [nodesInitialized, centerFollow]);
  return (
    <Panel position="top-right">
      <button
        type="button"
        data-testid="graph-reset-view"
        title="Reset graph view"
        onClick={() => {
          // Clear the follow memo so a re-entered node can re-center.
          followedRef.current = null;
          void fitView({ duration: 200, padding: 0.15 });
        }}
        className="rounded border border-line-strong bg-surface/95 px-2 py-1 text-xs text-ink-muted hover:bg-surface-raised"
      >
        Reset view
      </button>
    </Panel>
  );
}

/**
 * Follow context of a step: the step plus up to `hops` levels of
 * predecessors and successors along the flow edges — the nodes the
 * operator reads to see where the process is and where it heads.
 */
export function followFocusIds(graph: WorkflowGraph, stepId: string | null, hops = 3): string[] {
  if (!stepId) return [];
  const ids = new Set<string>([stepId]);
  const walk = (fromEnd: boolean) => {
    let frontier = [stepId];
    for (let hop = 0; hop < hops; hop += 1) {
      const next: string[] = [];
      for (const boundary of frontier) {
        for (const edge of graph.edges) {
          const neighbor = fromEnd
            ? // Walking backward: edges whose target sits on the frontier.
              edge.to === boundary ? edge.from : null
            : edge.from === boundary ? edge.to : null;
          if (neighbor && !ids.has(neighbor)) {
            ids.add(neighbor);
            next.push(neighbor);
          }
        }
      }
      frontier = next;
      if (frontier.length === 0) break;
    }
  };
  walk(true);
  walk(false);
  return [...ids];
}

export function WorkflowGraph({ graph, statuses = {}, forEachProgress = {}, selectedId, orientation = 'top-bottom', followStepId, onSelect, onExploreLayer, exploreableLayers }: WorkflowGraphProps) {
  // ELK layout is async (worker): the flow re-renders when positions and
  // edge routes land. Until then the graph renders nothing (jsdom and
  // worker-less environments get the sync legacy layout immediately —
  // see layout.ts / elkLayout.ts).
  const [layout, setLayout] = useState<
    | { positions: Map<string, NodePosition>; edgePoints: Map<number, Array<{ x: number; y: number }>> }
    | null
  >(null);
  // Node cards are fixed-size (NODE_BOX_W/H == ELK's declared size), so
  // ELK's edge anchors and React Flow's handle positions coincide by
  // construction — no measure feedback, no loop: one layout pass per
  // graph/orientation change.
  useEffect(() => {
    let cancelled = false;
    setLayout(null);
    void layoutWithElk(graph, { orientation }).then((result) => {
      if (!cancelled) setLayout(result);
    });
    return () => {
      cancelled = true;
    };
  }, [graph, orientation]);

  const { nodes, edges } = useMemo(
    () => {
      const result = layout
        ? buildFlow(graph, statuses, forEachProgress, selectedId ?? null, orientation, onExploreLayer, exploreableLayers, layout)
        : { nodes: [], edges: [] };
      if (import.meta.env.DEV) console.log('WFLOW', 'nodes', result.nodes.length, 'edges', result.edges.length, 'layout', layout ? 'set' : 'null', 'routes', layout?.edgePoints.size ?? -1);
      return result;
    },
    [layout, graph, statuses, forEachProgress, selectedId, orientation, onExploreLayer, exploreableLayers],
  );
  const focusIds = useMemo(() => followFocusIds(graph, followStepId ?? null), [graph, followStepId]);

  const handleNodeClick = onSelect
    ? (_event: MouseEvent, node: WorkflowFlowNode) => onSelect(node.id === selectedId ? null : node.id)
    : undefined;

  return (
    <div
      data-testid="workflow-graph"
      className="min-h-0 flex-1 bg-surface rounded border border-line overflow-hidden"
    >
      <ReactFlow
        key={orientation}
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodeClick={handleNodeClick}
        nodesConnectable={false}
        zoomOnScroll
        // The mount-time whole-graph fit only when follow is NOT active:
        // with follow on, the ±3-hop frame fit (GraphBehavior, on
        // nodesInitialized) must not race the prop fit — the race showed
        // as random pan-outs during replay traversal.
        fitView={!followStepId}
        fitViewOptions={{ padding: 0.08 }}
        minZoom={0.2}
        proOptions={{ hideAttribution: true }}
      >
        <Background color="#1e293b" gap={16} />
        <GraphBehavior followStepId={followStepId ?? null} focusIds={focusIds} />
      </ReactFlow>
    </div>
  );
}

function truncate(text: string, max = 28): string {
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

function buildFlow(
  graph: WorkflowGraph,
  statuses: Record<string, StepNodeStatus>,
  forEachProgress: Record<string, ForEachProgress>,
  selectedId: string | null,
  orientation: GraphOrientation,
  onExploreLayer: ((name: string) => void) | undefined,
  exploreableLayers: Set<string> | undefined,
  layout: { positions: Map<string, NodePosition>; edgePoints: Map<number, Array<{ x: number; y: number }>> },
): { nodes: WorkflowFlowNode[]; edges: Edge[] } {
  const positions = layout.positions;
  const roles = classifyEdges(graph);
  // Upward loop legs carry the collapsed loop badge on their source node
  // ("↺ loops back to X"); badge edges from the same source are grouped
  // so a node shows one badge listing its targets.
  const loopsTo = new Map<string, string[]>();
  for (const index of roles.loopBadge) {
    const edge = graph.edges[index];
    const targets = loopsTo.get(edge.from) ?? [];
    if (!targets.includes(edge.to)) targets.push(edge.to);
    loopsTo.set(edge.from, targets);
  }
  const nodes: WorkflowFlowNode[] = graph.nodes.map((node) => {
    const progress = forEachProgress[node.id];
    // Live per-iteration progress wins; otherwise show the declared
    // iteration control of the step.
    const badge = progress
      ? formatProgress(progress)
      : node.iteration
        ? iterationBadge(node.iteration)
        : undefined;
    return {
      id: node.id,
      type: 'workflow' as const,
      position: positions.get(node.id) ?? { x: 0, y: 0 },
      data: {
        node,
        status: statuses[node.id] ?? 'idle',
        badge,
        loopsTo: loopsTo.get(node.id),
        selected: node.id === selectedId,
        orientation,
        explore: node.subworkflow
          ? {
              name: node.subworkflow,
              available: exploreableLayers?.has(node.subworkflow) ?? false,
              onExplore: onExploreLayer,
            }
          : undefined,
      },
    };
  });
  // smoothstep edges can carry pathOptions (lane offset) — the main entry
  // exports only the component, so the option rides through this narrow
  // intersection type.
  type RoutedEdge = Edge & { pathOptions?: { offset?: number; borderRadius?: number } };
  const edges: Edge[] = graph.edges.map((edge, index) => {
    const route = layout.edgePoints.get(index);
    // Solid grey = forward flow. Dashed = a return (loop/back edge): with
    // in-left/out-right handles the dash pattern is the direction cue —
    // the old opacity variants only muddied the lines.
    const dashed = roles.cycle.has(index) && roles.back.has(index);
    const base: Edge = route
      ? // ELK-computed orthogonal route: rendered by the polyline edge type
        {
          id: `e${index}`,
          source: edge.from,
          target: edge.to,
          type: 'elk' as const,
          data: { points: route, label: truncate(edge.via) },
          labelStyle: { fill: '#cbd5e1', fontSize: 10 },
          labelBgStyle: { fill: '#0f172a' },
        }
      : // Sync fallback (worker-less envs): plain smoothstep between the
        // handle positions.
        {
          id: `e${index}`,
          source: edge.from,
          target: edge.to,
          label: truncate(edge.via),
          type: 'smoothstep',
          labelStyle: { fill: '#cbd5e1', fontSize: 10 },
          labelBgStyle: { fill: '#0f172a' },
          labelBgPadding: [4, 2],
          labelBgBorderRadius: 3,
        };
    const flowEdge: RoutedEdge = { ...base, style: { stroke: GRAPH_LINE } };
    if (roles.cycle.has(index)) {
      // Cycle leg: violet, fully visible; the upward leg dashes — solid vs
      // dashed is the direction cue (explicit opacity 1 inline, per the
      // edge contract; no dimmed variants).
      flowEdge.style = {
        stroke: GRAPH_LOOP,
        opacity: 1,
        ...(dashed ? { strokeDasharray: '6 4' } : {}),
      };
      flowEdge.className = 'workflow-edge-loop';
      if (roles.loopBadge.has(index)) {
        // The badge on the source node carries the target; the edge
        // itself is de-emphasized. The label is for screen readers.
        flowEdge.ariaLabel = `${edge.from} loops back to ${edge.to}`;
      }
    } else if (roles.back.has(index)) {
      // Layered back edge without a cycle: an upward return (failure
      // convergence), dashed.
      flowEdge.style = { stroke: GRAPH_BACK, strokeDasharray: '6 4' };
      flowEdge.className = 'workflow-edge-back';
    }
    return flowEdge;
  });
  return { nodes, edges };
}

/** "2/5" while iterating; the aggregate outcome once the loop completes. */
function formatProgress(progress: ForEachProgress): string | undefined {
  if (progress.outcome) {
    return `${progress.outcome}${progress.total !== null ? ` (${progress.total})` : ''}`;
  }
  if (progress.total === null && progress.started === 0) return undefined;
  return `${progress.started}/${progress.total ?? '?'}`;
}

/** Declared iteration control of a step, e.g. `for_each · ["a", "b"]`. */
function iterationBadge(iteration: NonNullable<WorkflowGraphNode['iteration']>): string {
  const items = iteration.items ? ` · ${truncate(iteration.items)}` : '';
  return `${iteration.control}${items}`;
}
