import { useEffect, useState } from 'react';
import type { WorkflowGraph } from './parseWorkflowHcl';
import { layoutWithElk, ELK_NODE_W, ELK_NODE_H } from './elkLayout';
import { layoutWorkflow } from './layout';
import type { SubworkflowLayer } from './layers';

/**
 * Subworkflow drill-down navigation (CRI-257): a rail of mini-graphs above
 * the step graph. Minis share the main graph's layout engine (elkjs), so a
 * thumbnail reads as a true miniature of the same picture — identical
 * layered arrangement and the same node-avoiding orthogonal edge routes.
 * Worker-less environments (jsdom) fall back to the legacy sync geometry,
 * so the older test contracts keep passing.
 */

export interface WorkflowLayerNavProps {
  /** Root workflow display name (the run's workflow). */
  rootName: string;
  /** The root workflow graph itself (for the always-present miniature). */
  rootGraph: WorkflowGraph | null;
  /** Open layers from root → deepest. */
  stack: SubworkflowLayer[];
  /** Navigates to a stack depth: 0 returns to the root workflow. */
  onNavigate: (depth: number) => void;
  /**
   * Live engine-state tint per miniature: maps a stack chain (empty string
   * = the root graph, otherwise the layer name) to the node ids of that
   * graph currently carrying process state (running / last completed), so
   * the rail shows where the process is.
   */
  activeIdsByLayer?: Map<string, ReadonlySet<string>>;
}

interface ThumbGeometry {
  coords: Array<{ id: string; x: number; y: number }>;
  edges: Array<{ key: string; d: string }>;
  width: number;
  height: number;
}

/** Fallback (no Worker): legacy sync layout, straight mid-to-mid edges. */
function legacyThumbGeometry(graph: WorkflowGraph): ThumbGeometry {
  const NODE_W = 64;
  const NODE_H = 22;
  const positions = layoutWorkflow(graph, {
    orientation: 'left-right',
    xGap: NODE_W + 44,
    yGap: NODE_H + 20,
  });
  const coords = graph.nodes.map((node) => {
    const p = positions.get(node.id) ?? { x: 0, y: 0 };
    return { id: node.id, x: p.x, y: p.y };
  });
  if (coords.length === 0) {
    return { coords, edges: [], width: NODE_W, height: NODE_H };
  }
  const minX = Math.min(...coords.map((c) => c.x));
  const minY = Math.min(...coords.map((c) => c.y));
  for (const c of coords) {
    c.x -= minX;
    c.y -= minY;
  }
  const byId = new Map(coords.map((c) => [c.id, c]));
  const edges = graph.edges
    .map((edge, i) => {
      const from = byId.get(edge.from);
      const to = byId.get(edge.to);
      if (!from || !to) return null;
      return {
        key: `t${i}`,
        d: `M ${from.x + NODE_W} ${from.y + NODE_H / 2} L ${to.x} ${to.y + NODE_H / 2}`,
      };
    })
    .filter((e): e is NonNullable<typeof e> => e !== null);
  return {
    coords,
    edges,
    width: Math.max(...coords.map((c) => c.x)) + NODE_W,
    height: Math.max(...coords.map((c) => c.y)) + NODE_H,
  };
}

/** ELK thumbnail geometry: identical layout family + orthogonal routes. */
async function elkThumbGeometry(graph: WorkflowGraph): Promise<ThumbGeometry> {
  const { positions, edgePoints } = await layoutWithElk(graph, { orientation: 'left-right' });
  const coords = graph.nodes.map((node) => {
    const p = positions.get(node.id) ?? { x: 0, y: 0 };
    return { id: node.id, x: p.x, y: p.y };
  });
  if (coords.length === 0) {
    return { coords, edges: [], width: ELK_NODE_W, height: ELK_NODE_H };
  }
  // Normalize to a small top-left-origin viewBox (the same scale shift the
  // main graph's fitView performs visually).
  const minX = Math.min(...coords.map((c) => c.x));
  const minY = Math.min(...coords.map((c) => c.y));
  let width = 0;
  let height = 0;
  for (const c of coords) {
    c.x -= minX;
    c.y -= minY;
    width = Math.max(width, c.x + ELK_NODE_W);
    height = Math.max(height, c.y + ELK_NODE_H);
  }
  const edges = graph.edges
    .map((_edge, i) => {
      const points = edgePoints.get(i);
      if (!points || points.length < 2) return null;
      return {
        key: `t${i}`,
        d: points.map((p, j) => `${j === 0 ? 'M' : 'L'} ${p.x - minX} ${p.y - minY}`).join(' '),
      };
    })
    .filter((e): e is NonNullable<typeof e> => e !== null);
  return { coords, edges, width, height };
}

function useThumbGeometry(graph: WorkflowGraph | null | undefined): ThumbGeometry | null {
  const [thumb, setThumb] = useState<ThumbGeometry | null>(null);
  useEffect(() => {
    if (!graph || graph.nodes.length === 0) {
      setThumb(null);
      return;
    }
    if (typeof Worker === 'undefined') {
      setThumb(legacyThumbGeometry(graph));
      return;
    }
    let cancelled = false;
    setThumb(null);
    void elkThumbGeometry(graph).then((t) => {
      if (!cancelled) setThumb(t);
    });
    return () => {
      cancelled = true;
    };
  }, [graph]);
  return thumb;
}

