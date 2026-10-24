import ELK from 'elkjs/lib/elk-api.js';
import type { WorkflowGraph } from './parseWorkflowHcl';
import { layoutWorkflow, type GraphOrientation, type NodePosition } from './layout';

/**
 * ELK-backed graph layout (the layout engine xyflow's docs point at):
 * layered algorithm with orthogonal, node-avoiding edge routing — the
 * hand-rolled BFS/relaxation layout plus the "edges through the boxes"
 * problem are replaced by the real engine. Runs in a web worker
 * (elkjs's bundled worker) through a Vite URL import.
 *
 * Output:
 * - `positions`: top-left node positions in flow coordinates (the same
 *   shape the legacy layout produced, so the graph wiring barely changes).
 * - `edgePoints`: per edge index, the orthogonal polyline (absolute flow
 *   coordinates) the custom polyline edge renders — ELK guarantees the
 *   route keeps clear of the node boxes.
 *
 * Environments without Worker (jsdom) fall back to the legacy sync
 * layout: positions only, plain smoothstep routing — every existing test
 * contract stays valid there, and the browser always gets the real
 * engine.
 */

// Declared node size fed to ELK — MUST equal the card's rendered box
// (WorkflowNodeView's NODE_BOX_W/H). Fixed boxes make ELK's edge anchors
// and React Flow's handle positions coincide by construction: every edge
// visibly connects, no measurement feedback needed.
export const ELK_NODE_W = 208;
export const ELK_NODE_H = 96;

// Spacing in flow units: betweenLayers is the column gap (x in LR), the
// other the intra-layer gap (y in LR). The wide-flat bias (big column gap,
// tight rows) keeps every graph — shallow chains and wide switch fan-outs
// alike — reading in the operator's horizontal flow direction; a tall
// fan-out layer otherwise dominates the height and the graph reads square.
const BETWEEN_LAYERS_GAP = 190;
const SAME_LAYER_GAP = 28;

interface ElkLayoutResult {
  positions: Map<string, NodePosition>;
  /** Orthogonal route per edge (indexed like graph.edges). */
  edgePoints: Map<number, Array<{ x: number; y: number }>>;
}

let elkInstance: InstanceType<typeof ELK> | null = null;

function getElk(): InstanceType<typeof ELK> | null {
  if (typeof Worker === 'undefined') return null;
  if (!elkInstance) {
    // The worker URL is resolved by Vite (webworker bundling); the API's
    // typings want a string path.
    elkInstance = new ELK({
      workerUrl: new URL('elkjs/lib/elk-worker.min.js', import.meta.url).toString(),
    } as ConstructorParameters<typeof ELK>[0]);
  }
  return elkInstance;
}

/** Legacy BFS+relaxation layout stands in where Workers do not exist. */
function fallbackLayout(graph: WorkflowGraph, orientation: GraphOrientation): ElkLayoutResult {
  const positions = layoutWorkflow(graph, { orientation });
  return { positions, edgePoints: new Map() };
}

export async function layoutWithElk(
  graph: WorkflowGraph,
  { orientation = 'left-right', sizes }: { orientation?: GraphOrientation; sizes?: Map<string, { w: number; h: number }> } = {},
): Promise<ElkLayoutResult> {
  const elk = getElk();
  if (!elk) return fallbackLayout(graph, orientation);

  const elkGraph = {
    id: 'root',
    layoutOptions: {
      'elk.algorithm': 'layered',
      'elk.direction': orientation === 'left-right' ? 'RIGHT' : 'DOWN',
      'elk.edgeRouting': 'ORTHOGONAL',
      // Longest-path layering: every path stretches to full length, so the
      // graph reads wide (entry leftmost → terminals rightmost) like the
      // operator expects; ELK's default network-simplex packs the same
      // graph much squarer.
      'elk.layered.layering.strategy': 'LONGEST_PATH',
      'elk.spacing.nodeNode': String(SAME_LAYER_GAP),
      'elk.layered.spacing.nodeNodeBetweenLayers': String(BETWEEN_LAYERS_GAP),
      // Distinguishable parallel legs: each edge gets its own routing lane
      // (no merged bundles) so the per-arm labels sit on separable lines.
      'elk.layered.unnecessaryBendpoints': 'false',
      // Deterministic without crossing heuristics churn: keep model order.
      'elk.layered.considerModelOrder.strategy': 'NODES_AND_EDGES',
    },
    children: graph.nodes.map((node) => {
      const s = sizes?.get(node.id);
      return {
        id: node.id,
        width: s?.w ?? ELK_NODE_W,
        height: s?.h ?? ELK_NODE_H,
      };
    }),
    edges: graph.edges.map((edge, index) => ({
      id: `e${index}`,
      sources: [edge.from],
      targets: [edge.to],
    })),
  };

  try {
    // The layout call's result is ELK's JSON graph; the library types the
    // echo narrowly (input shape), so the response widens locally.
    const laid = (await elk.layout(elkGraph)) as typeof elkGraph & {
      children?: Array<{ id?: unknown; x?: number; y?: number }>;
      edges?: Array<{
        id?: unknown;
        sections?: Array<{
          startPoint?: { x: number; y: number };
          endPoint?: { x: number; y: number };
          bendPoints?: Array<{ x: number; y: number }>;
        }>;
      }>;
    };
    const positions = new Map<string, NodePosition>();
    const edgePoints = new Map<number, Array<{ x: number; y: number }>>();
    for (const child of laid.children ?? []) {
      if (typeof child.id !== 'string' || child.x === undefined || child.y === undefined) continue;
      // ELK returns x/y of the node's top-left; layer index derives from
      // the coordinate (LR: x, TB: y) for the back-edge classifier.
      const layerCoord = orientation === 'left-right' ? child.x : child.y;
      const gap = orientation === 'left-right' ? BETWEEN_LAYERS_GAP + ELK_NODE_W : BETWEEN_LAYERS_GAP + ELK_NODE_H;
      positions.set(child.id, { x: child.x, y: child.y, layer: Math.round(layerCoord / gap) });
    }
    for (const edge of laid.edges ?? []) {
      if (typeof edge.id !== 'string') continue;
      const index = Number(edge.id.slice(1));
      if (!Number.isInteger(index)) continue;
      const points: Array<{ x: number; y: number }> = [];
      for (const section of edge.sections ?? []) {
        if (!points.length && section.startPoint) points.push(section.startPoint);
        for (const bend of section.bendPoints ?? []) points.push(bend);
        if (section.endPoint) points.push(section.endPoint);
      }
      if (points.length >= 2) edgePoints.set(index, points);
    }
    // Nodes the layerer dropped still need positions (dangling-end edges
    // etc.): fall back to the legacy layout for exactly those.
    for (const node of graph.nodes) {
      if (!positions.has(node.id)) {
        const legacy = fallbackLayout(graph, orientation);
        return {
          positions: new Map([...legacy.positions, ...positions]),
          edgePoints,
        };
      }
    }
    return { positions, edgePoints };
  } catch {
    // Any ELK failure (worker startup, pathological graph) degrades to the
    // legacy layout instead of blanking the graph.
    return fallbackLayout(graph, orientation);
  }
}