import type { WorkflowGraph } from './parseWorkflowHcl';

export const NODE_X_GAP = 240;
export const NODE_Y_GAP = 110;

/** BFS layering result: layer index and visit order per node id. */
interface Layers {
  layerOf: Map<string, number>;
  order: string[];
}

/**
 * Breadth-first layers from the workflow's `startAt`, with nodes
 * unreachable from it layered from their own roots (in declaration order)
 * so nothing is dropped. Shared by position layout and back-edge
 * classification, which must agree on layering.
 */
function computeLayers(graph: WorkflowGraph): Layers {
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

  return { layerOf, order };
}

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
 * Layered (Sugiyama-lite) layout: breadth-first layers (see
 * {@link computeLayers}), with nodes centered across the intra-layer axis
 * per layer. Deterministic: the result depends only on the graph.
 */
export function layoutWorkflow(
  graph: WorkflowGraph,
  { xGap = NODE_X_GAP, yGap = NODE_Y_GAP, orientation = 'top-bottom' }: { xGap?: number; yGap?: number; orientation?: GraphOrientation } = {},
): Map<string, NodePosition> {
  const { layerOf, order } = computeLayers(graph);

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

/**
 * Per-edge classification for first-class cyclic-graph rendering, indexed
 * by position in `graph.edges`:
 *
 * - `back`: the target layers at or before the source under the layered
 *   layout (see {@link computeLayers}) — the transitions the layout cannot
 *   draw as a forward step: cycle-closing back edges (self-loops included)
 *   and returns the BFS visited guard parked upward or on the same layer
 *   (deep nodes converging on an early-layered shared target, e.g. a
 *   shared `failed` state). Rendered de-emphasized.
 * - `cycle`: the target can reach the source, so the edge is a leg of a
 *   cycle — the loop-closing transition itself or a forward leg of one.
 *   Rendered with the distinct loop color.
 * - `loopBadge`: cycle edges whose target layers at or before the source;
 *   their source node carries the collapsed "loops back to X" badge (a
 *   cycle's forward legs do not badge). An acyclic workflow has empty
 *   `cycle`/`loopBadge` sets even when many edges are back edges.
 *
 * Layers are orientation-independent, so the classification is too.
 */
export interface EdgeRoles {
  back: Set<number>;
  cycle: Set<number>;
  loopBadge: Set<number>;
}

export function classifyEdges(graph: WorkflowGraph): EdgeRoles {
  const { layerOf } = computeLayers(graph);

  const adjacency = new Map<string, string[]>();
  for (const edge of graph.edges) {
    const targets = adjacency.get(edge.from);
    if (targets) targets.push(edge.to);
    else adjacency.set(edge.from, [edge.to]);
  }
  const reachCache = new Map<string, Set<string>>();
  const reachableFrom = (v: string): Set<string> => {
    let seen = reachCache.get(v);
    if (!seen) {
      seen = new Set([v]);
      const queue = [v];
      while (queue.length > 0) {
        const id = queue.shift()!;
        for (const next of adjacency.get(id) ?? []) {
          if (!seen.has(next)) {
            seen.add(next);
            queue.push(next);
          }
        }
      }
      reachCache.set(v, seen);
    }
    return seen;
  };

  const back = new Set<number>();
  const cycle = new Set<number>();
  const loopBadge = new Set<number>();
  graph.edges.forEach((edge, index) => {
    const from = layerOf.get(edge.from);
    const to = layerOf.get(edge.to);
    const nonForward = from !== undefined && to !== undefined && to <= from;
    if (nonForward) back.add(index);
    if (reachableFrom(edge.to).has(edge.from)) {
      cycle.add(index);
      if (nonForward) loopBadge.add(index);
    }
  });
  return { back, cycle, loopBadge };
}