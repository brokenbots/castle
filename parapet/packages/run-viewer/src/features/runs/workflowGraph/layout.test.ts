import { describe, expect, test } from 'vitest';
import { classifyEdges, layoutWorkflow, NODE_X_GAP, NODE_Y_GAP } from './layout';
import linearSource from './fixtures/linear_intake_v1.chcl?raw';
import { parseWorkflowHcl, type WorkflowGraph } from './parseWorkflowHcl';

const diamond: WorkflowGraph = {
  name: 'w',
  startAt: 'a',
  nodes: [
    { id: 'a', kind: 'step' },
    { id: 'b', kind: 'step' },
    { id: 'c', kind: 'state' },
  ],
  edges: [
    { from: 'a', to: 'b', via: 'success' },
    { from: 'a', to: 'c', via: 'skip' },
  ],
};

describe('layoutWorkflow', () => {
  test('layers top-to-bottom by default: layers on y, spread on x', () => {
    const positions = layoutWorkflow(diamond);
    const a = positions.get('a')!;
    const b = positions.get('b')!;
    const c = positions.get('c')!;
    expect(a.y).toBe(0);
    expect(b.y).toBe(NODE_Y_GAP);
    expect(c.y).toBe(NODE_Y_GAP);
    // Layer-1 siblings spread symmetrically around the origin on x.
    expect(b.x).toBe(-NODE_X_GAP / 2);
    expect(c.x).toBe(NODE_X_GAP / 2);
    expect(a.x).toBe(0);
  });

  test('left-right transposes: layers on x, spread on y', () => {
    const positions = layoutWorkflow(diamond, { orientation: 'left-right' });
    const a = positions.get('a')!;
    const b = positions.get('b')!;
    const c = positions.get('c')!;
    expect(a.x).toBe(0);
    expect(b.x).toBe(NODE_X_GAP);
    expect(c.x).toBe(NODE_X_GAP);
    // Layer-1 siblings spread symmetrically around the origin on y.
    expect(b.y).toBe(-NODE_Y_GAP / 2);
    expect(c.y).toBe(NODE_Y_GAP / 2);
    expect(a.y).toBe(0);
  });

  test('layer indices are preserved across orientations', () => {
    for (const positions of [
      layoutWorkflow(diamond),
      layoutWorkflow(diamond, { orientation: 'left-right' }),
    ]) {
      expect(positions.get('a')!.layer).toBe(0);
      expect(positions.get('b')!.layer).toBe(1);
      expect(positions.get('c')!.layer).toBe(1);
    }
  });
});

