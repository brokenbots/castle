import { useMemo } from 'react';
import type { MouseEvent } from 'react';
import {
  Background,
  Handle,
  Position,
  ReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import type { WorkflowGraph, WorkflowGraphNode, WorkflowNodeKind } from './parseWorkflowHcl';
import { layoutWorkflow } from './layout';
import type { ForEachProgress, StepNodeStatus } from './nodeStatus';

export interface WorkflowDagProps {
  graph: WorkflowGraph;
  /** Live overlay state per node id (absent = unvisited). */
  statuses?: Record<string, StepNodeStatus>;
  /** Per-iteration progress per for_each node id. */
  forEachProgress?: Record<string, ForEachProgress>;
  /** Currently selected node id, if any. */
  selectedId?: string | null;
  /** Called with the node id when a node is clicked. */
  onSelect?: (nodeId: string) => void;
}

interface WorkflowNodeData extends Record<string, unknown> {
  node: WorkflowGraphNode;
  status: StepNodeStatus;
  /** for_each iteration badge text, e.g. "2/5". */
  badge?: string;
  /** Currently selected node (highlight ring). */
  selected: boolean;
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

function WorkflowNodeView({ data }: NodeProps<WorkflowFlowNode>) {
  const { node, status, badge, selected } = data;
  const selectedClass = selected ? ' ring-2 ring-sky-400' : '';
  return (
    <div
      data-testid="dag-node"
      data-node-id={node.id}
      className={`rounded-lg border bg-slate-900/90 px-3 py-2 text-center shadow min-w-[8rem] max-w-[15rem] ${STATUS_CLASS[status]}${selectedClass}`}
    >
      <Handle type="target" position={Position.Top} className="!h-1.5 !w-1.5 !border-0 !bg-slate-500" />
      <p className="font-mono text-xs text-slate-100 break-all">{node.id}</p>
      <p className={`mt-0.5 text-[10px] uppercase tracking-wide font-semibold ${KIND_CLASS[node.kind]}`}>
        {KIND_LABEL[node.kind]}
      </p>
      {badge && <p className="text-[10px] font-mono text-slate-400">{badge}</p>}
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
      <Handle type="source" position={Position.Bottom} className="!h-1.5 !w-1.5 !border-0 !bg-slate-500" />
    </div>
  );
}

const nodeTypes = { workflow: WorkflowNodeView };

export function WorkflowDag({ graph, statuses = {}, forEachProgress = {}, selectedId, onSelect }: WorkflowDagProps) {
  const { nodes, edges } = useMemo(
    () => buildFlow(graph, statuses, forEachProgress, selectedId ?? null),
    [graph, statuses, forEachProgress, selectedId],
  );

  const handleNodeClick = onSelect
    ? (_event: MouseEvent, node: WorkflowFlowNode) => onSelect(node.id)
    : undefined;

  return (
    <div
      data-testid="workflow-dag"
      className="h-[38vh] min-h-[280px] bg-slate-900 rounded border border-slate-800 overflow-hidden"
    >
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodeClick={handleNodeClick}
        nodesConnectable={false}
        zoomOnScroll={false}
        fitView
        fitViewOptions={{ padding: 0.15 }}
        minZoom={0.2}
        proOptions={{ hideAttribution: true }}
      >
        <Background color="#1e293b" gap={16} />
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
): { nodes: WorkflowFlowNode[]; edges: Edge[] } {
  const positions = layoutWorkflow(graph);
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
      data: { node, status: statuses[node.id] ?? 'idle', badge, selected: node.id === selectedId },
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
