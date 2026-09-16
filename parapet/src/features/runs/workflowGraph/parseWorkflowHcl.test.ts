import { describe, expect, test } from 'vitest';
import linearIntakeSource from './fixtures/linear_intake_v1.chcl?raw';
import { parseWorkflowHcl, WorkflowParseError, extractTextEdges } from './parseWorkflowHcl';

const linearIntake = () => linearIntakeSource;

describe('parseWorkflowHcl', () => {
  test('parses the linear_intake_v1 .chcl fixture into a step graph', () => {
    const graph = parseWorkflowHcl(linearIntake());

    expect(graph.name).toBe('linear_intake_v1');
    expect(graph.startAt).toBe('triage');

    const kinds = Object.fromEntries(graph.nodes.map((n) => [n.id, n.kind]));
    expect(kinds).toEqual(
      expect.objectContaining({
        triage: 'step',
        check_env: 'branch',
        deploy_services: 'for_each',
        deploy_one: 'step',
        deploy_web: 'step',
        escalate: 'step',
        rollback: 'step',
        report: 'state',
        failed: 'state',
      }),
    );
    // Only declared nodes exist: no `_continue` placeholder leaks through.
    expect(kinds._continue).toBeUndefined();
    expect(graph.nodes.find((n) => n.id === 'report')).toMatchObject({ terminal: true, success: true });
    expect(graph.nodes.find((n) => n.id === 'failed')).toMatchObject({ terminal: true, success: false });

    const edges = graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`);
    expect(edges).toContain('triage -success-> check_env');
    expect(edges).toContain('triage -failure-> failed');
    expect(edges).toContain('check_env -var.team == "platform"-> deploy_services');
    expect(edges).toContain('check_env -var.team == "web"-> deploy_web');
    expect(edges).toContain('check_env -default-> escalate');
    expect(edges).toContain('deploy_services -do-> deploy_one');
    expect(edges).toContain('deploy_one -success-> deploy_services');
    expect(edges).toContain('deploy_one -failure-> deploy_services');
    expect(edges).toContain('deploy_services -all_succeeded-> report');
    expect(edges).toContain('deploy_services -any_failed-> rollback');
    expect(edges).toContain('deploy_web -success-> report');
    expect(edges).toContain('deploy_web -failure-> rollback');
    expect(edges).toContain('escalate -success-> failed');
    expect(edges).toContain('rollback -success-> failed');
  });

  test('exposes branch arms with their conditions in declaration order', () => {
    const graph = parseWorkflowHcl(linearIntake());
    const branch = graph.nodes.find((n) => n.id === 'check_env');
    expect(branch?.arms).toEqual([
      { condition: 'var.team == "platform"', target: 'deploy_services' },
      { condition: 'var.team == "web"', target: 'deploy_web' },
      { condition: '', target: 'escalate' },
    ]);
  });

  test('captures the for_each items expression and do step', () => {
    const graph = parseWorkflowHcl(linearIntake());
    const loop = graph.nodes.find((n) => n.id === 'deploy_services');
    expect(loop?.forEach).toEqual({
      items: '["api", "web", "worker"]',
      do: 'deploy_one',
    });
  });

  test('parses the transitions-map step shape used by current workflows', () => {
    const source = [
      'workflow "hello" {',
      '  start_at = "build"',
      '  step "build" {',
      '    transitions = {',
      '      "success" = "test"',
      '    }',
      '  }',
      '  step "test" {',
      '    transitions = {',
      '      "success" = "done"',
      '    }',
      '  }',
      '  state "done" { terminal = true }',
      '}',
    ].join('\n');

    const graph = parseWorkflowHcl(source);
    expect(graph.startAt).toBe('build');
    expect(graph.edges).toEqual([
      { from: 'build', via: 'success', to: 'test' },
      { from: 'test', via: 'success', to: 'done' },
    ]);
  });

  test('ignores comment lines and unrelated blocks', () => {
    const source = [
      '# deployment pipeline',
      'workflow "w" {',
      '  start_at = "a" // inline comment',
      '  variable "env" {',
      '    type = "string"',
      '  }',
      '  agent "bot" { adapter = "shell" }',
      '  step "a" {',
      '    input { prompt = "do it ${var.env}" }',
      '    outcome "success" { transition_to = "b" }',
      '  }',
      '  step "b" { outcome "success" { transition_to = "done" } }',
      '  state "done" { terminal = true }',
      '}',
    ].join('\n');

    const graph = parseWorkflowHcl(source);
    expect(graph.startAt).toBe('a');
    expect(graph.nodes.map((n) => n.id)).toEqual(['a', 'b', 'done']);
    expect(graph.edges).toEqual([{ from: 'a', via: 'success', to: 'b' }, { from: 'b', via: 'success', to: 'done' }]);
  });

  test('renders undeclared transition targets as placeholder nodes', () => {
    const graph = parseWorkflowHcl('workflow "w" { step "a" { outcome "success" { transition_to = "nowhere" } } }');
    const placeholder = graph.nodes.find((n) => n.id === 'nowhere');
    expect(placeholder).toMatchObject({ id: 'nowhere', kind: 'target' });
  });

  test('keeps _continue as a placeholder when no for_each owns the step', () => {
    const graph = parseWorkflowHcl('workflow "w" { step "a" { outcome "success" { transition_to = "_continue" } } }');
    expect(graph.edges).toEqual([{ from: 'a', via: 'success', to: '_continue' }]);
    expect(graph.nodes.find((n) => n.id === '_continue')).toMatchObject({ kind: 'target' });
  });

  test('dedupes repeated outcome declarations', () => {
    const graph = parseWorkflowHcl(
      [
        'workflow "w" {',
        '  step "a" {',
        '    outcome "success" { transition_to = "b" }',
        '    outcome "success" { transition_to = "b" }',
        '  }',
        '}',
      ].join('\n'),
    );
    expect(graph.edges).toEqual([{ from: 'a', via: 'success', to: 'b' }]);
  });

  test('throws WorkflowParseError for malformed sources', () => {
    expect(() => parseWorkflowHcl('step "a" { broken')).toThrow(WorkflowParseError);
    expect(() => parseWorkflowHcl('not a workflow at all')).toThrow(WorkflowParseError);
    expect(() => parseWorkflowHcl('workflow "w" { "unterminated')).toThrow(WorkflowParseError);
    // Non-HCL run payloads must not produce a graph.
    expect(() => parseWorkflowHcl('plain text run payload')).toThrow(WorkflowParseError);
  });

  test('regex fallback extracts text edges from simple sources', () => {
    const edges = extractTextEdges(
      'workflow "hello" {\n  step "build" {\n    transitions = {\n      "success" = "test"\n    }\n  }\n}',
    );
    expect(edges).toEqual([{ from: 'build', via: 'success', to: 'test' }]);
  });
});