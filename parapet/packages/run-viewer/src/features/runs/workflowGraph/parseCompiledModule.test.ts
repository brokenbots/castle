import { describe, expect, test } from 'vitest';
import { parseCompiledModuleBody } from './parseCompiledModule';

// The emitter's contract (CRI-294): each workflow.graphs layer body is the
// compiled module JSON — the `criteria compile --format json` shape of
// subworkflows[].body, shipped as a compact JSON string.
const INNER_BODY = JSON.stringify({
  name: 'inner_task',
  initial_state: 'execute',
  target_state: 'complete',
  policy: { MaxTotalSteps: 100, MaxStepRetries: 0, MaxToolDepth: 8, MaxVisitsWarnThreshold: 200 },
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
  outputs: [{ name: 'result', type: 'string' }],
  switches: [],
  step_order: ['execute'],
  plugins_required: ['shell'],
  metadata: { schema_version: 1 },
});

describe('parseCompiledModuleBody', () => {
  test('parses the compiled module JSON into the layer graph', () => {
    const graph = parseCompiledModuleBody(INNER_BODY);
    expect(graph).not.toBeNull();
    expect(graph!.name).toBe('inner_task');
    expect(graph!.startAt).toBe('execute');
    expect(graph!.nodes.map((n) => n.id)).toEqual(['execute', 'complete']);
    expect(graph!.nodes.map((n) => n.kind)).toEqual(['step', 'state']);
    const state = graph!.nodes.find((n) => n.id === 'complete');
    expect(state?.terminal).toBe(true);
    expect(state?.success).toBe(true);
    expect(graph!.edges).toEqual([
      { from: 'execute', via: 'failure', to: 'complete' },
      { from: 'execute', via: 'success', to: 'complete' },
    ]);
  });

  test('marks steps that run a subworkflow layer', () => {
    const graph = parseCompiledModuleBody(
      JSON.stringify({
        name: 'outer',
        initial_state: 'begin',
        steps: [
          { name: 'begin', subworkflow: 'call', outcomes: [{ name: 'success', next: 'done' }] },
        ],
        states: [{ name: 'done', terminal: true, success: true }],
      }),
    );
    expect(graph?.nodes.find((n) => n.id === 'begin')?.subworkflow).toBe('call');
  });

  test('renders switches with declaration-ordered arms and a default arm', () => {
    const graph = parseCompiledModuleBody(
      JSON.stringify({
        name: 'branchy',
        initial_state: 'decide',
        steps: [],
        switches: [
          {
            name: 'decide',
            conditions: [
              { match: 'var.env == "prod"', next: 'deploy' },
              { match: 'var.env == "staging"', next: 'stage' },
            ],
            default_next: 'skip',
          },
        ],
        states: [
          { name: 'deploy', terminal: true, success: true },
          { name: 'stage', terminal: true, success: true },
          { name: 'skip', terminal: true, success: false },
        ],
      }),
    );
    const sw = graph?.nodes.find((n) => n.id === 'decide');
    expect(sw?.kind).toBe('switch');
    expect(sw?.arms).toEqual([
      { condition: 'var.env == "prod"', target: 'deploy' },
      { condition: 'var.env == "staging"', target: 'stage' },
      { condition: '', target: 'skip' },
    ]);
    expect(graph?.edges).toEqual([
      { from: 'decide', via: 'arm[0]', to: 'deploy' },
      { from: 'decide', via: 'arm[1]', to: 'stage' },
      { from: 'decide', via: 'default', to: 'skip' },
    ]);
  });

  test('targets the compiled output never declares render as placeholder targets', () => {
    // Waits and approvals are absent from the compiled module JSON, so an
    // edge into one cannot attach to a real node.
    const graph = parseCompiledModuleBody(
      JSON.stringify({
        name: 'waiter',
        initial_state: 'pause',
        steps: [{ name: 'pause', outcomes: [{ name: 'resumed', next: 'go' }] }],
        states: [],
      }),
    );
    expect(graph?.nodes.find((n) => n.id === 'go')?.kind).toBe('target');
  });

  test('strips traversal qualifiers from next targets for parity with the HCL parser', () => {
    const graph = parseCompiledModuleBody(
      JSON.stringify({
        name: 'qualified',
        initial_state: 'a',
        steps: [{ name: 'a', outcomes: [{ name: 'success', next: 'state.done' }] }],
        states: [{ name: 'done', terminal: true, success: true }],
      }),
    );
    expect(graph?.edges[0].to).toBe('done');
    expect(graph?.startAt).toBe('a');
  });

  test('deduplicates identical edges', () => {
    const graph = parseCompiledModuleBody(
      JSON.stringify({
        name: 'dup',
        initial_state: 'a',
        steps: [{ name: 'a', outcomes: [{ name: 'success', next: 'done' }] }],
        states: [{ name: 'done', terminal: true, success: true }],
      }),
    );
    expect(graph?.edges).toHaveLength(1);
  });

  test('returns null for non-compiled bodies', () => {
    // HCL module source (the pre-CRI-294 producer shape) is not JSON.
    expect(parseCompiledModuleBody('workflow {\n  name = "x"\n}\nstep "a" {}')).toBeNull();
    expect(parseCompiledModuleBody('{not json')).toBeNull();
    expect(parseCompiledModuleBody('')).toBeNull();
    // JSON, but not an object / not the module shape.
    expect(parseCompiledModuleBody('[1, 2]')).toBeNull();
    expect(parseCompiledModuleBody('"inner_task"')).toBeNull();
    expect(parseCompiledModuleBody('42')).toBeNull();
    expect(parseCompiledModuleBody('{"foo": 1}')).toBeNull();
    expect(parseCompiledModuleBody('{"name": "x"}')).toBeNull();
    expect(parseCompiledModuleBody('{"name": "x", "steps": "nope"}')).toBeNull();
    expect(parseCompiledModuleBody('null')).toBeNull();
  });

  test('tolerates modules without states or switches arrays', () => {
    const graph = parseCompiledModuleBody(
      JSON.stringify({ name: 'minimal', initial_state: 'only', steps: [{ name: 'only' }] }),
    );
    expect(graph?.nodes.map((n) => n.id)).toEqual(['only']);
    expect(graph?.edges).toEqual([]);
    expect(graph?.startAt).toBe('only');
  });
});