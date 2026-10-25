import { http, HttpResponse } from 'msw';

// MSW handlers for Connect-web JSON transport. Each RPC is a
// `POST /<package>.<Service>/<Method>` returning a protojson response.
// Protojson's canonical wire form uses snake_case field names; connect-web
// accepts either, but we stick to snake_case for consistency with the proto
// source of truth. Streaming RPCs (e.g. WatchRun) are left unhandled here —
// tests that rely on live tail mock the client module directly.

export function serverPath(method: string): string {
  return `/criteria.v1.ServerService/${method}`;
}

export function consolePath(method: string): string {
  return `/castle.v1.ConsoleService/${method}`;
}

export function projectPath(method: string): string {
  return `/castle.v1.ProjectService/${method}`;
}

// Mutable mirror of castle's project rows for settings-console tests. The
// etag mirrors castle's content-addressed scheme loosely: any stable string
// distinct across doc states works, since tests drive swaps through the
// responses these handlers return.
const projectRows: Record<
  string,
  {
    id: string;
    membership_policy: 'PROJECT_MEMBERSHIP_POLICY_OPEN' | 'PROJECT_MEMBERSHIP_POLICY_CLOSED';
    default_refire_policy: string;
    queues: string[];
    settings: Record<string, string>;
    info: Record<string, string>;
    created_at: string;
    updated_at: string;
    etag: string;
    settingsSeq: number;
  }
> = {};

const memberRows: Record<string, { project_id: string; member_id: string; kind: 'PROJECT_MEMBER_KIND_AI' | 'PROJECT_MEMBER_KIND_HUMAN'; created_at: string }[]> = {};

const refireRows: Record<string, { max_attempts: number; backoff_seconds: number; transient: string[]; parks: string[]; cosmetics: string[] }> = {};

function seedProject(id: string, policy: 'OPEN' | 'CLOSED', members: string[]) {
  projectRows[id] = {
    id,
    membership_policy:
      policy === 'OPEN' ? 'PROJECT_MEMBERSHIP_POLICY_OPEN' : 'PROJECT_MEMBERSHIP_POLICY_CLOSED',
    default_refire_policy: 'refire-default',
    queues: [`${id}-queue`],
    settings: { allow_anonymous_reads: 'false' },
    info: { purpose: 'seeded' },
    created_at: new Date(0).toISOString(),
    updated_at: new Date(0).toISOString(),
    etag: `etag-${id}-1`,
    settingsSeq: 1,
  };
  memberRows[id] = members.map((memberId) => ({
    project_id: id,
    member_id: memberId,
    kind: memberId.startsWith('human-') ? 'PROJECT_MEMBER_KIND_HUMAN' : 'PROJECT_MEMBER_KIND_AI',
    created_at: new Date(0).toISOString(),
  }));
}

seedProject('alpha', 'OPEN', ['ov-1']);
seedProject('beta', 'CLOSED', ['ov-2', 'human-ops']);

