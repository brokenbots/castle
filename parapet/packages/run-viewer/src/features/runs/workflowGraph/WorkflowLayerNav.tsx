import { useMemo } from 'react';
import { layoutWorkflow } from './layout';
import type { SubworkflowLayer } from './layers';

/**
 * Subworkflow drill-down navigation (CRI-257): a breadcrumb over the open
 * layer stack plus a thumbnail rail of the open layers. Thumbnails are
 * always laid out left-to-right regardless of the main graph's
 * orientation, so they read as consistent miniatures.
 */

export interface WorkflowLayerNavProps {
  /** Root workflow display name (the run's workflow). */
  rootName: string;
  /** Open layers from root → deepest. */
  stack: SubworkflowLayer[];
  /** Navigates to a stack depth: 0 returns to the root workflow. */
  onNavigate: (depth: number) => void;
}

/** Thumbnail node box + spacing (logical SVG units). */
const THUMB_NODE_W = 96;
const THUMB_NODE_H = 28;
const THUMB_X_GAP = 44;
const THUMB_Y_GAP = 20;

/** Static left-to-right miniature of a layer graph: plain SVG, no ReactFlow. */
export function WorkflowLayerThumb({ layer }: { layer: SubworkflowLayer }) {
  const thumb = useMemo(() => {
    if (!layer.graph || layer.graph.nodes.length === 0) return null;
    const positions = layoutWorkflow(layer.graph, {
      orientation: 'left-right',
      xGap: THUMB_NODE_W + THUMB_X_GAP,
      yGap: THUMB_NODE_H + THUMB_Y_GAP,
    });
    const coords = layer.graph.nodes.map((node) => {
      const p = positions.get(node.id) ?? { x: 0, y: 0 };
      return { id: node.id, x: p.x, y: p.y };
    });
    // Layer positions center each layer around 0 (y can be negative);
    // normalize everything to a top-left-origin viewBox.
    const minX = Math.min(...coords.map((c) => c.x));
    const minY = Math.min(...coords.map((c) => c.y));
    for (const c of coords) {
      c.x -= minX;
      c.y -= minY;
    }
    const width = Math.max(...coords.map((c) => c.x)) + THUMB_NODE_W;
    const height = Math.max(...coords.map((c) => c.y)) + THUMB_NODE_H;
    const byId = new Map(coords.map((c) => [c.id, c]));
    const edges = layer.graph.edges
      .map((edge, i) => {
        const from = byId.get(edge.from);
        const to = byId.get(edge.to);
        if (!from || !to) return null;
        return {
          key: `t${i}`,
          x1: from.x + THUMB_NODE_W,
          y1: from.y + THUMB_NODE_H / 2,
          x2: to.x,
          y2: to.y + THUMB_NODE_H / 2,
        };
      })
      .filter((e): e is NonNullable<typeof e> => e !== null);
    return { coords, edges, width, height };
  }, [layer]);

  if (!thumb) {
    return <span className="text-[10px] text-slate-500">no graph</span>;
  }
  return (
    <svg
      viewBox={`0 0 ${thumb.width} ${thumb.height}`}
      className="h-10 w-full"
      role="img"
      aria-label={`${layer.name} thumbnail`}
    >
      {thumb.edges.map((edge) => (
        <line
          key={edge.key}
          x1={edge.x1}
          y1={edge.y1}
          x2={edge.x2}
          y2={edge.y2}
          stroke="#475569"
          strokeWidth={1.5}
        />
      ))}
      {thumb.coords.map((node) => (
        <g key={node.id}>
          <rect
            x={node.x}
            y={node.y}
            width={THUMB_NODE_W}
            height={THUMB_NODE_H}
            rx={4}
            fill="#0f172a"
            stroke="#334155"
          />
          <text
            x={node.x + THUMB_NODE_W / 2}
            y={node.y + THUMB_NODE_H / 2 + 3}
            textAnchor="middle"
            fill="#94a3b8"
            fontSize={9}
            fontFamily="monospace"
          >
            {node.id}
          </text>
        </g>
      ))}
    </svg>
  );
}

export function WorkflowLayerNav({ rootName, stack, onNavigate }: WorkflowLayerNavProps) {
  const deepest = stack.length;
  return (
    <div className="space-y-1">
      <nav aria-label="Subworkflow layers" data-testid="layer-breadcrumb">
        <ol className="flex items-center gap-1 text-xs text-ink-muted flex-wrap">
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
      <div className="flex gap-2" data-testid="layer-thumbs">
        {stack.map((layer, index) => (
          <button
            key={layer.name}
            type="button"
            data-testid="layer-thumb"
            title={`Open ${layer.name}`}
            aria-pressed={index + 1 === deepest}
            onClick={() => onNavigate(index + 1)}
            className="w-40 rounded border border-slate-700 bg-slate-900 p-1 hover:border-sky-500"
          >
            <WorkflowLayerThumb layer={layer} />
            <p className="truncate text-[10px] text-slate-400">{layer.name}</p>
          </button>
        ))}
      </div>
    </div>
  );
}