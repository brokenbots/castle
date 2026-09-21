import { describe, expect, test } from 'vitest';
import { layoutWorkflow, NODE_X_GAP, NODE_Y_GAP } from './layout';
import type { WorkflowGraph } from './parseWorkflowHcl';

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