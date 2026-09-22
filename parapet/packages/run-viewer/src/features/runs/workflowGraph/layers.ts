import type { EventEnvelope } from '../../../api/castleApi';
import { parseWorkflowHcl, type WorkflowGraph } from './parseWorkflowHcl';
import { parseCompiledModuleBody } from './parseCompiledModule';

/**
 * Subworkflow drill-down data (CRI-257). Castle carries the compiled
 * subworkflow layers of the run's workflow through the `workflow.graphs`
 * event (protojson camelCase: name/sourcePath/body). Each layer body is the
 * subworkflow's compiled module JSON — the `criteria compile --format json`
 * `subworkflows[].body` shape (CRI-294): the emitter serializes the
 * already-compiled graph rather than re-compiling HCL. Bodies are parsed
 * with {@link parseCompiledModuleBody}; HCL module source (older producers)
 * still parses with the workflow parser.
 */
export interface SubworkflowLayer {
  /** Matches the parent module's `subworkflow "<name>"` declaration. */
  name: string;
  /** Module path the parent declared for the subworkflow; display-only. */
  sourcePath: string;
  /** Layer body as the event carries it (compiled module JSON or HCL). */
  body: string;
  /** True when the body is the compiled module JSON (CRI-294 contract). */
  compiledJson: boolean;
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
    let compiledJson = false;
    let graph: WorkflowGraph | null = null;
    try {
      // The emitter ships the compiled module JSON (CRI-294); HCL module
      // source is the fallback for producers that predate the contract.
      const compiled = parseCompiledModuleBody(entry.body);
      if (compiled) {
        compiledJson = true;
        graph = compiled;
      } else if (entry.body) {
        graph = parseWorkflowHcl(entry.body);
      }
    } catch {
      // Unparseable layer body: keep the layer, drop the graph. A parser
      // crash (e.g. pathological nesting) must not blank the page.
      graph = null;
    }
    layers.push({
      name: entry.name,
      sourcePath: typeof entry.sourcePath === 'string' ? entry.sourcePath : '',
      body: entry.body,
      compiledJson,
      graph,
    });
  }
  return layers;
}

/**
 * Source-pane text for a layer (CRI-294): HCL bodies render as-is;
 * compiled module JSON renders pretty-printed — the emitter ships the
 * body as a compact JSON string, and a one-line blob is unreadable in the
 * pane. Falls back to the raw body when re-parsing fails.
 */
export function layerSourceText(layer: SubworkflowLayer): string {
  if (!layer.compiledJson) return layer.body;
  try {
    return `${JSON.stringify(JSON.parse(layer.body), null, 2)}\n`;
  } catch {
    return layer.body;
  }
}