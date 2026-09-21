import type { EventEnvelope } from '../../../api/castleApi';
import { parseWorkflowHcl, type WorkflowGraph } from './parseWorkflowHcl';

/**
 * Subworkflow drill-down data (CRI-257). Castle carries the compiled
 * subworkflow layers of the run's workflow through the `workflow.graphs`
 * event (protojson camelCase: name/sourcePath/body); each layer body is
 * the subworkflow's HCL module source in the same dialect as the
 * top-level workflow, parsed with the same parser.
 */
export interface SubworkflowLayer {
  /** Matches the parent module's `subworkflow "<name>"` declaration. */
  name: string;
  /** Module path the parent declared for the subworkflow; display-only. */
  sourcePath: string;
  /** Compiled module source; the layer's source pane content. */
  body: string;
  /** Parsed layer graph; null when the body does not parse. */
  graph: WorkflowGraph | null;
}

interface WorkflowGraphsPayload {
  subworkflows: { name?: unknown; sourcePath?: unknown; body?: unknown }[];
}

function isGraphsPayload(payload: unknown): payload is WorkflowGraphsPayload {
  if (typeof payload !== 'object' || payload === null) return false;
  const subworkflows = (payload as { subworkflows?: unknown }).subworkflows;
  return Array.isArray(subworkflows);
}

/**
 * Picks the most recent workflow.graphs event's payload: a resend replaces
 * the previous payload, so consumers use the last one per run.
 */
export function selectWorkflowGraphs(events: EventEnvelope[]): WorkflowGraphsPayload | null {
  let found: WorkflowGraphsPayload | null = null;
  for (const event of events) {
    if (event.type === 'workflowGraphs' && isGraphsPayload(event.payload)) found = event.payload;
  }
  return found;
}

/**
 * Parses each compiled subworkflow layer body into its graph. Entries
 * missing a name or body are skipped; a body that does not parse keeps
 * the layer visible with `graph: null` (the drill-down stays closed for
 * it) instead of dropping the entry — the source pane can still show it.
 */
export function buildSubworkflowLayers(payload: WorkflowGraphsPayload): SubworkflowLayer[] {
  const layers: SubworkflowLayer[] = [];
  for (const entry of payload.subworkflows) {
    if (typeof entry?.name !== 'string' || entry.name === '' || typeof entry?.body !== 'string') {
      continue;
    }
    let graph: WorkflowGraph | null = null;
    try {
      graph = entry.body ? parseWorkflowHcl(entry.body) : null;
    } catch {
      // Unparseable layer body: keep the layer, drop the graph. A parser
      // crash (e.g. pathological nesting) must not blank the page.
      graph = null;
    }
    layers.push({
      name: entry.name,
      sourcePath: typeof entry.sourcePath === 'string' ? entry.sourcePath : '',
      body: entry.body,
      graph,
    });
  }
  return layers;
}