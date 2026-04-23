import { http, HttpResponse } from 'msw';

export const handlers = [
  http.get('/api/v0/runs', () =>
    HttpResponse.json([
      {
        ID: 'run-1',
        OverseerID: 'ov-1',
        WorkflowName: 'hello',
        Status: 'running',
        CurrentStep: 'build',
        LastSeq: 3,
        CreatedAt: new Date().toISOString(),
        WorkflowHCL: 'workflow "hello" {}',
      },
    ]),
  ),
  http.get('/api/v0/runs/:id', ({ params }) =>
    HttpResponse.json({
      ID: String(params.id),
      OverseerID: 'ov-1',
      WorkflowName: 'hello',
      Status: 'running',
      CurrentStep: 'build',
      LastSeq: 3,
      CreatedAt: new Date().toISOString(),
      WorkflowHCL:
        'workflow "hello" {\n  start_at = "build"\n  step "build" {\n    transitions = {\n      "success" = "test"\n    }\n  }\n  step "test" {\n    transitions = {\n      "success" = "done"\n    }\n  }\n  state "done" { terminal = true }\n}',
    }),
  ),
  http.get('/api/v0/runs/:id/events', () => HttpResponse.json([])),
  http.get('/api/v0/overseers', () =>
    HttpResponse.json([
      {
        id: 'ov-1',
        name: 'local',
        status: 'online',
        last_seen_at: new Date().toISOString(),
      },
    ]),
  ),
];
