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

export function identityPath(method: string): string {
  return `/castle.v1.IdentityService/${method}`;
}

// Connect-web writes protojson with lowerCamelCase JSON names while these
// mocks speak the proto field names (snake_case); read a parsed body field
// under either spelling. User-owned map keys (Project.settings) are passed
// through untouched — only message field names get an alias.
function readField<T>(record: Record<string, unknown>, protoName: string): T | undefined {
  const jsonName = protoName.replace(/_([a-z0-9])/g, (_match, c: string) => c.toUpperCase());
  return (record[protoName] ?? record[jsonName]) as T | undefined;
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

// KB-340 console fixtures: one workflow config record per project (the
// canonical JSON the proto bytes carry) and the run rows the Runs tab pages
// through. Unscoped legacy runs carry a null project id — the admin
// include_unscoped arm observes them.
const configRows: Record<string, { json: string; etag: string; seq: number }> = {};
const runRows: { run: Record<string, unknown>; project_id: string | null }[] = [];

/** Test fixture: install a project's workflow config record JSON, returning its etag. */
export function seedProjectConfig(projectId: string, json: string): string {
  const seq = (configRows[projectId]?.seq ?? 0) + 1;
  const etag = `cfg-etag-${projectId}-${seq}`;
  configRows[projectId] = { json, etag, seq };
  return etag;
}

/** Test fixture: append run rows (newest-first); null project_id = legacy unscoped. */
export function seedProjectRuns(
  projectId: string | null,
  runs: Record<string, unknown>[],
): void {
  for (const run of runs) runRows.push({ run, project_id: projectId });
}

/** Test fixture: drop the accumulated run rows. */
export function resetProjectRuns(): void {
  runRows.length = 0;
}

function decodeBytesDoc(document: unknown): string {
  // Protojson carries bytes fields as base64 strings on the wire.
  if (typeof document !== 'string' || document === '') return '';
  return decodeURIComponent(escape(atob(document)));
}

function encodeBytesDoc(json: string): string {
  if (!json) return '';
  return btoa(unescape(encodeURIComponent(json)));
}

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

// The mock caller's identity (KB-333): an admin console operator with both
// seeded projects as membership — the same shape castle answers for the
// console-default operator the login mock admits. Scoped-identity tests
// override this handler with server.use; keep it consistent with login.
const identityHandlers = [
  http.post(identityPath('GetCallerIdentity'), () =>
    HttpResponse.json({
      operator_id: 'console-default',
      name: 'operator',
      display_name: 'operator',
      role: 'admin',
      attributes: { role: 'admin', display: 'operator' },
      membership_scoped: false,
      membership: ['alpha', 'beta'],
    }),
  ),
];

const projectHandlers = [
  http.post(projectPath('ListProjects'), () =>
    HttpResponse.json({ projects: Object.values(projectRows).map(({ settingsSeq, etag, ...p }) => p) }),
  ),
  http.post(projectPath('GetProject'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = projectId ? projectRows[projectId] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    const { settingsSeq, etag, ...p } = row;
    return HttpResponse.json({ project: p });
  }),
  http.post(projectPath('CreateProject'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const project = (readField<Record<string, unknown>>(body, 'project') ?? {}) as Record<string, unknown>;
    const id = (readField<string>(project, 'id') ?? '') as string;
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
        readField<string>(project, 'membership_policy') === 'PROJECT_MEMBERSHIP_POLICY_CLOSED'
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
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = projectId ? projectRows[projectId] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    return HttpResponse.json({ settings: row.settings, etag: row.etag });
  }),
  http.post(projectPath('SetProjectSettings'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = projectId ? projectRows[projectId] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    const expectedEtag = readField<string>(body, 'expected_etag');
    if ((expectedEtag ?? '') !== row.etag) {
      return HttpResponse.json(
        { code: 'failed_precondition', message: 'settings were modified concurrently' },
        { status: 412 },
      );
    }
    const settings = (readField<Record<string, string>>(body, 'settings') ?? {}) as Record<string, string>;
    const anon = settings.allow_anonymous_reads;
    if (anon !== undefined && anon !== '' && anon !== 'true' && anon !== 'false') {
      return HttpResponse.json(
        { code: 'invalid_argument', message: 'allow_anonymous_reads must be a JSON boolean' },
        { status: 400 },
      );
    }
    row.settings = { ...settings };
    row.settingsSeq += 1;
    row.etag = `etag-${row.id}-${row.settingsSeq}`;
    row.updated_at = new Date().toISOString();
    return HttpResponse.json({ etag: row.etag });
  }),
  http.post(projectPath('GetRefirePolicy'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const key = `${readField<string>(body, 'project_id') ?? ''}/${readField<string>(body, 'route') ?? ''}`;
    const row = refireRows[key];
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'refire policy not set' }, { status: 404 });
    }
    return HttpResponse.json({ policy: row, updated_at: new Date().toISOString() });
  }),
  http.post(projectPath('SetRefirePolicy'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const key = `${readField<string>(body, 'project_id') ?? ''}/${readField<string>(body, 'route') ?? ''}`;
    const policy = (readField<Record<string, unknown>>(body, 'policy') ?? {}) as Record<string, unknown>;
    const maxAttempts = readField<number>(policy, 'max_attempts') ?? 0;
    const backoffSeconds = readField<number>(policy, 'backoff_seconds') ?? 0;
    if (maxAttempts < 0 || backoffSeconds < 0) {
      return HttpResponse.json(
        { code: 'invalid_argument', message: 'refire budgets must be non-negative' },
        { status: 400 },
      );
    }
    refireRows[key] = {
      max_attempts: maxAttempts,
      backoff_seconds: backoffSeconds,
      transient: (readField<string[]>(policy, 'transient') ?? []) as string[],
      parks: (readField<string[]>(policy, 'parks') ?? []) as string[],
      cosmetics: (readField<string[]>(policy, 'cosmetics') ?? []) as string[],
    };
    return HttpResponse.json({ updated_at: new Date().toISOString() });
  }),
  http.post(projectPath('ListProjectMembers'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = projectId ? projectRows[projectId] : undefined;
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    return HttpResponse.json({ members: memberRows[projectId] ?? [] });
  }),
  http.post(projectPath('AddProjectMember'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = projectRows[projectId];
    if (!row) {
      return HttpResponse.json({ code: 'not_found', message: 'project not found' }, { status: 404 });
    }
    const memberId = readField<string>(body, 'member_id') ?? '';
    // Test hook for the console's denied-write surface (castle denies
    // settings/refire/membership writes to protocol credentials).
    if (memberId === 'denied') {
      return HttpResponse.json(
        { code: 'permission_denied', message: 'requires project admin credentials' },
        { status: 403 },
      );
    }
    const members = memberRows[projectId];
    if (members.some((m) => m.member_id === memberId)) {
      return HttpResponse.json(
        { code: 'already_exists', message: 'member already admitted' },
        { status: 409 },
      );
    }
    if (!memberId) {
      return HttpResponse.json({ code: 'invalid_argument', message: 'member_id is required' }, { status: 400 });
    }
    const member = {
      project_id: projectId,
      member_id: memberId,
      kind: readField<string>(body, 'kind') === 'PROJECT_MEMBER_KIND_HUMAN' ? ('PROJECT_MEMBER_KIND_HUMAN' as const) : ('PROJECT_MEMBER_KIND_AI' as const),
      created_at: new Date().toISOString(),
    };
    members.push(member);
    return HttpResponse.json({ member });
  }),
  http.post(projectPath('RemoveProjectMember'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const memberId = readField<string>(body, 'member_id') ?? '';
    const members = memberRows[projectId] ?? [];
    const idx = members.findIndex((m) => m.member_id === memberId);
    if (idx >= 0) members.splice(idx, 1);
    return HttpResponse.json({});
  }),
  // KB-339 config home + KB-340 console: the project's ONE workflow config
  // record rides Get/SetProjectConfig with the same etag-swap discipline as
  // settings, and the catalog listing derives from the stored record.
  http.post(projectPath('GetProjectConfig'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = configRows[projectId];
    return HttpResponse.json({
      document: encodeBytesDoc(row?.json ?? ''),
      etag: row?.etag ?? '',
    });
  }),
  http.post(projectPath('SetProjectConfig'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = configRows[projectId];
    const expectedEtag = readField<string>(body, 'expected_etag') ?? '';
    if ((row?.etag ?? '') !== expectedEtag) {
      return HttpResponse.json(
        { code: 'failed_precondition', message: 'config record was modified concurrently' },
        { status: 412 },
      );
    }
    let json: string;
    try {
      json = decodeBytesDoc(readField<string>(body, 'document'));
      JSON.parse(json) as unknown;
    } catch {
      return HttpResponse.json(
        { code: 'invalid_argument', message: 'record is not valid JSON' },
        { status: 400 },
      );
    }
    const parsed = JSON.parse(json) as Record<string, unknown>;
    if (parsed.apiVersion !== 'criteria.brokenbots.dev/v1' || parsed.kind !== 'ProjectWorkflowConfig' || typeof parsed.registryName !== 'string') {
      return HttpResponse.json(
        { code: 'invalid_argument', message: 'record is not a ProjectWorkflowConfig record' },
        { status: 400 },
      );
    }
    const etag = seedProjectConfig(projectId, json);
    return HttpResponse.json({ etag });
  }),
  http.post(projectPath('ListWorkflowCatalog'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const row = configRows[projectId];
    if (!row) return HttpResponse.json({ entries: [], etag: '' });
    const entries: Record<string, unknown>[] = [];
    try {
      const rec = JSON.parse(row.json) as {
        namespace?: string;
        digests?: Record<string, string>;
        catalog?: Record<string, Record<string, unknown>>;
      };
      for (const [name, wf] of Object.entries(rec.catalog ?? {})) {
        entries.push({
          name,
          type: wf['type'],
          url: (wf['url'] as string) ?? '',
          ref: (wf['ref'] as string) ?? '',
          digest: rec.digests?.[name] ?? '',
          namespace: rec.namespace ?? '',
          class: (wf['class'] as string) ?? '',
          image: (wf['image'] as string) ?? '',
          interface_name: ((wf['interface'] as Record<string, unknown>)?.['workflow'] as string) ?? '',
        });
      }
    } catch {
      // A deliberately corrupt fixture answers an empty catalog; the etag
      // still reflects the stored row so previews stay consistent.
    }
    return HttpResponse.json({ entries, etag: row.etag });
  }),
  http.post(projectPath('ListProjectRuns'), async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const projectId = readField<string>(body, 'project_id') ?? '';
    const status = readField<string>(body, 'status') ?? '';
    const limit = readField<number>(body, 'limit') ?? 0;
    const pageToken = readField<string>(body, 'page_token') ?? '';
    const includeUnscoped = readField<boolean>(body, 'include_unscoped') ?? false;
    const scope = runRows.filter(
      (r) => r.project_id === projectId || (includeUnscoped && r.project_id === null),
    );
    const filtered = status ? scope.filter((r) => r.run['status'] === status) : scope;
    const offset = /^[0-9]+$/.test(pageToken) ? Number(pageToken) : 0;
    const page = limit > 0 ? filtered.slice(offset, offset + limit) : filtered;
    const next = limit > 0 && offset + limit < filtered.length ? String(offset + limit) : '';
    return HttpResponse.json({ runs: page.map((r) => r.run), next_page_token: next });
  }),
];

export const handlers = [
  ...identityHandlers,
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
