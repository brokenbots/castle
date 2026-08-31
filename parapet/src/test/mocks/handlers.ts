import { http, HttpResponse } from 'msw';

// MSW handlers for Connect-web JSON transport. Each RPC is a
// `POST /criteria.v1.ServerService/<Method>` returning a protojson response.
// Protojson's canonical wire form uses snake_case field names; connect-web
// accepts either, but we stick to snake_case for consistency with the proto
// source of truth. Streaming RPCs (e.g. WatchRun) are left unhandled here —
// tests that rely on live tail mock the client module directly.

function serverPath(method: string): string {
  return `/criteria.v1.ServerService/${method}`;
}

export const handlers = [
  http.post(serverPath('ListRuns'), () =>
    HttpResponse.json({
      runs: [
        {
          run_id: 'run-1',
          criteria_id: 'ov-1',
          workflow_name: 'hello',
          workflow_hash: 'workflow "hello" {}',
          status: 'running',
          created_at: new Date().toISOString(),
        },
      ],
      next_page_token: '',
    }),
  ),
  http.post(serverPath('GetRun'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { run_id?: string };
    return HttpResponse.json({
      run_id: body.run_id ?? 'run-1',
      criteria_id: 'ov-1',
      workflow_name: 'hello',
      workflow_hash:
        'workflow "hello" {\n  start_at = "build"\n  step "build" {\n    transitions = {\n      "success" = "test"\n    }\n  }\n  step "test" {\n    transitions = {\n      "success" = "done"\n    }\n  }\n  state "done" { terminal = true }\n}',
      status: 'running',
      created_at: new Date().toISOString(),
    });
  }),
  http.post(serverPath('ListRunEvents'), () =>
    HttpResponse.json({ events: [], last_seq: '0' }),
  ),
  http.post(serverPath('ListAgents'), () =>
    HttpResponse.json({
      agents: [
        {
          criteria_id: 'ov-1',
          name: 'local',
          labels: { hostname: 'dev' },
          status: 'online',
          last_seen_at: new Date().toISOString(),
        },
      ],
      next_page_token: '',
    }),
  ),
  http.post(serverPath('ResumeRun'), async () => {
    return HttpResponse.json({
      issued_at: new Date().toISOString(),
    });
  }),
];
