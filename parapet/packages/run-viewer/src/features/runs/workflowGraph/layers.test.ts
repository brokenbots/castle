import { describe, expect, test } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import { buildSubworkflowLayers, layerSourceText, selectWorkflowGraphs } from './layers';

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

// The CRI-294 wire contract: the emitter ships each layer body as the
// compiled module JSON — the `criteria compile --format json` shape of
// subworkflows[].body, serialized as a compact JSON string.
const COMPILED_BODY = JSON.stringify({
  name: 'inner_task',
  initial_state: 'execute',
  target_state: 'complete',
  adapters: [{ type: 'shell', name: 'default', on_crash: 'fail', config_keys: null }],
  steps: [
    {
      name: 'execute',
      adapter: 'shell.default',
      input_keys: ['command'],
      allow_tools: null,
      outcomes: [
        { name: 'failure', next: 'complete' },
        { name: 'success', next: 'complete' },
      ],
    },
  ],
  states: [{ name: 'complete', terminal: true, success: true }],
  outputs: [],
  switches: [],
  step_order: ['execute'],
  plugins_required: ['shell'],
  metadata: { schema_version: 1 },
});

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
    expect(layers[0].compiledJson).toBe(false);
    expect(layers[0].graph?.name).toBe('qa_triage');
    expect(layers[0].graph?.nodes.map((n) => n.id)).toEqual(['triage', 'done']);
    expect(layers[0].graph?.startAt).toBe('triage');
  });

  // CRI-294 regression: the emitter's body is the compiled module JSON, not
  // HCL — it must parse into a graph carrying the layer's steps/states so
  // the drill-down opens.
  test('parses a compiled-JSON layer body into the layer graph', () => {
    const payload = selectWorkflowGraphs([
      envelope('workflowGraphs', {
        subworkflows: [
          { name: 'inner_task', sourcePath: '/tmp/cri286-fixture/subworkflows/inner', body: COMPILED_BODY },
        ],
      }),
    ])!;
    const layers = buildSubworkflowLayers(payload);
    expect(layers).toHaveLength(1);
    expect(layers[0].name).toBe('inner_task');
    expect(layers[0].compiledJson).toBe(true);
    expect(layers[0].graph?.name).toBe('inner_task');
    expect(layers[0].graph?.startAt).toBe('execute');
    expect(layers[0].graph?.nodes.map((n) => n.id)).toEqual(['execute', 'complete']);
    expect(layers[0].graph?.nodes.map((n) => n.kind)).toEqual(['step', 'state']);
    expect(layers[0].graph?.edges.map((e) => e.via)).toEqual(['failure', 'success']);
  });

  test('an empty layers payload builds no layers (subworkflow-less runs)', () => {
    expect(buildSubworkflowLayers({ subworkflows: [] })).toEqual([]);
    expect(buildSubworkflowLayers(selectWorkflowGraphs([envelope('workflowGraphs', { subworkflows: [] })])!)).toEqual([]);
  });

  test('a body that does not parse keeps the layer with a null graph', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [{ name: 'broken', body: 'workflow { this is not hcl }' }],
    });
    expect(layers).toHaveLength(1);
    expect(layers[0].compiledJson).toBe(false);
    expect(layers[0].graph).toBeNull();
    // The source pane can still show the raw body.
    expect(layers[0].body).toContain('not hcl');
  });

  test('a JSON body without the module shape keeps the layer with a null graph', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [{ name: 'not-a-module', body: '{"foo": 1}' }],
    });
    expect(layers).toHaveLength(1);
    expect(layers[0].graph).toBeNull();
    expect(layers[0].body).toBe('{"foo": 1}');
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
    expect(layers[0].compiledJson).toBe(false);
  });
});

describe('layerSourceText', () => {
  test('renders HCL bodies as-is', () => {
    const layers = buildSubworkflowLayers({ subworkflows: [{ name: 'hcl', body: LAYER_BODY }] });
    expect(layerSourceText(layers[0])).toBe(LAYER_BODY);
  });

  test('pretty-prints compiled-JSON bodies so the pane stays readable', () => {
    const layers = buildSubworkflowLayers({ subworkflows: [{ name: 'inner_task', body: COMPILED_BODY }] });
    const text = layerSourceText(layers[0]);
    expect(JSON.parse(text)).toEqual(JSON.parse(COMPILED_BODY));
    expect(text).toContain('\n  "name": "inner_task"');
  });
});