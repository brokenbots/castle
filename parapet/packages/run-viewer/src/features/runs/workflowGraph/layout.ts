import type { WorkflowGraph } from './parseWorkflowHcl';

export const NODE_X_GAP = 300;
export const NODE_Y_GAP = 140;

/** Forward adjacency built once per graph. */
function buildAdjacency(graph: WorkflowGraph): Map<string, string[]> {
  const edgesBySource = new Map<string, string[]>();
  for (const edge of graph.edges) {
    const targets = edgesBySource.get(edge.from);
    if (targets) targets.push(edge.to);
    else edgesBySource.set(edge.from, [edge.to]);
  }
  return edgesBySource;
}

/** Nodes reachable from `v` along forward edges (memoized per call site). */
function reachableSet(v: string, adjacency: Map<string, string[]>, cache: Map<string, Set<string>>): Set<string> {
  let seen = cache.get(v);
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
    cache.set(v, seen);
  }
  return seen;
}

/** BFS seed layering result: layer index and visit order per node id. */
interface Layers {
  layerOf: Map<string, number>;
  order: string[];
}

/**
 * Breadth-first seed layers from the workflow's `startAt` (nodes
 * unreachable from it layered from their own roots, in declaration order,
 * so nothing is dropped). Seeding pass only — layers are refined afterwards
 * (see {@link computeLayers}).
 */
function bfsLayers(graph: WorkflowGraph, edgesBySource: Map<string, string[]>): Layers {
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
  const start = graph.startAt && roots.includes(graph.startAt) ? graph.startAt : roots[0];
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

export function computeLayers(graph: WorkflowGraph): Layers {
  const edgesBySource = buildAdjacency(graph);
  const { layerOf, order } = bfsLayers(graph, edgesBySource);

  const reachCache = new Map<string, Set<string>>();
  const reaches = (v: string): Set<string> => reachableSet(v, edgesBySource, reachCache);

  // Longest-path relaxation over the acyclic edge set: every edge whose
  // target cannot reach its source (non-cycle by reachability) pushes its
  // target one layer past the deepest source seen. A push can chain (A→B
  // lifts B, which lifts C on the next sweep), and each sweep strictly
  // increases at least one node's layer, so the process terminates within
  // nodeCount/2 sweeps; the edge-count bound is generous headroom.
  for (let sweep = 0; sweep < graph.edges.length + 1; sweep += 1) {
    let changed = false;
    for (const edge of graph.edges) {
      const from = layerOf.get(edge.from);
      const to = layerOf.get(edge.to);
      const isCycleLeg = from !== undefined && to !== undefined && reaches(edge.to).has(edge.from);
      if (from === undefined || to === undefined || isCycleLeg) continue;
      if (from + 1 > to) {
        layerOf.set(edge.to, from + 1);
        changed = true;
      }
    }
    if (!changed) break;
  }

  return { layerOf, order };
}

/**
 * Graph reading direction. `left-right` layers run left-to-right — the
 * default: the entry node sits leftmost, terminals end on the right.
 * `top-bottom` renders the same layering transposed (CRI-257 toggle).
 */
export type GraphOrientation = 'top-bottom' | 'left-right';

export interface NodePosition {
  x: number;
  y: number;
  layer: number;
}

/**
 * Layered (Sugiyama-lite) layout. Layers come from {@link computeLayers}
 * (BFS seed + longest-path relaxation): paths stretch to their full
 * length, shared targets land at the deepest layer they are entered from,
 * and the graph reads entry-left → terminal-right in the default
 * left-right orientation. Nodes are centered across the intra-layer axis
 * per layer. Deterministic: the result depends only on the graph.
 */
export function layoutWorkflow(
  graph: WorkflowGraph,
  { xGap = NODE_X_GAP, yGap = NODE_Y_GAP, orientation = 'left-right' }: { xGap?: number; yGap?: number; orientation?: GraphOrientation } = {},
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
        // The layer axis runs along x (entry leftmost), the intra-layer
        // spread along y.
        positions.set(id, {
          x: layer * xGap,
          y: index * yGap - ((ids.length - 1) / 2) * yGap,
          layer,
        });
      } else {
        // Transposed: layers top-to-bottom, spread on x.
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
 *   and upward returns to nodes layered early by their other entries (a
 *   shared `failed` state). Longest-path layering keeps these to genuine
 *   returns. Rendered de-emphasized.
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

  const adjacency = buildAdjacency(graph);
  const reachCache = new Map<string, Set<string>>();
  const reachableFrom = (v: string): Set<string> => reachableSet(v, adjacency, reachCache);

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