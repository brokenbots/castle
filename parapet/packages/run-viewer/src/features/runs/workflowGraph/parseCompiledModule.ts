import type { WorkflowGraph, WorkflowGraphEdge, WorkflowGraphNode, WorkflowNodeKind } from './parseWorkflowHcl';
import { traversalTarget } from './parseWorkflowHcl';

/**
 * The compiled module JSON shape the workflow.graphs emitter ships as each
 * layer's body — the same content `criteria compile --format json` renders
 * for `subworkflows[].body` (snake_case keys; steps/states/switches arrays;
 * bare node names in `initial_state` and every `next`). Waits and approvals
 * are not part of the compiled output, so edges that target one resolve to
 * placeholder target nodes — the same rendering the HCL parser gives
 * undeclared targets.
 */
interface CompiledModuleJson {
  name?: unknown;
  initial_state?: unknown;
  steps?: unknown;
  states?: unknown;
  switches?: unknown;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

function asBool(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined;
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

/**
 * Parses a workflow.graphs layer body carrying the emitter's compiled module
 * JSON into the same graph model the HCL parser produces: steps (with the
 * subworkflow layer a step runs) and states become nodes, step `outcomes`
 * become via-labelled edges, switches become arm-labelled nodes whose edges
 * are labelled `arm[<index>]` in declaration order plus `default`, and the
 * compiled `initial_state` names the start node. Transition targets the
 * module never declares render as placeholder targets.
 *
 * Returns null when the body is not compiled module JSON (a non-object, or
 * an object without the module shape — a `name` plus a `steps` array), so
 * callers can fall back to the HCL parser.
 */
export function parseCompiledModuleBody(body: string): WorkflowGraph | null {
  if (typeof body !== 'string' || !body.trimStart().startsWith('{')) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    return null;
  }
  if (!isRecord(parsed)) return null;
  const module = parsed as CompiledModuleJson;
  const name = asString(module.name);
  if (name === null || !Array.isArray(module.steps)) return null;

  const nodes = new Map<string, WorkflowGraphNode>();
  const edges: WorkflowGraphEdge[] = [];
  const seenEdges = new Set<string>();
  const addNode = (id: string, kind: WorkflowNodeKind): void => {
    if (!nodes.has(id)) nodes.set(id, { id, kind });
  };
  const addEdge = (from: string, via: string, to: string): void => {
    if (!from || !to) return;
    const key = `${from} ${via} ${to}`;
    if (seenEdges.has(key)) return;
    seenEdges.add(key);
    edges.push({ from, via, to });
  };

  for (const entry of asArray(module.steps)) {
    if (!isRecord(entry)) continue;
    const step = asString(entry.name);
    if (!step) continue;
    addNode(step, 'step');
    const node = nodes.get(step)!;
    const subworkflow = asString(entry.subworkflow);
    if (subworkflow) node.subworkflow = subworkflow;
    for (const outcome of asArray(entry.outcomes)) {
      if (!isRecord(outcome)) continue;
      const via = asString(outcome.name);
      const next = asString(outcome.next);
      if (!via || !next) continue;
      addEdge(step, via, traversalTarget(next));
    }
  }

  for (const entry of asArray(module.states)) {
    if (!isRecord(entry)) continue;
    const state = asString(entry.name);
    if (!state) continue;
    addNode(state, 'state');
    const node = nodes.get(state)!;
    node.terminal = asBool(entry.terminal);
    node.success = asBool(entry.success);
  }

  for (const entry of asArray(module.switches)) {
    if (!isRecord(entry)) continue;
    const swName = asString(entry.name);
    if (!swName) continue;
    addNode(swName, 'switch');
    const node = nodes.get(swName)!;
    node.arms = [];
    let armIndex = 0;
    for (const arm of asArray(entry.conditions)) {
      if (!isRecord(arm)) continue;
      const next = asString(arm.next);
      if (!next) continue;
      node.arms.push({ condition: asString(arm.match) ?? '', target: traversalTarget(next) });
      // Arm edges are labelled by declaration order ("arm[<index>]"); the
      // default arm is labelled "default" — matching the HCL parser.
      addEdge(swName, `arm[${armIndex}]`, traversalTarget(next));
      armIndex++;
    }
    const fallback = asString(entry.default_next);
    if (fallback) {
      node.arms.push({ condition: '', target: traversalTarget(fallback) });
      addEdge(swName, 'default', traversalTarget(fallback));
    }
  }

  // Transition targets the compiled module never declares (the engine's
  // implicit `_error` terminal, wait/approval nodes the compile output
  // omits) render as placeholders so every edge attaches to a node.
  for (const edge of edges) {
    for (const id of [edge.from, edge.to]) {
      if (!nodes.has(id)) addNode(id, 'target');
    }
  }

  const startAt = asString(module.initial_state);
  return {
    name,
    startAt: startAt ? traversalTarget(startAt) : null,
    nodes: [...nodes.values()],
    edges,
  };
}