/**
 * The miniature SVG: identical engine geometry to the main graph,
 * rendered as static SVG (no ReactFlow machinery).
 */
export function MiniGraph({
  graph,
  label,
  activeIds,
}: {
  graph: WorkflowGraph | null | undefined;
  label: string;
  activeIds?: ReadonlySet<string>;
}) {
  const thumb = useThumbGeometry(graph);
  if (!thumb) {
    return <div className="flex h-12 items-center justify-center text-[10px] text-ink-faint">no graph</div>;
  }
  return (
    <svg viewBox={`0 0 ${thumb.width} ${thumb.height}`} className="h-12 w-full" role="img" aria-label={label}>
      {thumb.edges.map((edge) => (
        <path key={edge.key} d={edge.d} fill="none" stroke="#475569" strokeWidth={1.2} />
      ))}
      {thumb.coords.map((node) => {
        const active = activeIds?.has(node.id) ?? false;
        return (
          <g key={node.id}>
            <rect
              x={node.x}
              y={node.y}
              width={ELK_NODE_W}
              height={ELK_NODE_H}
              rx={4}
              fill={active ? '#0ea5e9' : '#0f172a'}
              stroke={active ? '#7dd3fc' : '#334155'}
            />
            <text
              x={node.x + ELK_NODE_W / 2}
              y={node.y + ELK_NODE_H / 2 + 3}
              textAnchor="middle"
              fill={active ? '#e0f2fe' : '#94a3b8'}
              fontSize={8}
              fontFamily="monospace"
            >
              {node.id}
            </text>
          </g>
        );
      })}
    </svg>
  );
}

/** A layer thumbnail (same geometry engine as the root mini). */
export function WorkflowLayerThumb({
  layer,
  activeIds,
}: {
  layer: SubworkflowLayer;
  activeIds?: ReadonlySet<string>;
}) {
  return <MiniGraph graph={layer.graph} label={`${layer.name} thumbnail`} activeIds={activeIds} />;
}

export function WorkflowLayerNav({ rootName, rootGraph, stack, onNavigate, activeIdsByLayer }: WorkflowLayerNavProps) {
  const deepest = stack.length;
  return (
    <div className="space-y-1">
      {stack.length > 0 && (
        <nav aria-label="Subworkflow layers" data-testid="layer-breadcrumb">
          <ol className="flex flex-wrap items-center gap-1 text-xs text-ink-muted">
            <li>
              <button
                type="button"
                data-testid="layer-crumb-root"
                title="Back to the top-level workflow"
                onClick={() => onNavigate(0)}
                className={deepest === 0 ? 'text-ink font-semibold' : 'hover:text-ink'}
              >
                {rootName}
              </button>
            </li>
            {stack.map((layer, index) => (
              <li key={layer.name} className="flex items-center gap-1">
                <span aria-hidden="true">›</span>
                <button
                  type="button"
                  data-testid="layer-crumb"
                  title={layer.sourcePath ? `Open ${layer.name} (${layer.sourcePath})` : `Open ${layer.name}`}
                  aria-current={index + 1 === deepest ? 'page' : undefined}
                  onClick={() => onNavigate(index + 1)}
                  className={index + 1 === deepest ? 'text-ink font-semibold' : 'hover:text-ink'}
                >
                  {layer.name}
                </button>
              </li>
            ))}
          </ol>
        </nav>
      )}
      <div className="flex gap-2 overflow-x-auto" data-testid="layer-thumbs">
        {/* The root miniature is always present so the rail exists from first
            paint; clicking it returns to the root workflow. */}
        <button
          type="button"
          data-testid="layer-thumb-root"
          title={`Open ${rootName}`}
          aria-pressed={deepest === 0}
          onClick={() => onNavigate(0)}
          className="w-48 shrink-0 rounded border border-line-strong bg-surface p-1 hover:border-accent-strong"
        >
          <MiniGraph
            graph={rootGraph}
            label={`${rootName} thumbnail`}
            activeIds={activeIdsByLayer?.get('')}
          />
          <p className="truncate text-[10px] text-ink-muted">{rootName}</p>
        </button>
        {stack.map((layer, index) => (
          <button
            key={layer.name}
            type="button"
            data-testid="layer-thumb"
            title={`Open ${layer.name}`}
            aria-pressed={index + 1 === deepest}
            onClick={() => onNavigate(index + 1)}
            className="w-48 shrink-0 rounded border border-line-strong bg-surface p-1 hover:border-accent-strong"
          >
            <WorkflowLayerThumb layer={layer} activeIds={activeIdsByLayer?.get(layer.name)} />
            {/* Path from the root: the drill-down chain, deepest emphasized. */}
            <p className="truncate text-[10px] text-ink-faint">
              {rootName}
              {' › '}
              {stack.slice(0, index + 1).map((l) => l.name).join(' › ')}
            </p>
          </button>
        ))}
      </div>
    </div>
  );
}