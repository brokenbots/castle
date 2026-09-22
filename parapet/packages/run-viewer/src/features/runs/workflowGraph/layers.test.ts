import { describe, expect, test } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import { buildSubworkflowLayers, layerSourceText, selectWorkflowGraphs } from './layers';
import wirePayload from './fixtures/workflow_graphs_fd98126e.json';

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

// A compiled module body helper: the `criteria compile --format json` shape
// the CRI-294 contract pins, with an optional inline `subworkflows` key
// (CRI-296) listing the layers this module's own steps reference.
function moduleBody(name: string, subworkflows?: unknown[]): string {
  const module: Record<string, unknown> = {
    name,
    initial_state: 'begin',
    steps: [{ name: 'begin', outcomes: [{ name: 'success', next: 'done' }] }],
    states: [{ name: 'done', terminal: true, success: true }],
  };
  if (subworkflows) module.subworkflows = subworkflows;
  return JSON.stringify(module);
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

  // CRI-296 regression: the emitter ships the FULL nesting — a layer body's
  // `subworkflows` key inlines the layers that body's own steps reference.
  // Every nested layer must land in the built map, or the affordances
  // inside an opened layer gray out ("Subworkflow <name> graph not
  // available yet"). Mirrors run fd98126e: handler inlines four nested
  // layers, each with its own compiled body.
  test('registers nested layers inlined in a compiled layer body', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        {
          name: 'handler',
          sourcePath: '../linear_develop_v1/handler',
          body: moduleBody('handler', [
            { name: 'branch_manager', source_path: '../branch_manager_v1', body: moduleBody('branch_manager') },
            { name: 'pr_reviewer_loop', source_path: '../pr_reviewer_loop_v1', body: moduleBody('pr_reviewer_loop') },
            { name: 'pair_programming_loop', source_path: '../pair_programming_loop_v1', body: moduleBody('pair_programming_loop') },
            {
              name: 'pair_programming_loop_feedback',
              source_path: '../pair_programming_loop_feedback_v1',
              body: moduleBody('pair_programming_loop_feedback'),
            },
          ]),
        },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual([
      'handler',
      'branch_manager',
      'pr_reviewer_loop',
      'pair_programming_loop',
      'pair_programming_loop_feedback',
    ]);
    // Every layer parses to a graph: each affordance is enabled.
    for (const layer of layers) {
      expect(layer.compiledJson).toBe(true);
      expect(layer.graph?.name).toBe(layer.name);
      expect(layer.graph?.nodes.map((n) => n.id)).toEqual(['begin', 'done']);
      expect(layer.graph?.startAt).toBe('begin');
    }
    // The display-only source path rides snake_case inside a compiled body.
    expect(layers[1].sourcePath).toBe('../branch_manager_v1');
  });

  // The recursion is not hard-coded to two levels: a body may inline bodies
  // that inline bodies, at any depth the wire carries.
  test('registers layers at depth 3+ the same way', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        {
          name: 'handler',
          sourcePath: '../handler',
          body: moduleBody('handler', [
            {
              name: 'mid',
              source_path: './mid',
              body: moduleBody('mid', [
                {
                  name: 'inner',
                  source_path: './inner',
                  body: moduleBody('inner', [
                    { name: 'leaf', sourcePath: './leaf', body: moduleBody('leaf') },
                  ]),
                },
              ]),
            },
          ]),
        },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['handler', 'mid', 'inner', 'leaf']);
    for (const layer of layers) expect(layer.graph).not.toBeNull();
    // The innermost entry rides camelCase inside a hand-built body.
    expect(layers[3].sourcePath).toBe('./leaf');
  });

  // Layer names are unique across the compiled tree in practice; a repeat
  // collapses to its shallowest occurrence so the name-keyed layer map
  // resolves deterministically.
  test('a repeated layer name keeps its shallowest occurrence', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        {
          name: 'handler',
          sourcePath: '../handler',
          body: moduleBody('handler', [
            { name: 'shared', source_path: './shared_nested', body: moduleBody('shared_nested') },
          ]),
        },
        { name: 'shared', sourcePath: './shared_top', body: moduleBody('shared_top') },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['handler', 'shared']);
    const shared = layers[1];
    expect(shared.sourcePath).toBe('./shared_top');
    expect(shared.graph?.name).toBe('shared_top');
  });

  test('a nested body that does not parse keeps the layer with a null graph', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        {
          name: 'handler',
          body: moduleBody('handler', [
            { name: 'broken', source_path: './broken', body: 'workflow { this is not hcl }' },
          ]),
        },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['handler', 'broken']);
    const broken = layers[1];
    expect(broken.compiledJson).toBe(false);
    expect(broken.graph).toBeNull();
    // The source pane can still show the raw body.
    expect(broken.body).toBe('workflow { this is not hcl }');
  });

  test('nested entries without a name or body are skipped', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        {
          name: 'handler',
          body: moduleBody('handler', [
            { body: moduleBody('orphan') },
            { name: 'no-body' },
            'garbage',
            { name: 'ok', body: moduleBody('ok') },
          ]),
        },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['handler', 'ok']);
  });

  // The inline walk is lenient about the carrier body's own shape: any
  // JSON body with a `subworkflows` array registers its layers, even when
  // the carrier itself does not parse to a graph.
  test('a JSON body without the module shape still registers its inlined layers', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        {
          name: 'odd',
          body: JSON.stringify({
            subworkflows: [{ name: 'nested', source_path: './nested', body: moduleBody('nested') }],
          }),
        },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['odd', 'nested']);
    expect(layers[0].graph).toBeNull();
    expect(layers[1].graph?.name).toBe('nested');
  });

  // CRI-297 regression, pinning the EXACT live wire shape (run fd98126e,
  // WorkflowGraphs seq 1): the emitter stringifies only the top-level layer
  // bodies, so the entries inside a body's `subworkflows` key ride as
  // INLINE OBJECTS with snake_case source_path. The CRI-296 walk skipped
  // those entries entirely (the string guard), leaving every depth>=2
  // affordance grayed out with "graph not available yet".
  test('registers nested layers whose bodies ride as inline objects (fd98126e wire shape)', () => {
    // Guard the fixture against drift: it must carry the live shape —
    // top-level string body, nested inline object bodies, snake_case
    // source_path — not the all-strings shape the other tests synthesize.
    const topEntry = wirePayload.subworkflows[0];
    expect(typeof topEntry.body).toBe('string');
    const handlerModule = JSON.parse(topEntry.body) as {
      subworkflows: { name: string; source_path?: unknown; sourcePath?: unknown; body: unknown }[];
    };
    expect(handlerModule.subworkflows.map((e) => typeof e.body)).toEqual([
      'object',
      'object',
      'object',
      'object',
    ]);
    expect(handlerModule.subworkflows.map((e) => e.source_path)).toEqual([
      '../branch_manager_v1',
      '../pr_reviewer_loop_v1',
      '../pair_programming_loop_v1',
      '../pair_programming_loop_feedback_v1',
    ]);
    expect(handlerModule.subworkflows.map((e) => e.sourcePath)).toEqual([undefined, undefined, undefined, undefined]);

    const payload = selectWorkflowGraphs([envelope('workflowGraphs', wirePayload)])!;
    const layers = buildSubworkflowLayers(payload);
    expect(layers.map((l) => l.name)).toEqual([
      'handler',
      'branch_manager',
      'pr_reviewer_loop',
      'pair_programming_loop',
      'pair_programming_loop_feedback',
    ]);
    // Top level keeps the CRI-294 behavior: string body, camelCase
    // sourcePath.
    expect(layers[0].sourcePath).toBe('../linear_develop_v1/handler');
    expect(layers[0].compiledJson).toBe(true);
    expect(layers[0].graph?.name).toBe('handler');
    // Nested layers register, parse, and read the snake_case source path —
    // the four affordances inside the opened handler layer.
    expect(layers.slice(1).map((l) => l.sourcePath)).toEqual([
      '../branch_manager_v1',
      '../pr_reviewer_loop_v1',
      '../pair_programming_loop_v1',
      '../pair_programming_loop_feedback_v1',
    ]);
    for (const layer of layers.slice(1)) {
      expect(layer.compiledJson).toBe(true);
      expect(layer.graph?.name).toBe(layer.name);
      expect(layer.graph).not.toBeNull();
    }
    // The pair_programming_loop graph parses its loop: a pair step whose
    // continue arm routes through the switch, plus the wrap terminal.
    const pair = layers[3];
    expect(pair.graph?.startAt).toBe('pair');
    expect(pair.graph?.nodes.map((n) => n.id)).toEqual(['pair', 'wrap', 'continue_check']);
    expect(pair.graph?.edges.map((e) => e.via)).toEqual(['continue', 'success', 'arm[0]', 'default']);
    // The record keeps the object body serialized; the source pane
    // pretty-prints the original object back.
    expect(JSON.parse(layerSourceText(pair))).toEqual(handlerModule.subworkflows[2].body);
  });

  // The normalization is not tied to one depth: an inline object body may
  // itself inline objects, at any depth the wire carries.
  test('normalizes inline object bodies at every depth', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        {
          name: 'top',
          sourcePath: '../top',
          body: moduleBody('top', [
            {
              name: 'mid',
              source_path: '../mid',
              body: {
                name: 'mid',
                initial_state: 'begin',
                steps: [{ name: 'begin', outcomes: [{ name: 'success', next: 'done' }] }],
                states: [{ name: 'done', terminal: true, success: true }],
                subworkflows: [
                  {
                    name: 'leaf',
                    source_path: '../leaf',
                    body: {
                      name: 'leaf',
                      initial_state: 'begin',
                      steps: [{ name: 'begin', outcomes: [{ name: 'success', next: 'done' }] }],
                      states: [{ name: 'done', terminal: true, success: true }],
                    },
                  },
                ],
              },
            },
          ]),
        },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['top', 'mid', 'leaf']);
    for (const layer of layers) {
      expect(layer.compiledJson).toBe(true);
      expect(layer.graph?.name).toBe(layer.name);
    }
    expect(layers[1].sourcePath).toBe('../mid');
    expect(layers[2].sourcePath).toBe('../leaf');
  });

  // An inline object body is compiled JSON by producer contract: even
  // without the module shape the layer keeps compiledJson (the source pane
  // still pretty-prints it) and only the graph drops.
  test('an inline object body without the module shape keeps compiledJson with a null graph', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [{ name: 'odd', source_path: '../odd', body: { foo: 1, subworkflows: [] } }],
    });
    expect(layers).toHaveLength(1);
    expect(layers[0].compiledJson).toBe(true);
    expect(layers[0].graph).toBeNull();
    expect(JSON.parse(layerSourceText(layers[0]))).toEqual({ foo: 1, subworkflows: [] });
  });

  // Bodies that are neither a string nor an object (numbers, arrays) are
  // still skipped at every level.
  test('bodies that are neither string nor object are skipped', () => {
    const layers = buildSubworkflowLayers({
      subworkflows: [
        { name: 'num', body: 42 },
        { name: 'arr', body: [LAYER_BODY] },
        { name: 'ok', body: LAYER_BODY },
      ],
    });
    expect(layers.map((l) => l.name)).toEqual(['ok']);
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