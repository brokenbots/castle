import { describe, expect, test } from 'vitest';
import credentialIsolationSource from './fixtures/credential_isolation.chcl?raw';
import linearIntakeSource from './fixtures/linear_intake_v1.chcl?raw';
import tourSource from './fixtures/tour.chcl?raw';
import { parseHclDocument } from './hcl';
import { parseWorkflowHcl, WorkflowParseError, extractTextEdges } from './parseWorkflowHcl';

// Fixtures are real .chcl workflows: linear_intake_v1 is a faithful
// reduction of brokenbots/workflow-example linear_intake_v1/main.chcl
// (identical node/edge graph), tour is verbatim from the criteria repo's
// examples and covers step-level iteration, wait and switch, and
// credential_isolation is verbatim from the linear_intake_v1 test suite and
// covers `<<-EOT` heredoc command bodies.
describe('parseWorkflowHcl', () => {
  test('parses the real linear_intake_v1 workflow into a step graph', () => {
    const graph = parseWorkflowHcl(linearIntakeSource);

    // Name and start node come from the unlabelled workflow block's
    // attributes.
    expect(graph.name).toBe('linear_intake_v1');
    expect(graph.startAt).toBe('fetch_ticket');

    // 23 steps + 5 switches + 5 states; nothing becomes a placeholder
    // because every transition target is declared.
    expect(graph.nodes).toHaveLength(33);
    expect(graph.edges).toHaveLength(65);

    const kinds = Object.fromEntries(graph.nodes.map((n) => [n.id, n.kind]));
    expect(kinds).toEqual(
      expect.objectContaining({
        fetch_ticket: 'step',
        classify_ticket: 'step',
        run_qa_triage: 'step',
        review_qa_output: 'step',
        route_after_state_check: 'switch',
        route_after_plan_check: 'switch',
        route_internal_label: 'switch',
        route_qa_result: 'switch',
        route_handler: 'switch',
        handler_complete: 'state',
        awaiting_human: 'state',
        ticket_not_found: 'state',
        failed: 'state',
      }),
    );
    // Non-graph declarations never become nodes.
    for (const ignored of ['verdict', 'workstream_file', 'qa_triage', 'workflow']) {
      expect(kinds[ignored]).toBeUndefined();
    }

    // Terminal states carry their success flags.
    expect(graph.nodes.find((n) => n.id === 'handler_complete')).toMatchObject({ terminal: true, success: true });
    expect(graph.nodes.find((n) => n.id === 'awaiting_human')).toMatchObject({ terminal: true, success: true });
    expect(graph.nodes.find((n) => n.id === 'ticket_not_found')).toMatchObject({ terminal: true, success: false });
    expect(graph.nodes.find((n) => n.id === 'failed')).toMatchObject({ terminal: true, success: false });

    // No undeclared targets leak in as placeholders.
    expect(graph.nodes.filter((n) => n.kind === 'target')).toEqual([]);
  });

  test('maps outcome blocks and their `next` traversals to edges', () => {
    const graph = parseWorkflowHcl(linearIntakeSource);

    const edges = graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`);
    expect(edges).toContain('fetch_ticket -success-> check_ticket_state');
    expect(edges).toContain('fetch_ticket -failure-> ticket_not_found');
    expect(edges).toContain('check_ticket_state -success-> route_after_state_check');
    expect(edges).toContain('classify_ticket -bug_written-> set_bug_label');
    expect(edges).toContain('classify_ticket -needs_human-> comment_unusable_ticket');
    expect(edges).toContain('classify_ticket -default-> failed');
    expect(edges).toContain('run_qa_triage -success-> route_qa_result');
    expect(edges).toContain('run_qa_triage -failure-> route_qa_result');
    expect(edges).toContain('set_done_state -success-> handler_complete');
    expect(edges).toContain('set_review_state -success-> awaiting_human');

    // Traversal qualifiers are stripped: ids are the bare names the event
    // stream carries, never "step.x" / "state.y".
    for (const edge of graph.edges) {
      expect(edge.to).not.toMatch(/^(step|state|wait|switch|approval|subworkflow)\./);
    }
  });

  test('parses every switch block into match/default arms with targets', () => {
    const graph = parseWorkflowHcl(linearIntakeSource);

    expect(graph.nodes.find((n) => n.id === 'route_after_state_check')?.arms).toEqual([
      { condition: 'data.internal.ticket_terminal.value', target: 'already_complete' },
      { condition: '', target: 'check_approved_plan' },
    ]);
    expect(graph.nodes.find((n) => n.id === 'route_qa_result')?.arms).toEqual([
      { condition: 'data.internal.verdict.value == ""', target: 'comment_triage_failed' },
      {
        condition: 'startswith ( data.internal.verdict.value , "needs_human_" ) || data.internal.verdict.value == "needs_design_decision"',
        target: 'comment_needs_human',
      },
      { condition: '', target: 'review_qa_output' },
    ]);
    expect(graph.nodes.find((n) => n.id === 'route_handler')?.arms).toEqual([
      { condition: 'data.internal.workstream_file.value != ""', target: 'comment_handler_started' },
      { condition: '', target: 'comment_needs_human' },
    ]);

    const edges = graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`);
    // Arm edges are labelled by declaration order ("arm[<index>]" / "default").
    expect(edges).toContain('route_after_state_check -arm[0]-> already_complete');
    expect(edges).toContain('route_after_state_check -default-> check_approved_plan');
    expect(edges).toContain('route_qa_result -arm[0]-> comment_triage_failed');
    expect(edges).toContain('route_qa_result -arm[1]-> comment_needs_human');
    expect(edges).toContain('route_qa_result -default-> review_qa_output');
    expect(edges).toContain('route_handler -arm[0]-> comment_handler_started');
    expect(edges).toContain('route_handler -default-> comment_needs_human');
  });

  test('marks step-level iteration instead of synthesising separate nodes', () => {
    const graph = parseWorkflowHcl(tourSource);

    expect(graph.name).toBe('tour');
    expect(graph.startAt).toBe('boot');
    expect(graph.nodes).toHaveLength(8);
    expect(graph.edges).toHaveLength(11);

    // Iterating steps stay single nodes; the control and items expression
    // ride along for display.
    expect(graph.nodes.find((n) => n.id === 'process_each')).toMatchObject({
      kind: 'step',
      iteration: { control: 'for_each', items: '["alpha", "beta", "gamma"]' },
    });
    expect(graph.nodes.find((n) => n.id === 'fan_out')).toMatchObject({
      kind: 'step',
      iteration: { control: 'parallel', items: '["auth", "catalog", "billing"]' },
    });
    // No synthetic for_each/do child or loop-back nodes exist: the iterating
    // step is itself the node.
    expect(graph.nodes.some((n) => n.id === 'deploy_one' || n.id === '_continue')).toBe(false);

    const edges = graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`);
    expect(edges).toContain('boot -success-> process_each');
    expect(edges).toContain('process_each -all_succeeded-> fan_out');
    expect(edges).toContain('process_each -any_failed-> aborted');
    expect(edges).toContain('fan_out -all_succeeded-> settle');
    expect(edges).toContain('finish -success-> done');
  });

  test('parses wait, approval, count and while shapes', () => {
    const source = [
      'workflow {',
      '  name = "misc"',
      '  initial_state = "tick"',
      '}',
      'step "tick" {',
      '  target = adapter.noop.default',
      '  count  = 3',
      '  outcome "all_succeeded" { next = wait.gate }',
      '  outcome "any_failed"    { next = state.aborted }',
      '}',
      'wait "gate" {',
      '  duration = "5m"',
      '  outcome "elapsed" { next = switch.decide }',
      '}',
      'switch "decide" {',
      '  match {',
      '    condition = var.flag',
      '    next      = step.guarded',
      '  }',
      '  default { next = step.approve }',
      '}',
      'step "guarded" {',
      '  target = adapter.noop.default',
      '  while  = var.keep_going',
      '  outcome "all_succeeded" { next = approval.sign }',
      '}',
      'approval "sign" {',
      '  approvers = ["supervisor"]',
      '  reason    = "merge?"',
      '  outcome "approved" { next = state.done }',
      '}',
      'step "approve" {',
      '  target = adapter.noop.default',
      '  outcome "success" { next = state.done }',
      '}',
      'state "aborted" { terminal = true success = false }',
      'state "done" { terminal = true success = true }',
    ].join('\n');

    const graph = parseWorkflowHcl(source);
    const kinds = Object.fromEntries(graph.nodes.map((n) => [n.id, n.kind]));
    expect(kinds).toEqual({
      tick: 'step',
      gate: 'wait',
      decide: 'switch',
      guarded: 'step',
      sign: 'approval',
      approve: 'step',
      aborted: 'state',
      done: 'state',
    });
    expect(graph.nodes.find((n) => n.id === 'tick')?.iteration).toEqual({ control: 'count', items: '3' });
    expect(graph.nodes.find((n) => n.id === 'guarded')?.iteration).toEqual({ control: 'while', items: 'var.keep_going' });

    const edges = graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`);
    expect(edges).toEqual([
      'tick -all_succeeded-> gate',
      'tick -any_failed-> aborted',
      'gate -elapsed-> decide',
      'decide -arm[0]-> guarded',
      'decide -default-> approve',
      'guarded -all_succeeded-> sign',
      'sign -approved-> done',
      'approve -success-> done',
    ]);
  });

  test('renders undeclared transition targets as placeholder nodes', () => {
    // The engine's implicit `_error` terminal is never declared by sources;
    // a run's outcome may still route to it, so it renders as a placeholder.
    const graph = parseWorkflowHcl(
      [
        'workflow { name = "w" initial_state = "a" }',
        'step "a" {',
        '  outcome "success" { next = state._error }',
        '  outcome "escalate" { next = subworkflow.child }',
        '}',
      ].join('\n'),
    );
    expect(graph.nodes.find((n) => n.id === '_error')).toMatchObject({ id: '_error', kind: 'target' });
    // The subworkflow qualifier is stripped like any other traversal.
    expect(graph.nodes.find((n) => n.id === 'child')).toMatchObject({ id: 'child', kind: 'target' });
    expect(graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`)).toEqual([
      'a -success-> _error',
      'a -escalate-> child',
    ]);
  });

  test('dedupes repeated outcome declarations', () => {
    const graph = parseWorkflowHcl(
      [
        'workflow { name = "w" }',
        'step "a" {',
        '  outcome "success" { next = step.b }',
        '  outcome "success" { next = step.b }',
        '}',
        'state "b" {}',
      ].join('\n'),
    );
    expect(graph.edges).toEqual([{ from: 'a', via: 'success', to: 'b' }]);
  });

  test('ignores comment lines and unrelated content declarations', () => {
    const source = [
      '# deployment pipeline',
      'workflow {',
      '  name = "w" // inline comment',
      '  initial_state = "a"',
      '}',
      'variable "env" {',
      '  type = "string"',
      '}',
      'data "internal" "flag" {',
      '  type  = bool',
      '  value = false',
      '}',
      'adapter "shell" "default" {',
      '  config {}',
      '}',
      'output "label" {',
      '  value = var.env',
      '}',
      'step "a" {',
      '  input { prompt = "do it ${var.env}" }',
      '  outcome "success" { next = step.b }',
      '}',
      'step "b" { outcome "success" { next = state.done } }',
      'state "done" { terminal = true }',
    ].join('\n');

    const graph = parseWorkflowHcl(source);
    expect(graph.name).toBe('w');
    expect(graph.startAt).toBe('a');
    expect(graph.nodes.map((n) => n.id)).toEqual(['a', 'b', 'done']);
    expect(graph.edges).toEqual([{ from: 'a', via: 'success', to: 'b' }, { from: 'b', via: 'success', to: 'done' }]);
  });

  test('parses the real credential_isolation heredoc workflow into a step graph', () => {
    // Real .chcl workflows carry `command = <<-EOT … EOT` bodies; the
    // heredoc must parse (not fall back to the text-edge regex) so the run
    // Graphs render for these runs.
    const graph = parseWorkflowHcl(credentialIsolationSource);

    expect(graph.name).toBe('credential_isolation');
    expect(graph.startAt).toBe('workflow_identity');
    expect(graph.nodes).toHaveLength(4);
    expect(graph.edges).toHaveLength(4);

    const kinds = Object.fromEntries(graph.nodes.map((n) => [n.id, n.kind]));
    expect(kinds).toEqual({
      workflow_identity: 'step',
      reviewer_identity: 'step',
      passed: 'state',
      failed: 'state',
    });
    expect(graph.nodes.find((n) => n.id === 'passed')).toMatchObject({ terminal: true, success: true });
    expect(graph.nodes.find((n) => n.id === 'failed')).toMatchObject({ terminal: true, success: false });

    const edges = graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`);
    expect(edges).toEqual([
      'workflow_identity -success-> reviewer_identity',
      'workflow_identity -failure-> failed',
      'reviewer_identity -success-> passed',
      'reviewer_identity -failure-> failed',
    ]);
  });

  test('captures heredoc command bodies verbatim as string values', () => {
    const doc = parseHclDocument(credentialIsolationSource);
    const step = doc.find((b) => b.type === 'step' && b.labels[0] === 'workflow_identity');
    const input = step?.blocks.find((b) => b.type === 'input');

    // `<<-EOT` dedents the body to the common leading whitespace and keeps
    // newlines inside the value; `$${…}` shell escapes survive verbatim.
    const command = input?.attrs.get('command');
    expect(command).toMatchObject({
      kind: 'string',
      value: [
        'test "$GH_TOKEN" = "workflow-sentinel"',
        'test -z "$${WORKFLOW_GITHUB_TOKEN+x}"',
        'test -z "$${REVIEWER_GITHUB_TOKEN+x}"',
        'test -z "$${LINEAR_API_KEY+x}"',
      ].join('\n'),
    });
    expect(command?.kind === 'string' && command.text.startsWith('<<-EOT')).toBe(true);
  });

  test('joins line-continued raw expressions and resolves traversal initial_state', () => {
    const graph = parseWorkflowHcl(
      [
        'workflow {',
        '  name          = "continued"',
        '  initial_state = step.check',
        '}',
        'switch "check" {',
        '  match {',
        '    condition = var.a || \\',
        '                var.b',
        '    next = state.ok',
        '  }',
        '  default { next = state.fail }',
        '}',
        'state "ok" { terminal = true success = true }',
        'state "fail" { terminal = true success = false }',
      ].join('\n'),
    );

    // The backslash continuation keeps the condition a single raw
    // expression instead of ending the value at the newline.
    expect(graph.nodes.find((n) => n.id === 'check')?.arms).toEqual([
      { condition: 'var.a || var.b', target: 'ok' },
      { condition: '', target: 'fail' },
    ]);
    // Traversal-form initial_state resolves like its quoted form.
    expect(graph.startAt).toBe('check');
    expect(graph.edges.map((e) => `${e.from} -${e.via}-> ${e.to}`)).toEqual([
      'check -arm[0]-> ok',
      'check -default-> fail',
    ]);

    // Without the continuation the multiline condition is unparseable and
    // the page must keep its fallback rendering.
    expect(() =>
      parseWorkflowHcl(
        [
          'workflow { name = "broken" }',
          'switch "check" {',
          '  match {',
          '    condition = var.a ||',
          '                var.b',
          '  }',
          '}',
        ].join('\n'),
      ),
    ).toThrow(WorkflowParseError);
  });

  test('throws WorkflowParseError for malformed or non-workflow sources', () => {
    // A stray token in a real-shaped source makes the parser reject it —
    // the run detail page must fall back rather than render a partial graph.
    expect(() =>
      parseWorkflowHcl('workflow { name = "w" }\nstep "a" {\n  outcome "success" { next = step.b oops\n}'),
    ).toThrow(WorkflowParseError);
    expect(() => parseWorkflowHcl('workflow { name = "w" }\nswitch "s" {\n  match {\n    condition = true')).toThrow(
      WorkflowParseError,
    );
    expect(() => parseWorkflowHcl('step "a" { broken')).toThrow(WorkflowParseError);
    expect(() => parseWorkflowHcl('not a workflow at all')).toThrow(WorkflowParseError);
    expect(() => parseWorkflowHcl('workflow { name = "unterminated')).toThrow(WorkflowParseError);
    // Non-HCL run payloads must not produce a graph.
    expect(() => parseWorkflowHcl('plain text run payload')).toThrow(WorkflowParseError);
  });

  test('regex fallback extracts text edges from legacy string-map sources', () => {
    const edges = extractTextEdges(
      'workflow "hello" {\n  step "build" {\n    transitions = {\n      "success" = "test"\n    }\n  }\n}',
    );
    expect(edges).toEqual([{ from: 'build', via: 'success', to: 'test' }]);
  });

  test('regex fallback finds no edges in current-dialect sources', () => {
    // The `next = <traversal>` shape has no `"a" = "b"` string pairs, so the
    // legacy regex legitimately reports nothing; the page renders its
    // "no transitions" message rather than a blank panel.
    expect(extractTextEdges(linearIntakeSource)).toEqual([]);
    expect(extractTextEdges(tourSource)).toEqual([]);
  });

  test('records exact source ranges for declared nodes', () => {
    const graph = parseWorkflowHcl(linearIntakeSource);

    // Every declared node's range slices back to its own block; placeholder
    // targets carry no range because they have no declaration.
    const targets = graph.nodes.filter((n) => n.kind === 'target');
    expect(targets).toEqual([]);
    for (const node of graph.nodes) {
      const { sourceRange } = node;
      if (!sourceRange) continue;
      const block = linearIntakeSource.slice(sourceRange.start, sourceRange.end);
      expect(block.startsWith(`${node.kind} "${node.id}"`)).toBe(true);
      expect(block.endsWith('}')).toBe(true);
      // The range must not bleed into a following declaration.
      expect(block.includes(`\n${node.kind} "`)).toBe(false);
    }
    // Spotted check: the run_qa_triage step targets a subworkflow and its
    // highlighted declaration must contain its target attribute.
    const runQaTriage = graph.nodes.find((n) => n.id === 'run_qa_triage')!;
    const block = linearIntakeSource.slice(runQaTriage.sourceRange!.start, runQaTriage.sourceRange!.end);
    expect(block).toContain('subworkflow.qa_triage');
  });

  test('subworkflow-targeting steps are discoverable from the graph model', () => {
    // Drill-down (CRI-257) keys off steps whose `target = subworkflow.<name>`
    // traversal crosses into a subworkflow layer; the graph must expose the
    // layer name so the UI can match it against the workflow.graphs payload.
    const graph = parseWorkflowHcl(linearIntakeSource);
    const layers = graph.nodes.filter((n) => n.subworkflow !== undefined).map((n) => n.subworkflow);
    expect(layers).toEqual(expect.arrayContaining(['qa_triage', 'handler']));
    // Every subworkflow reference resolves to a `subworkflow "<name>"`
    // declaration in the parent module (the fixture declares qa_triage but
    // leaves handler undeclared, exactly like real sources can).
    expect(linearIntakeSource).toMatch(/subworkflow "qa_triage"/);
    // Adapter/data targets are not subworkflow layers.
    expect(graph.nodes.find((n) => n.id === 'set_confirmed_bug_label')?.subworkflow).toBeUndefined();
  });
});
