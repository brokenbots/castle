import { describe, expect, test } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import { buildSubworkflowLayers, selectWorkflowGraphs } from './layers';

function envelope(
  type: string,
  payload: unknown,
  overrides: Partial<EventEnvelope> = {},
): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq: 1,
    type,
    ts: '2026-09-21T00:00:00Z',
    correlationId: '',
    payload,
    ...overrides,
  };
}

const LAYER_BODY = 'workflow {\n  name = "qa_triage"\n  initial_state = "triage"\n}\nstep "triage" {\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';

describe('selectWorkflowGraphs', () => {
  test('returns null when no workflowGraphs event exists', () => {
    expect(selectWorkflowGraphs([envelope('stepEntered', { step: 'build' })])).toBeNull();
    expect(selectWorkflowGraphs([])).toBeNull();
  });

  test('ignores malformed payloads', () => {
    expect(selectWorkflowGraphs([envelope('workflowGraphs', { nope: true })])).toBeNull();
    expect(selectWorkflowGraphs([envelope('workflowGraphs', 'string')])).toBeNull();
    expect(selectWorkflowGraphs([envelope('workflowGraphs', null)])).toBeNull();
  });

  test('the most recent workflowGraphs event wins (a resend replaces the payload)', () => {
    const events = [
      envelope('stepEntered', { step: 'build' }),
      envelope('workflowGraphs', { subworkflows: [{ name: 'old', body: LAYER_BODY }] }, { seq: 2 }),
      envelope('workflowGraphs', { subworkflows: [{ name: 'new', body: LAYER_BODY }] }, { seq: 3 }),
    ];
    const payload = selectWorkflowGraphs(events);
    expect(payload?.subworkflows).toHaveLength(1);
    expect(payload?.subworkflows[0].name).toBe('new');
  });
});

describe('buildSubworkflowLayers', () => {
  test('parses each layer body with the workflow parser', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        { name: 'qa_triage', sourcePath: '../qa_triage_v1', body: LAYER_BODY },
      ],
    });
    expect(layers).toHaveLength(1);
    expect(layers[0].name).toBe('qa_triage');
    expect(layers[0].sourcePath).toBe('../qa_triage_v1');
    expect(layers[0].graph?.name).toBe('qa_triage');
    expect(layers[0].graph?.nodes.map((n) => n.id)).toEqual(['triage', 'done']);
    expect(layers[0].graph?.startAt).toBe('triage');
  });

  test('a body that does not parse keeps the layer with a null graph', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [{ name: 'broken', body: 'workflow { this is not hcl }' }],
    });
    expect(layers).toHaveLength(1);
    expect(layers[0].graph).toBeNull();
    // The source pane can still show the raw body.
    expect(layers[0].body).toContain('not hcl');
  });

  test('entries without a name or body are skipped', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        { body: LAYER_BODY },
        { name: 'no-body' },
        { name: 'ok', body: LAYER_BODY },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['ok']);
  });

  test('an empty body yields a null graph without throwing', () => {
    const layers = buildSubworkflowLayers({ subworkflows: [{ name: 'empty', body: '' }] });
    expect(layers[0].graph).toBeNull();
  });
});