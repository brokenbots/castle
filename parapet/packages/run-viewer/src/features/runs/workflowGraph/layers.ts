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

/**
 * One subworkflow entry as the wire carries it — a top-level
 * `payload.subworkflows` member (protojson camelCase) or an entry inlined in
 * a layer body's own `subworkflows` key (CRI-296, compiled module JSON
 * snake_case). Both spellings of the display-only source path are accepted.
 */
interface SubworkflowEntry {
  name?: unknown;
  sourcePath?: unknown;
  source_path?: unknown;
  body?: unknown;
}

interface WorkflowGraphsPayload {
  subworkflows: SubworkflowEntry[];
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
 * Parses each subworkflow layer body into its graph and registers every
 * layer the payload carries, recursively (CRI-296): compiled module JSON
 * bodies inline the layers their own steps reference in a `subworkflows`
 * key, and those must land in the built map too, or the affordances inside
 * an opened layer gray out. Entries missing a name or body are skipped at
 * every level; a body that does not parse keeps the layer visible with
 * `graph: null` (the drill-down stays closed for it) instead of dropping
 * the entry — the source pane can still show it.
 */
export function buildSubworkflowLayers(payload: WorkflowGraphsPayload): SubworkflowLayer[] {
  const layers: SubworkflowLayer[] = [];
  // Layer names are module-scoped and unique across the compiled tree in
  // practice; a repeated name collapses to its shallowest occurrence so the
  // name-keyed layer map in the run view resolves deterministically.
  const seen = new Set<string>();
  // Breadth-first walk: every entry at one depth registers before any
  // deeper one, so first-wins == shallowest-wins. Termination is
  // structural — a nested body is a substring of its parent's body, so the
  // nesting depth is bounded by the payload itself.
  let frontier = payload.subworkflows;
  while (frontier.length > 0) {
    const nested: SubworkflowEntry[] = [];
    for (const entry of frontier) {
      if (typeof entry?.name !== 'string' || entry.name === '' || typeof entry?.body !== 'string') {
        continue;
      }
      if (seen.has(entry.name)) continue;
      seen.add(entry.name);
      const { compiledJson, graph } = parseLayerBody(entry.body);
      layers.push({
        name: entry.name,
        sourcePath: readSourcePath(entry),
        body: entry.body,
        compiledJson,
        graph,
      });
      // The layers this body inlines ride its own `subworkflows` key;
      // walk them with the same rules. HCL bodies (older producers) and
      // bodies that do not JSON-parse inline nothing.
      nested.push(...inlinedEntries(entry.body));
    }
    frontier = nested;
  }
  return layers;
}

function parseLayerBody(body: string): { compiledJson: boolean; graph: WorkflowGraph | null } {
  let compiledJson = false;
  let graph: WorkflowGraph | null = null;
  try {
    // The emitter ships the compiled module JSON (CRI-294); HCL module
    // source is the fallback for producers that predate the contract.
    const compiled = parseCompiledModuleBody(body);
    if (compiled) {
      compiledJson = true;
      graph = compiled;
    } else if (body) {
      graph = parseWorkflowHcl(body);
    }
  } catch {
    // Unparseable layer body: keep the layer, drop the graph. A parser
    // crash (e.g. pathological nesting) must not blank the page.
    graph = null;
  }
  return { compiledJson, graph };
}

function readSourcePath(entry: SubworkflowEntry): string {
  if (typeof entry.sourcePath === 'string') return entry.sourcePath;
  if (typeof entry.source_path === 'string') return entry.source_path;
  return '';
}

/**
 * The subworkflow entries a layer body inlines (CRI-296): compiled module
 * JSON may carry a `subworkflows` key listing the layers its own steps
 * reference, in the same shape as the top-level payload. Bodies that do not
 * JSON-parse to an object with that key — HCL source, garbage — inline
 * nothing.
 */
function inlinedEntries(body: string): SubworkflowEntry[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return [];
  }
  if (typeof parsed !== 'object' || parsed === null) return [];
  const subworkflows = (parsed as { subworkflows?: unknown }).subworkflows;
  return Array.isArray(subworkflows) ? (subworkflows as SubworkflowEntry[]) : [];
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