describe('classifyEdges', () => {
  test('classifies only the upward leg of a cycle as a layered back edge, both legs as cycle edges', () => {
    // A review-shaped loop: build -> review -> build.
    const loop: WorkflowGraph = {
      name: 'w',
      startAt: 'build',
      nodes: [
        { id: 'build', kind: 'step' },
        { id: 'review', kind: 'step' },
      ],
      edges: [
        { from: 'build', via: 'success', to: 'review' },
        { from: 'review', via: 'revise', to: 'build' },
      ],
    };
    const roles = classifyEdges(loop);
    // Both edges are legs of the cycle; only review -> build sweeps back
    // up (build sits on layer 0, review on layer 1), so it alone carries
    // the collapsed loop badge.
    expect(roles.back).toEqual(new Set([1]));
    expect(roles.cycle).toEqual(new Set([0, 1]));
    expect(roles.loopBadge).toEqual(new Set([1]));
  });

  test('classifies self-loops as back, cycle and badge edges', () => {
    const selfLoop: WorkflowGraph = {
      name: 'w',
      startAt: 'retry',
      nodes: [{ id: 'retry', kind: 'step' }],
      edges: [{ from: 'retry', via: 'again', to: 'retry' }],
    };
    const roles = classifyEdges(selfLoop);
    expect(roles.back).toEqual(new Set([0]));
    expect(roles.cycle).toEqual(new Set([0]));
    expect(roles.loopBadge).toEqual(new Set([0]));
  });

  test('classifies a plain acyclic diamond with no special roles', () => {
    const roles = classifyEdges(diamond);
    expect(roles.back).toEqual(new Set());
    expect(roles.cycle).toEqual(new Set());
    expect(roles.loopBadge).toEqual(new Set());
  });

  test('classifies an upward return as a back edge but not a cycle edge', () => {
    // classify -> failed style returns: the target never reaches the
    // source, so nothing loops back.
    const returns: WorkflowGraph = {
      name: 'w',
      startAt: 'start',
      nodes: [
        { id: 'start', kind: 'step' },
        { id: 'failed', kind: 'state' },
        { id: 'deep', kind: 'step' },
      ],
      edges: [
        { from: 'start', via: 'failure', to: 'failed' },
        { from: 'start', via: 'success', to: 'deep' },
        { from: 'deep', via: 'failure', to: 'failed' },
      ],
    };
    const roles = classifyEdges(returns);
    expect(roles.back).toEqual(new Set([2]));
    expect(roles.cycle).toEqual(new Set());
    expect(roles.loopBadge).toEqual(new Set());
  });

  test('classifies an edge whose target sits on the same layer (visited-guard artifact)', () => {
    // c is reached from a before the b -> c edge expands, so BFS layers b
    // and c on the same layer: the layered layout cannot draw b -> c as a
    // forward step, and it renders with the back-edge class (see the doc
    // comment on classifyEdges).
    const flat: WorkflowGraph = {
      name: 'w',
      startAt: 'a',
      nodes: [
        { id: 'a', kind: 'step' },
        { id: 'b', kind: 'step' },
        { id: 'c', kind: 'step' },
      ],
      edges: [
        { from: 'a', via: 'success', to: 'b' },
        { from: 'a', via: 'skip', to: 'c' },
        { from: 'b', via: 'redo', to: 'c' },
      ],
    };
    const positions = layoutWorkflow(flat);
    expect(positions.get('b')!.layer).toBe(1);
    expect(positions.get('c')!.layer).toBe(1);
    const roles = classifyEdges(flat);
    expect(roles.back).toEqual(new Set([2]));
    expect(roles.cycle).toEqual(new Set());
    expect(roles.loopBadge).toEqual(new Set());
  });

  test('classifies a cycle entered through a long path, not sibling returns', () => {
    const chain: WorkflowGraph = {
      name: 'w',
      startAt: 'a',
      nodes: [
        { id: 'a', kind: 'step' },
        { id: 'b', kind: 'step' },
        { id: 'c', kind: 'step' },
        { id: 'd', kind: 'step' },
        { id: 'failed', kind: 'state' },
      ],
      edges: [
        { from: 'a', via: 'success', to: 'b' },
        { from: 'a', via: 'failure', to: 'failed' },
        { from: 'b', via: 'success', to: 'c' },
        { from: 'c', via: 'success', to: 'd' },
        { from: 'd', via: 'revise', to: 'b' },
        { from: 'd', via: 'failure', to: 'failed' },
      ],
    };
    // d -> b closes the b-c-d loop; d -> failed is an upward return, not a
    // loop. All three legs of the loop are cycle edges (each target
    // reaches its source around the loop).
    const roles = classifyEdges(chain);
    expect(roles.cycle).toEqual(new Set([2, 3, 4]));
    expect(roles.back).toEqual(new Set([4, 5]));
    expect(roles.loopBadge).toEqual(new Set([4]));
  });

  test('edges to nodes that are not laid out are not classified', () => {
    const dangling: WorkflowGraph = {
      name: 'w',
      startAt: 'a',
      nodes: [{ id: 'a', kind: 'step' }],
      edges: [{ from: 'a', via: 'success', to: 'ghost' }],
    };
    const roles = classifyEdges(dangling);
    expect(roles.back).toEqual(new Set());
    expect(roles.cycle).toEqual(new Set());
    expect(roles.loopBadge).toEqual(new Set());
  });

  test('intake fixture: upward returns are back edges, though the workflow is acyclic', () => {
    // The intake workflow has no cycles, but many steps converge on the
    // early-layered shared `failed`/`set_review_state` nodes, so their
    // edges target an earlier layer — exactly the long sweeping edges item
    // 2 de-emphasizes. The cycle classification separates these from real
    // loops: the fixture has none.
    const graph = parseWorkflowHcl(linearSource);
    expect(graph.edges.length).toBeGreaterThan(0);
    const roles = classifyEdges(graph);
    expect(roles.back.size).toBeGreaterThan(0);
    expect(roles.cycle).toEqual(new Set());
    expect(roles.loopBadge).toEqual(new Set());
  });
});