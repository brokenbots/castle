import { parseHclDocument, WorkflowParseError } from './hcl';
import type { HclBlock, HclRange, HclValue } from './hcl';

export { WorkflowParseError } from './hcl';

/**
 * A node in the workflow graph parsed from HCL. `kind` mirrors the top-level
 * node block types of the criteria workflow language; `target` marks
 * transition destinations the source never declares (rendered as
 * placeholders, which also covers the engine's implicit `_error` terminal).
 */
export type WorkflowNodeKind = 'step' | 'switch' | 'wait' | 'approval' | 'state' | 'target';

export interface WorkflowGraphNode {
  id: string;
  kind: WorkflowNodeKind;
  /** switch nodes: declared arms in declaration order. */
  arms?: { condition: string; target: string }[];
  /**
   * step nodes only: the iteration control declared on the step
   * (`for_each`/`count`/`parallel`/`while`); `items` keeps the raw
   * expression for display. The step itself is the iterating node — the
   * event stream keys per-iteration events on the step name.
   */
  iteration?: { control: 'for_each' | 'count' | 'parallel' | 'while'; items?: string };
  /**
   * step nodes only: name of the subworkflow layer the step runs, when its
   * `target = subworkflow.<name>` traversal crosses into a subworkflow
   * declaration; the UI matches it against workflow.graphs event layers
   * (CRI-257 drill-down).
   */
  subworkflow?: string;
  /** state nodes only. */
  terminal?: boolean;
  /** state nodes only. */
  success?: boolean;
  /**
   * Exact source range of the node's declaration block, from its header to
   * just past its closing brace; absent for placeholder targets (CRI-257).
   */
  sourceRange?: HclRange;
}

/** A transition edge: `from` moved to `to` via the named outcome. */
export interface WorkflowGraphEdge {
  from: string;
  to: string;
  via: string;
}

export interface WorkflowGraph {
  name: string;
  /** `initial_state` of the workflow, when declared. */
  startAt: string | null;
  nodes: WorkflowGraphNode[];
  edges: WorkflowGraphEdge[];
}

/** Traversals that may appear as an outcome's `next` target. */
const TRAVERSAL_QUALIFIERS = ['step', 'state', 'wait', 'switch', 'approval', 'subworkflow'];

/**
 * Strips the traversal qualifier (`step.`/`state.`/…) from a `next`
 * traversal so node ids match the bare names the event stream carries
 * (`StepOutcome.step`, `StepTransition.from/to`, `WaitEntered.node`, …).
 * Exported for the compiled-module parser, whose JSON bodies carry bare
 * node names already but are stripped the same way for parity.
 */
export function traversalTarget(raw: string): string {
  const name = raw.trim();
  const match = new RegExp(`^(?:${TRAVERSAL_QUALIFIERS.join('|')})\\.(.+)$`).exec(name);
  return match ? match[1] : name;
}

/**
 * Resolves a traversal attribute (`next`, `initial_state`) to its target
 * text: quoted strings resolve to their unquoted value, raw traversals
 * (`next = step.a`) to their captured expression text.
 */
function traversalValue(v?: HclValue): string {
  return valueString(v) ?? valueRaw(v) ?? '';
}

