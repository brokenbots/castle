import type { WorkflowGraph } from './parseWorkflowHcl';

export const NODE_X_GAP = 240;
export const NODE_Y_GAP = 110;

/**
 * Graph reading direction. `top-bottom` layers top-to-bottom (the original
 * rendering); `left-right` transposes the same layered layout so layers run
 * left-to-right (CRI-257 orientation toggle).
 */
export type GraphOrientation = 'top-bottom' | 'left-right';

export interface NodePosition {
  x: number;
  y: number;
  layer: number;
}

/**
 * Layered (Sugiyama-lite) layout: breadth-first layers from the workflow's
 * `initial_state`, with nodes unreachable from it layered from their own
 * roots (in declaration order) so nothing is dropped. Nodes are centered
 * across the intra-layer axis per layer. Deterministic: the result depends
 * only on the graph.
 */
export function layoutWorkflow(
  graph: WorkflowGraph,
  { xGap = NODE_X_GAP, yGap = NODE_Y_GAP, orientation = 'top-bottom' }: { xGap?: number; yGap?: number; orientation?: GraphOrientation } = {},
): Map<string, NodePosition> {
  const adjacency = new Map<string, string[]>();
  const edgesBySource = new Map<string, string[]>();
  for (const node of graph.nodes) adjacency.set(node.id, []);
  for (const edge of graph.edges) {
    let targets = edgesBySource.get(edge.from);
    if (!targets) {
      targets = [];
      edgesBySource.set(edge.from, targets);
    }
    targets.push(edge.to);
  }

  const layerOf = new Map<string, number>();
  const order: string[] = [];
  const queue: string[] = [];
  const visit = (id: string, layer: number): void => {
    if (layerOf.has(id)) return;
    layerOf.set(id, layer);
    order.push(id);
    queue.push(id);
  };

  const roots = graph.nodes.map((n) => n.id);
  const start = graph.startAt && adjacency.has(graph.startAt) ? graph.startAt : roots[0];
  if (start) visit(start, 0);
  while (queue.length > 0) {
    const id = queue.shift()!;
    const nextLayer = layerOf.get(id)! + 1;
    for (const target of edgesBySource.get(id) ?? []) visit(target, nextLayer);
    if (queue.length === 0) {
      // Layer everything not reached from the previous roots (cycles or
      // disconnected subgraphs) before draining this frontier.
      const root = roots.find((r) => !layerOf.has(r));
      if (root) visit(root, 0);
    }
  }

  const byLayer = new Map<number, string[]>();
  for (const id of order) {
    const layer = layerOf.get(id)!;
    let bucket = byLayer.get(layer);
    if (!bucket) {
      bucket = [];
      byLayer.set(layer, bucket);
    }
    bucket.push(id);
  }

  const positions = new Map<string, NodePosition>();
  for (const [layer, ids] of byLayer) {
    ids.forEach((id, index) => {
      if (orientation === 'left-right') {
        // Transposed: the layer axis runs along x, the intra-layer spread
        // along y.
        positions.set(id, {
          x: layer * xGap,
          y: index * yGap - ((ids.length - 1) / 2) * yGap,
          layer,
        });
      } else {
        positions.set(id, { x: index * xGap - ((ids.length - 1) / 2) * xGap, y: layer * yGap, layer });
      }
    });
  }
  return positions;
}