const projectHandlers = [
  http.post(projectPath('ListProjects'), () =>
    HttpResponse.json({ projects: Object.values(projectRows).map(({ settingsSeq, etag, ...p }) => p) }),
  ),
  http.post(projectPath('GetProject'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { project_id?: string };
    const row = body.project_id ? projectRows[body.project_id] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    const { settingsSeq, etag, ...p } = row;
    return HttpResponse.json(p);
  }),
  http.post(projectPath('CreateProject'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      project?: { id?: string; membership_policy?: string };
    };
    const id = body.project?.id ?? '';
    if (!/^[a-z0-9][a-z0-9-_.]{0,127}$/.test(id)) {
      return HttpResponse.json(
        { code: 'invalid_argument', message: 'project id must be a lowercase slug' },
        { status: 400 },
      );
    }
    if (projectRows[id]) {
      return HttpResponse.json(
        { code: 'already_exists', message: 'project already exists' },
        { status: 409 },
      );
    }
    projectRows[id] = {
      id,
      membership_policy:
        body.project?.membership_policy === 'PROJECT_MEMBERSHIP_POLICY_CLOSED'
          ? 'PROJECT_MEMBERSHIP_POLICY_CLOSED'
          : 'PROJECT_MEMBERSHIP_POLICY_OPEN',
      default_refire_policy: 'refire-default',
      queues: [],
      settings: {},
      info: {},
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      etag: `etag-${id}-1`,
      settingsSeq: 1,
    };
    memberRows[id] = [];
    return HttpResponse.json({ project: { id, membership_policy: projectRows[id].membership_policy } });
  }),
  http.post(projectPath('GetProjectSettings'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { project_id?: string };
    const row = body.project_id ? projectRows[body.project_id] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    return HttpResponse.json({ settings: row.settings, etag: row.etag });
  }),
  http.post(projectPath('SetProjectSettings'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      project_id?: string;
      settings?: Record<string, string>;
      expected_etag?: string;
    };
    const row = body.project_id ? projectRows[body.project_id] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    if ((body.expected_etag ?? '') !== row.etag) {
      return HttpResponse.json(
        { code: 'failed_precondition', message: 'settings were modified concurrently' },
        { status: 412 },
      );
    }
    const anon = body.settings?.allow_anonymous_reads;
    if (anon !== undefined && anon !== '' && anon !== 'true' && anon !== 'false') {
      return HttpResponse.json(
        { code: 'invalid_argument', message: 'allow_anonymous_reads must be a JSON boolean' },
        { status: 400 },
      );
    }
    row.settings = { ...(body.settings ?? {}) };
    row.settingsSeq += 1;
    row.etag = `etag-${row.id}-${row.settingsSeq}`;
    row.updated_at = new Date().toISOString();
    return HttpResponse.json({ etag: row.etag });
  }),
  http.post(projectPath('GetRefirePolicy'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { project_id?: string; route?: string };
    const key = `${body.project_id ?? ''}/${body.route ?? ''}`;
    const row = refireRows[key];
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'refire policy not set' }, { status: 404 });
    }
    return HttpResponse.json({ policy: row, updated_at: new Date().toISOString() });
  }),
  http.post(projectPath('SetRefirePolicy'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      project_id?: string;
      route?: string;
      policy?: { max_attempts?: number; backoff_seconds?: number; transient?: string[]; parks?: string[]; cosmetics?: string[] };
    };
    const key = `${body.project_id ?? ''}/${body.route ?? ''}`;
    const policy = body.policy ?? {};
    if (
      (policy.max_attempts ?? 0) < 0 ||
      (policy.backoff_seconds ?? 0) < 0
    ) {
      return HttpResponse.json(
        { code: 'invalid_argument', message: 'refire budgets must be non-negative' },
        { status: 400 },
      );
    }
    refireRows[key] = {
      max_attempts: policy.max_attempts ?? 0,
      backoff_seconds: policy.backoff_seconds ?? 0,
      transient: policy.transient ?? [],
      parks: policy.parks ?? [],
      cosmetics: policy.cosmetics ?? [],
    };
    return HttpResponse.json({ updated_at: new Date().toISOString() });
  }),
  http.post(projectPath('ListProjectMembers'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { project_id?: string };
    const row = body.project_id ? projectRows[body.project_id] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    return HttpResponse.json({ members: memberRows[body.project_id ?? ''] ?? [] });
  }),
  http.post(projectPath('AddProjectMember'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { project_id?: string; member_id?: string; kind?: string };
    const projectId = body.project_id ?? '';
    const row = projectRows[projectId];
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    // Test hook for the console's denied-write surface (castle denies
    // settings/refire/membership writes to protocol credentials).
    if (body.member_id === 'denied') {
      return HttpResponse.json(
        { code: 'permission_denied', message: 'requires project admin credentials' },
        { status: 403 },
      );
    }
    const members = memberRows[projectId];
    if (members.some((m) => m.member_id === body.member_id)) {
      return HttpResponse.json(
        { code: 'already_exists', message: 'member already admitted' },
        { status: 409 },
      );
    }
    if (!body.member_id) {
      return HttpResponse.json({ code: 'invalid_argument', message: 'member_id is required' }, { status: 400 });
    }
    const member = {
      project_id: projectId,
      member_id: body.member_id,
      kind: body.kind === 'PROJECT_MEMBER_KIND_HUMAN' ? ('PROJECT_MEMBER_KIND_HUMAN' as const) : ('PROJECT_MEMBER_KIND_AI' as const),
      created_at: new Date().toISOString(),
    };
    members.push(member);
    return HttpResponse.json({ member });
  }),
  http.post(projectPath('RemoveProjectMember'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { project_id?: string; member_id?: string };
    const members = memberRows[body.project_id ?? ''] ?? [];
    const idx = members.findIndex((m) => m.member_id === body.member_id);
    if (idx >= 0) members.splice(idx, 1);
    return HttpResponse.json({});
  }),
];

export const handlers = [
  ...projectHandlers,
  // CRI-195: default console login (castle.v1.ConsoleService). Successful
  // credentials return a session token; wrong credentials are unauthenticated
  // — mirroring castle's Login handler for tests that exercise the password
  // login path.
  http.post(consolePath('Login'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { username?: string; password?: string };
    if (body.username === 'operator' && body.password === 'op-password') {
      return HttpResponse.json({
        session_token: 'console-session-token-123456',
        username: body.username,
      });
    }
    return HttpResponse.json(
      { code: 'unauthenticated', message: 'invalid username or password' },
      { status: 401 },
    );
  }),
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
  http.post(serverPath('InspectRun'), () =>
    HttpResponse.json({
      run_id: 'run-1',
      session_id: 'sess-1',
      adapter: 'local',
      current_step: 'build',
      pending_permissions: '0',
      last_activity_at: new Date().toISOString(),
      state_json: '',
    }),
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
  http.post(serverPath('GetAgent'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as { criteriaId?: string; criteria_id?: string };
    const criteriaId = body.criteriaId ?? body.criteria_id ?? 'ov-1';
    return HttpResponse.json({
      criteria_id: criteriaId,
      name: 'local',
      labels: { hostname: 'dev' },
      status: 'online',
      registered_at: new Date().toISOString(),
      last_seen_at: new Date().toISOString(),
    });
  }),
  http.post(serverPath('ResumeRun'), async () => {
    return HttpResponse.json({
      issued_at: new Date().toISOString(),
    });
  }),
  // CRI-196 decision delivery rides castle.v1.ConsoleService.ResolveResume
  // (KB-102): signal/payload contracts are castle-owned, not criteria wire.
  http.post(consolePath('ResolveResume'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as {
      run_id?: string;
      signal?: string;
    };
    if (body.run_id === 'run-stale') {
      return HttpResponse.json(
        { code: 'failed_precondition', message: 'signal already satisfied' },
        { status: 412 },
      );
    }
    return HttpResponse.json({
      issued_at: new Date().toISOString(),
      run_id: body.run_id,
      signal: body.signal ?? '',
    });
  }),
  http.post(serverPath('PauseRun'), async () => {
    return HttpResponse.json({
      issued_at: new Date().toISOString(),
    });
  }),
  http.post(serverPath('StopRun'), async () => {
    return HttpResponse.json({
      issued_at: new Date().toISOString(),
    });
  }),
];
