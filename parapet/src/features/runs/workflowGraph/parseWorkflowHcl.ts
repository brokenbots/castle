import { parseHclDocument, WorkflowParseError } from './hcl';
import type { HclBlock, HclValue } from './hcl';

export { WorkflowParseError } from './hcl';

/**
 * A node in the workflow graph parsed from HCL. `kind` mirrors the node
 * block types of the criteria workflow language; `target` marks transition
 * destinations the source never declares (rendered as placeholders).
 */
export type WorkflowNodeKind = 'step' | 'branch' | 'for_each' | 'wait' | 'approval' | 'state' | 'target';

export interface WorkflowGraphNode {
  id: string;
  kind: WorkflowNodeKind;
  /** Declared arms of a branch node, in declaration order. */
  arms?: { condition: string; target: string }[];
  /** for_each payload: the items expression and the child ("do") step. */
  forEach?: { items?: string; do?: string };
  /** state nodes only. */
  terminal?: boolean;
  /** state nodes only. */
  success?: boolean;
}

/** A transition edge: `from` moved to `to` via the named outcome. */
export interface WorkflowGraphEdge {
  from: string;
  to: string;
  via: string;
}

export interface WorkflowGraph {
  name: string;
  /** `start_at` / `initial_state` of the workflow, when declared. */
  startAt: string | null;
  nodes: WorkflowGraphNode[];
  edges: WorkflowGraphEdge[];
}

/**
 * Parses the criteria workflow HCL language into a step-graph model.
 *
 * Hand-rolled recursive-descent parser over the known HCL shapes (criteria
 * workflow reference): a `workflow` block containing `step` /
 * `branch`(alias `switch`) / `for_each` / `wait` / `approval` / `state`
 * nodes whose transitions are declared either as `outcome "<name>" {
 * transition_to = "<target>" }` blocks or as a `transitions = { "<outcome>"
 * = "<target>" }` map. Only this node/transition grammar is modeled;
 * unrelated blocks (`variable`, `agent`, `input`, …) parse generically and
 * are ignored.
 *
 * Throws {@link WorkflowParseError} when the source is not parseable
 * workflow HCL; callers must keep their fallback rendering in that case.
 */
export function parseWorkflowHcl(source: string): WorkflowGraph {
  if (typeof source !== 'string') {
    throw new WorkflowParseError('workflow source is not a string');
  }
  const doc = parseHclDocument(source);
  const workflow = doc.find((b) => b.type === 'workflow');
  if (!workflow) {
    throw new WorkflowParseError('workflow source contains no "workflow" block');
  }

  const nodes = new Map<string, WorkflowGraphNode>();
  const edges: WorkflowGraphEdge[] = [];
  const seenEdges = new Set<string>();
  // Child ("do") step -> declaring for_each node, used to resolve the
  // synthetic `_continue` target back to the loop it belongs to.
  const doOwners = new Map<string, string>();

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

  /** Outcome edges, shared by every executable node shape. */
  const collectOutcomes = (block: HclBlock, from: string): void => {
    for (const sub of block.blocks) {
      if (sub.type !== 'outcome') continue;
      const via = sub.labels[0] ?? '';
      const to = valueString(sub.attrs.get('transition_to'));
      if (via && to) addEdge(from, via, to);
    }
    const transitions = block.attrs.get('transitions');
    if (transitions?.kind === 'map') {
      for (const key of transitions.keys) {
        const to = valueString(transitions.entries.get(key));
        if (key && to) addEdge(from, key, to);
      }
    }
  };

  for (const block of workflow.blocks) {
    const id = block.labels[0] ?? '';
    if (!id) continue;
    switch (block.type) {
      case 'step':
      case 'wait':
      case 'approval': {
        addNode(id, block.type as WorkflowGraphNode['kind']);
        collectOutcomes(block, id);
        break;
      }
      case 'branch':
      case 'switch': {
        addNode(id, 'branch');
        const node = nodes.get(id)!;
        node.arms = [];
        let armIndex = 0;
        for (const sub of block.blocks) {
          const to = valueString(sub.attrs.get('transition_to'));
          if (!to) continue;
          if (sub.type === 'arm') {
            const condition = valueRaw(sub.attrs.get('when')) ?? '';
            node.arms.push({ condition, target: to });
            addEdge(id, condition || `arm[${armIndex}]`, to);
          } else if (sub.type === 'default') {
            node.arms.push({ condition: '', target: to });
            addEdge(id, 'default', to);
          }
          armIndex++;
        }
        collectOutcomes(block, id);
        break;
      }
      case 'for_each': {
        addNode(id, 'for_each');
        const node = nodes.get(id)!;
        const doStep = valueString(block.attrs.get('do'));
        node.forEach = {
          items: valueRaw(block.attrs.get('items')),
          do: doStep ?? undefined,
        };
        if (doStep) {
          if (!doOwners.has(doStep)) doOwners.set(doStep, id);
          addEdge(id, 'do', doStep);
        }
        collectOutcomes(block, id);
        break;
      }
      case 'state': {
        addNode(id, 'state');
        const node = nodes.get(id)!;
        node.terminal = valueBool(block.attrs.get('terminal'));
        node.success = valueBool(block.attrs.get('success'));
        collectOutcomes(block, id);
        break;
      }
      default:
        break;
    }
  }

  // `_continue` is the engine-internal "advance the loop cursor" target;
  // render it as the loop-back edge into the for_each node that owns the step.
  for (const edge of edges) {
    if (edge.to === '_continue' && doOwners.has(edge.from)) {
      edge.to = doOwners.get(edge.from)!;
    }
  }

  // Transition targets the source never declares render as placeholders so
  // every edge attaches to a node.
  for (const edge of edges) {
    for (const id of [edge.from, edge.to]) {
      if (!nodes.has(id)) addNode(id, 'target');
    }
  }

  return {
    name: workflow.labels[0] ?? '',
    startAt: valueString(workflow.attrs.get('start_at') ?? workflow.attrs.get('initial_state')),
    nodes: [...nodes.values()],
    edges,
  };
}

function valueString(v?: HclValue): string | null {
  return v?.kind === 'string' ? v.value : null;
}

function valueRaw(v?: HclValue): string | undefined {
  if (v?.kind === 'string') return v.text;
  if (v?.kind === 'raw') return v.text;
  if (v?.kind === 'list') {
    const items = v.items.map((item) => {
      if (item.kind === 'string') return `"${item.value}"`;
      if (item.kind === 'bool') return item.value ? 'true' : 'false';
      if (item.kind === 'list') return valueRaw(item) ?? '[]';
      if (item.kind === 'map') return '{}';
      return item.text;
    });
    return `[${items.join(', ')}]`;
  }
  return undefined;
}

function valueBool(v?: HclValue): boolean | undefined {
  return v?.kind === 'bool' ? v.value : undefined;
}

/**
 * Regex fallback used when HCL parsing fails, matching the original
 * RunDetailPage behavior: step blocks and their string-map transitions.
 */
export interface TextEdge {
  from: string;
  to: string;
  via: string;
}

export function extractTextEdges(source: string): TextEdge[] {
  const edges: TextEdge[] = [];
  const stepBlocks = source.match(/step\s+"[^"]+"\s*\{[\s\S]*?\n\}/g) ?? [];
  for (const block of stepBlocks) {
    const stepName = block.match(/step\s+"([^"]+)"/)?.[1];
    if (!stepName) continue;
    for (const tr of block.matchAll(/"([^"]+)"\s*=\s*"([^"]+)"/g)) {
      edges.push({ from: stepName, via: tr[1], to: tr[2] });
    }
  }
  return edges;
}