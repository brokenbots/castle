import { useEffect, useMemo, useRef } from 'react';
import type { MouseEvent } from 'react';
import {
  Background,
  Handle,
  Panel,
  Position,
  ReactFlow,
  useReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import type { WorkflowGraph, WorkflowGraphNode, WorkflowNodeKind } from './parseWorkflowHcl';
import { layoutWorkflow, type GraphOrientation } from './layout';
import type { ForEachProgress, StepNodeStatus } from './nodeStatus';

export interface WorkflowDagProps {
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

const STATUS_CLASS: Record<StepNodeStatus, string> = {
  idle: 'border-slate-700 opacity-60',
  running: 'border-sky-400 ring-2 ring-sky-400/40 animate-pulse',
  succeeded: 'border-emerald-400/70',
  failed: 'border-rose-500',
};

const KIND_LABEL: Record<WorkflowGraphNode['kind'], string> = {
  step: 'step',
  switch: 'switch',
  wait: 'wait',
  approval: 'approval',
  state: 'state',
  target: 'target',
};

const KIND_CLASS: Record<WorkflowNodeKind, string> = {
  step: 'text-sky-300',
  switch: 'text-amber-300',
  wait: 'text-slate-300',
  approval: 'text-slate-300',
  state: 'text-emerald-300/80',
  target: 'text-slate-500',
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

function WorkflowNodeView({ data }: NodeProps<WorkflowFlowNode>) {
  const { node, status, badge, selected, orientation, explore } = data;
  const handle = HANDLE_POSITION[orientation];
  const selectedClass = selected ? ' ring-2 ring-sky-400' : '';
  return (
    <div
      data-testid="dag-node"
      data-node-id={node.id}
      className={`rounded-lg border bg-slate-900/90 px-3 py-2 text-center shadow min-w-[8rem] max-w-[15rem] ${STATUS_CLASS[status]}${selectedClass}`}
    >
      <Handle type="target" position={handle.target} className="!h-1.5 !w-1.5 !border-0 !bg-slate-500" />
      <p className="font-mono text-xs text-slate-100 break-all">{node.id}</p>
      <p className={`mt-0.5 text-[10px] uppercase tracking-wide font-semibold ${KIND_CLASS[node.kind]}`}>
        {KIND_LABEL[node.kind]}
      </p>
      {badge && <p className="text-[10px] font-mono text-slate-400">{badge}</p>}
      {explore && (
        <button
          type="button"
          data-testid="dag-node-explore"
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
          className="mt-1 w-full rounded border border-sky-500/40 px-1 py-0.5 text-[10px] font-medium text-sky-300 hover:bg-sky-400/10 disabled:cursor-not-allowed disabled:border-slate-700 disabled:text-slate-600"
        >
          ⤷ {explore.name}
        </button>
      )}
      <p className="text-xs" aria-label={`status ${status}`}>
        <span
          className={
            status === 'succeeded'
              ? 'text-emerald-400'
              : status === 'failed'
                ? 'text-rose-400'
                : status === 'running'
                  ? 'text-sky-300'
                  : 'text-slate-600'
          }
        >
          {STATUS_MARK[status]}
        </span>
      </p>
      <Handle type="source" position={handle.source} className="!h-1.5 !w-1.5 !border-0 !bg-slate-500" />
    </div>
  );
}

const nodeTypes = { workflow: WorkflowNodeView };

/**
 * In-graph behavior mounted inside <ReactFlow> so it can reach the store:
 * pans/zooms to the followed node when follow mode is live, and offers the
 * reset control that restores the full-graph framing.
 */
function DagBehavior({ followStepId }: { followStepId: string | null }) {
  const { fitView } = useReactFlow();
  const followedRef = useRef<string | null>(null);
  useEffect(() => {
    if (!followStepId || followedRef.current === followStepId) return;
    followedRef.current = followStepId;
    void fitView({ nodes: [{ id: followStepId }], duration: 400, padding: 2 });
  }, [followStepId, fitView]);
  return (
    <Panel position="top-right">
      <button
        type="button"
        data-testid="dag-reset-view"
        title="Reset graph view"
        onClick={() => {
          // Clear the follow memo so a re-entered node can re-center.
          followedRef.current = null;
          void fitView({ duration: 200, padding: 0.15 });
        }}
        className="rounded border border-slate-600 bg-slate-900/90 px-2 py-1 text-xs text-slate-300 hover:bg-slate-800"
      >
        Reset view
      </button>
    </Panel>
  );
}

export function WorkflowDag({ graph, statuses = {}, forEachProgress = {}, selectedId, orientation = 'top-bottom', followStepId, onSelect, onExploreLayer, exploreableLayers }: WorkflowDagProps) {
  const { nodes, edges } = useMemo(
    () => buildFlow(graph, statuses, forEachProgress, selectedId ?? null, orientation, onExploreLayer, exploreableLayers),
    [graph, statuses, forEachProgress, selectedId, orientation, onExploreLayer, exploreableLayers],
  );

  const handleNodeClick = onSelect
    ? (_event: MouseEvent, node: WorkflowFlowNode) => onSelect(node.id === selectedId ? null : node.id)
    : undefined;

  return (
    <div
      data-testid="workflow-dag"
      className="h-[38vh] min-h-[280px] bg-slate-900 rounded border border-slate-800 overflow-hidden"
    >
      <ReactFlow
        key={orientation}
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodeClick={handleNodeClick}
        nodesConnectable={false}
        zoomOnScroll
        fitView
        fitViewOptions={{ padding: 0.15 }}
        minZoom={0.2}
        proOptions={{ hideAttribution: true }}
      >
        <Background color="#1e293b" gap={16} />
        <DagBehavior followStepId={followStepId ?? null} />
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
  onExploreLayer?: (name: string) => void,
  exploreableLayers?: Set<string>,
): { nodes: WorkflowFlowNode[]; edges: Edge[] } {
  const positions = layoutWorkflow(graph, { orientation });
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
  const edges: Edge[] = graph.edges.map((edge, index) => ({
    id: `e${index}`,
    source: edge.from,
    target: edge.to,
    label: truncate(edge.via),
    type: 'smoothstep',
    style: { stroke: '#475569' },
    labelStyle: { fill: '#cbd5e1', fontSize: 10 },
    labelBgStyle: { fill: '#0f172a' },
    labelBgPadding: [4, 2],
    labelBgBorderRadius: 3,
  }));
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