/**
 * Parses the criteria workflow HCL language into a step-graph model.
 *
 * Hand-rolled recursive-descent reader over the generic HCL block shape
 * (see {@link parseHclDocument}) plus this grammar mapping from the
 * criteria workflow language (docs/LANGUAGE-SPEC.md): executable nodes are
 * top-level content declarations — `step`/`state`/`switch`/`wait`/`approval`
 * blocks, siblings of the unlabelled `workflow` header block whose `name`
 * and `initial_state` attributes name the graph and its start node.
 * Transitions are `outcome "<name>" { next = <traversal> }` blocks
 * (`step.x`, `state.y`, `wait.w`, `switch.s`, `approval.a`,
 * `subworkflow.n`); switches branch via `match { condition, next }` /
 * `default { next }`; steps iterate via step-level `for_each`/`count`/
 * `parallel`/`while` attributes. Other declarations (`variable`, `local`,
 * `data`, `adapter`, `subworkflow`, `environment`, `output`,
 * `permissions`, `policy`) parse generically and are ignored.
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

  const addNode = (id: string, kind: WorkflowNodeKind, sourceRange?: HclRange): void => {
    if (!nodes.has(id)) nodes.set(id, { id, kind, sourceRange });
  };
  const addEdge = (from: string, via: string, to: string): void => {
    if (!from || !to) return;
    const key = `${from} ${via} ${to}`;
    if (seenEdges.has(key)) return;
    seenEdges.add(key);
    edges.push({ from, via, to });
  };

  /** `next = <traversal>` targets of `outcome` blocks. */
  const collectOutcomes = (block: HclBlock, from: string): void => {
    for (const sub of block.blocks) {
      if (sub.type !== 'outcome') continue;
      const via = sub.labels[0] ?? '';
      const next = traversalTarget(traversalValue(sub.attrs.get('next')));
      if (via && next) addEdge(from, via, next);
    }
  };

  for (const block of doc) {
    const id = block.labels[0] ?? '';
    switch (block.type) {
      case 'step': {
        if (!id) break;
        addNode(id, 'step', block.range);
        const node = nodes.get(id)!;
        const control = ITERATION_CONTROLS.find((name) => block.attrs.has(name));
        if (control) {
          node.iteration = { control, items: valueRaw(block.attrs.get(control)) };
        }
        // `target = subworkflow.<name>` marks the step as running a
        // subworkflow layer; other target namespaces (adapter.*,
        // data.*, …) are ignored here.
        const target = traversalValue(block.attrs.get('target'));
        const sub = /^subworkflow\.([A-Za-z0-9_.-]+)$/.exec(target);
        if (sub) node.subworkflow = sub[1];
        collectOutcomes(block, id);
        break;
      }
      case 'switch': {
        if (!id) break;
        addNode(id, 'switch', block.range);
        const node = nodes.get(id)!;
        node.arms = [];
        let armIndex = 0;
        for (const sub of block.blocks) {
          if (sub.type === 'match') {
            const condition = valueRaw(sub.attrs.get('condition')) ?? '';
            const next = traversalTarget(traversalValue(sub.attrs.get('next')));
            if (!next) continue;
            node.arms.push({ condition, target: next });
            // Arm edges are labelled by declaration order ("arm[<index>]"); the
            // default arm is labelled "default".
            addEdge(id, `arm[${armIndex}]`, next);
            armIndex++;
          } else if (sub.type === 'default') {
            const next = traversalTarget(traversalValue(sub.attrs.get('next')));
            if (!next) continue;
            node.arms.push({ condition: '', target: next });
            addEdge(id, 'default', next);
          }
        }
        break;
      }
      case 'wait':
      case 'approval': {
        if (!id) break;
        addNode(id, block.type, block.range);
        collectOutcomes(block, id);
        break;
      }
      case 'state': {
        if (!id) break;
        addNode(id, 'state', block.range);
        const node = nodes.get(id)!;
        node.terminal = valueBool(block.attrs.get('terminal'));
        node.success = valueBool(block.attrs.get('success'));
        break;
      }
      default:
        // workflow/variable/local/data/adapter/subworkflow/… are not graph
        // nodes; they parse generically above and are ignored here.
        break;
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
    name: valueString(workflow.attrs.get('name')) ?? '',
    startAt: traversalTarget(traversalValue(workflow.attrs.get('initial_state'))) || null,
    nodes: [...nodes.values()],
    edges,
  };
}

const ITERATION_CONTROLS = ['for_each', 'count', 'parallel', 'while'] as const;

function valueString(v?: HclValue): string | null {
  return v?.kind === 'string' ? v.value : null;
}

function valueRaw(v?: HclValue): string | undefined {
  if (v?.kind === 'string') return v.text;
  if (v?.kind === 'raw') return v.text;
  if (v?.kind === 'number') return v.text;
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
