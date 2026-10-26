import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, test } from 'vitest';
import {
  settingsApi,
  SETTINGS_API_REDUCER_PATH,
  policyId,
  type MembershipPolicy,
} from './settingsApi';
import { createRunViewerStore } from '@castle/run-viewer';
import { selectAuthExpired, sessionRecovered } from '@castle/run-viewer';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import {
  projectPath,
  resetProjectRuns,
  seedProjectConfig,
  seedProjectRuns,
} from '@castle/run-viewer/src/test/mocks/handlers';

// The store wiring under test mirrors production (parapet/src/store.ts):
// the settings console slice rides the run-viewer store seam via the
// extraReducers / extraMiddleware / extraApiPaths options.
const store = createRunViewerStore({
  extraReducers: { [SETTINGS_API_REDUCER_PATH]: settingsApi.reducer },
  extraMiddleware: [settingsApi.middleware],
  extraApiPaths: [SETTINGS_API_REDUCER_PATH],
});

afterEach(() => {
  store.dispatch(settingsApi.util.resetApiState());
  store.dispatch(sessionRecovered());
});

describe('settingsApi store wiring', () => {
  test('hosts the settings slice inside the run-viewer store', () => {
    expect(Object.keys(store.getState())).toContain(SETTINGS_API_REDUCER_PATH);
  });

  test('an unauthenticated settings rejection flips the session gate via extraApiPaths', async () => {
    server.use(
      http.post(projectPath('ListProjects'), () =>
        new HttpResponse(JSON.stringify({ code: 'unauthenticated', message: 'session rejected' }), {
          status: 401,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );

    await store.dispatch(settingsApi.endpoints.listProjects.initiate());

    expect(selectAuthExpired(store.getState())).toBe(true);
  });
});

describe('settingsApi projects', () => {
  test('listProjects maps the wire enum onto open/closed policy strings', async () => {
    const res = await store.dispatch(settingsApi.endpoints.listProjects.initiate());

    expect(res.error).toBeUndefined();
    expect(res.data?.map((p) => `${p.id}:${p.membershipPolicy}`)).toEqual([
      'alpha:open',
      'beta:closed',
    ]);
  });

  test('createProject posts the slug and the requested policy enum, returns the project', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('CreateProject'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        const project = (bodies[0].project ?? {}) as Record<string, unknown>;
        return HttpResponse.json({
          project: { id: project.id, membership_policy: project.membership_policy },
        });
      }),
    );

    const res: { data?: { id: string; membershipPolicy: MembershipPolicy } } = await store.dispatch(
      settingsApi.endpoints.createProject.initiate({ slug: 'gamma', membershipPolicy: 'closed' }),
    );

    expect(bodies).toHaveLength(1);
    expect((bodies[0].project as Record<string, unknown>).id).toBe('gamma');
    expect((bodies[0].project as Record<string, unknown>).membershipPolicy).toBe(
      'PROJECT_MEMBERSHIP_POLICY_CLOSED',
    );
    expect(res.data?.id).toBe('gamma');
    expect(res.data?.membershipPolicy).toBe('closed');
  });
});

describe('settingsApi settings doc (etag swap)', () => {
  test('getProjectSettings surfaces the doc and its etag', async () => {
    const res = await store.dispatch(settingsApi.endpoints.getProjectSettings.initiate('alpha'));

    expect(res.data).toEqual({
      settings: { allow_anonymous_reads: 'false' },
      etag: 'etag-alpha-1',
    });
  });

  test('setProjectSettings carries the read etag and returns the new one', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('SetProjectSettings'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({ etag: 'etag-alpha-2' });
      }),
    );

    const res = await store.dispatch(
      settingsApi.endpoints.setProjectSettings.initiate({
        projectId: 'alpha',
        settings: { allow_anonymous_reads: 'true' },
        expectedEtag: 'etag-alpha-1',
      }),
    );

    expect(bodies[0].projectId).toBe('alpha');
    expect(bodies[0].expectedEtag).toBe('etag-alpha-1');
    expect(bodies[0].settings).toEqual({ allow_anonymous_reads: 'true' });
    expect(res.data).toBe('etag-alpha-2');
  });

  test('a stale etag surfaces failed_precondition for conflict recovery', async () => {
    // The mock rows still carry etag-alpha-1; sending a mismatched
    // expectation exercises castle's failed_precondition swap rejection.
    const res = await store.dispatch(
      settingsApi.endpoints.setProjectSettings.initiate({
        projectId: 'alpha',
        settings: {},
        expectedEtag: 'etag-alpha-STALE',
      }),
    );

    expect(res.data).toBeUndefined();
    expect(res.error).toEqual({
      status: 'failed_precondition',
      data: 'settings were modified concurrently',
    });
  });
});

describe('settingsApi refire policies', () => {
  test('getRefirePolicy surfaces an unset row as not_found', async () => {
    const res = await store.dispatch(
      settingsApi.endpoints.getRefirePolicy.initiate({ projectId: 'alpha', route: '' }),
    );

    expect(res.data).toBeUndefined();
    expect((res.error as { status?: string }).status).toBe('not_found');
  });

  test('setRefirePolicy posts budgets and vocabularies for the exact route key', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('SetRefirePolicy'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({ updated_at: '2026-01-01T00:00:00.000Z' });
      }),
    );

    const res = await store.dispatch(
      settingsApi.endpoints.setRefirePolicy.initiate({
        projectId: 'alpha',
        route: 'deploy/build',
        policy: { maxAttempts: 3, backoffSeconds: 30, transient: ['timeout'], parks: [], cosmetics: [] },
      }),
    );

    expect(bodies[0].projectId).toBe('alpha');
    expect(bodies[0].route).toBe('deploy/build');
    // Protojson omits default values (empty repeated fields, zero ints) —
    // the wire carries only the non-zero budgets and vocabularies.
    expect(bodies[0].policy).toEqual({
      maxAttempts: 3,
      backoffSeconds: 30,
      transient: ['timeout'],
    });
    expect(res.data?.policy.maxAttempts).toBe(3);
  });

  test('a negative budget surfaces invalid_argument', async () => {
    const res = await store.dispatch(
      settingsApi.endpoints.setRefirePolicy.initiate({
        projectId: 'alpha',
        route: '',
        policy: { maxAttempts: -1, backoffSeconds: 0, transient: [], parks: [], cosmetics: [] },
      }),
    );

    expect((res.error as { status?: string }).status).toBe('invalid_argument');
  });
});

describe('settingsApi members', () => {
  test('listProjectMembers maps the member kind enum', async () => {
    const res = await store.dispatch(settingsApi.endpoints.listProjectMembers.initiate('beta'));

    expect(res.data?.map((m) => `${m.memberId}:${m.kind}`)).toEqual(['ov-2:ai', 'human-ops:human']);
  });

  test('addProjectMember posts the requested kind enum', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('AddProjectMember'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({
          member: {
            project_id: bodies[0].projectId,
            member_id: bodies[0].memberId,
            kind: bodies[0].kind,
          },
        });
      }),
    );

    const res = await store.dispatch(
      settingsApi.endpoints.addProjectMember.initiate({
        projectId: 'beta',
        memberId: 'human-2',
        kind: 'human',
      }),
    );

    expect(bodies[0].memberId).toBe('human-2');
    expect(bodies[0].kind).toBe('PROJECT_MEMBER_KIND_HUMAN');
    expect(res.data?.kind).toBe('human');
  });

  test('a denied membership write surfaces permission_denied', async () => {
    const res = await store.dispatch(
      settingsApi.endpoints.addProjectMember.initiate({
        projectId: 'beta',
        memberId: 'denied',
        kind: 'ai',
      }),
    );

    expect((res.error as { status?: string }).status).toBe('permission_denied');
  });

  test('removeProjectMember tolerates unknown members (idempotent no-op)', async () => {
    const res = await store.dispatch(
      settingsApi.endpoints.removeProjectMember.initiate({
        projectId: 'beta',
        memberId: 'never-existed',
      }),
    );

    expect(res.error).toBeUndefined();
  });
});

describe('policyId', () => {
  test('derives per-(project, route) tag ids with the default row at empty route', () => {
    expect(policyId('alpha', '')).toBe('alpha/');
    expect(policyId('alpha', 'deploy/build')).toBe('alpha/deploy/build');
  });
});

// KB-340: the project console's config/routes/catalog/runs surfaces ride
// the same etag-swap store as settings; these tests pin the wire mapping
// (bytes documents, catalog projection, runs paging + the admin
// include_unscoped arm).
describe('settingsApi project config console', () => {
  const configRecord = {
    apiVersion: 'criteria.brokenbots.dev/v1',
    kind: 'ProjectWorkflowConfig',
    registryName: 'alpha',
    namespace: 'alpha-ns',
    catalog: {
      'ship-it': {
        type: 'url',
        url: 'https://git.example/alpha/ship-it',
        ref: 'main',
        interface: { workflow: 'ship-it' },
      },
      'lint-only': { type: 'image', image: 'registry.example/lint@sha256:zz', class: 'triage' },
    },
    routes: [
      {
        name: 'triage-runs',
        workflow: 'lint-only',
        project: 'alpha',
        states: ['Triage'],
      },
    ],
    digests: { 'ship-it': 'a'.repeat(64) },
  };

  test('getProjectConfig reports unset docs as empty json + empty etag', async () => {
    const res = await store.dispatch(settingsApi.endpoints.getProjectConfig.initiate('beta'));
    expect(res.error).toBeUndefined();
    expect(res.data).toEqual({ json: '', etag: '', record: undefined });
  });

  test('getProjectConfig decodes the bytes document into json + the TS record model', async () => {
    seedProjectConfig('alpha', JSON.stringify(configRecord));

    const res = await store.dispatch(settingsApi.endpoints.getProjectConfig.initiate('alpha'));
    expect(res.error).toBeUndefined();
    expect(res.data?.etag).toBe('cfg-etag-alpha-1');
    expect(res.data?.json).toBe(JSON.stringify(configRecord));
    expect(res.data?.record?.registryName).toBe('alpha');
    expect(Object.keys(res.data?.record?.catalog ?? {})).toEqual(['ship-it', 'lint-only']);
    expect(res.data?.record?.routes?.[0]?.workflow).toBe('lint-only');
  });

  test('setProjectConfig posts the document as bytes + expected etag and returns the new etag', async () => {
    const edited = JSON.stringify({ ...configRecord, namespace: 'alpha-ns-v2' });
    const res = await store.dispatch(
      settingsApi.endpoints.setProjectConfig.initiate({
        projectId: 'alpha',
        json: edited,
        expectedEtag: 'cfg-etag-alpha-1',
      }),
    );

    expect(res.error).toBeUndefined();
    expect(res.data).toBe('cfg-etag-alpha-2');

    // The swap invalidates the config: a fresh read sees the edit.
    const after = await store.dispatch(settingsApi.endpoints.getProjectConfig.initiate('alpha'));
    expect(JSON.parse(after.data?.json ?? '{}').namespace).toBe('alpha-ns-v2');
  });

  test('setProjectConfig rejects a stale etag without clobbering', async () => {
    const res = await store.dispatch(
      settingsApi.endpoints.setProjectConfig.initiate({
        projectId: 'alpha',
        json: JSON.stringify(configRecord),
        expectedEtag: 'cfg-etag-alpha-stale',
      }),
    );
    expect((res.error as { status?: string }).status).toBe('failed_precondition');
  });

  test('setProjectConfig surfaces the server fail-closed shape rejection', async () => {
    const res = await store.dispatch(
      settingsApi.endpoints.setProjectConfig.initiate({
        projectId: 'alpha',
        json: JSON.stringify({ apiVersion: 'x', kind: 'y' }),
        expectedEtag: 'cfg-etag-alpha-2',
      }),
    );
    expect((res.error as { status?: string }).status).toBe('invalid_argument');
  });

  test('listWorkflowCatalog projects the record catalog with digests and namespaces', async () => {
    const res = await store.dispatch(settingsApi.endpoints.listWorkflowCatalog.initiate('alpha'));
    expect(res.error).toBeUndefined();
    expect(res.data?.etag).toBe('cfg-etag-alpha-2');
    expect(res.data?.entries).toEqual([
      expect.objectContaining({
        name: 'ship-it',
        type: 'url',
        url: 'https://git.example/alpha/ship-it',
        ref: 'main',
        digest: 'a'.repeat(64),
        namespace: 'alpha-ns-v2',
        interfaceName: 'ship-it',
      }),
      expect.objectContaining({ name: 'lint-only', type: 'image', class: 'triage' }),
    ]);
  });

  test('listWorkflowCatalog on an unset record answers an empty listing', async () => {
    const res = await store.dispatch(settingsApi.endpoints.listWorkflowCatalog.initiate('beta'));
    expect(res.data).toEqual({ entries: [], etag: '' });
  });
});

describe('settingsApi listProjectRuns', () => {
  beforeEach(() => resetProjectRuns());
  const runWire = (runId: string, status: string, scope: string | null) => ({
    run_id: runId,
    criteria_id: scope === null ? 'ov-legacy' : 'ov-1',
    workflow_name: 'ship-it',
    workflow_hash: 'workflow "ship-it" {}',
    status,
    created_at: new Date(0).toISOString(),
    final_state: '',
    failure_reason: '',
  });

  test('pages the project scoped runs and maps the criteria projection', async () => {
    seedProjectRuns('alpha', [
      runWire('alpha-run-1', 'running', 'alpha'),
      runWire('alpha-run-2', 'pending', 'alpha'),
    ]);

    const res = await store.dispatch(
      settingsApi.endpoints.listProjectRuns.initiate({
        projectId: 'alpha',
        status: '',
        limit: 0,
        pageToken: '',
        includeUnscoped: false,
      }),
    );

    expect(res.error).toBeUndefined();
    expect(res.data?.nextPageToken).toBe('');
    expect(res.data?.runs.map((r) => `${r.runId}:${r.status}`)).toEqual(['alpha-run-1:running', 'alpha-run-2:pending']);
  });

  test('status filter and paging flow through the wire request', async () => {
    seedProjectRuns('alpha', [
      runWire('alpha-filter-1', 'running', 'alpha'),
      runWire('alpha-filter-2', 'running', 'alpha'),
      runWire('alpha-filter-3', 'running', 'alpha'),
    ]);

    const page1 = await store.dispatch(
      settingsApi.endpoints.listProjectRuns.initiate({
        projectId: 'alpha',
        status: 'running',
        limit: 2,
        pageToken: '',
        includeUnscoped: false,
      }),
    );

    expect(page1.data?.runs.map((r) => r.runId)).toEqual(['alpha-filter-1', 'alpha-filter-2']);
    expect(page1.data?.runs.every((r) => r.runId.startsWith('alpha-filter-'))).toBe(true);
    expect(page1.data?.nextPageToken).toBe('2');

    const page2 = await store.dispatch(
      settingsApi.endpoints.listProjectRuns.initiate({
        projectId: 'alpha',
        status: 'running',
        limit: 2,
        pageToken: '2',
        includeUnscoped: false,
      }),
    );
    expect(page2.data?.nextPageToken).toBe('');
    expect(page2.data?.runs.map((r) => r.runId)).toEqual(['alpha-filter-3']);
  });

  test('admin callers widen the page with include_unscoped; denials surface as errors', async () => {
    seedProjectRuns(null, [runWire('alpha-unscoped-legacy', 'stopped', null)]);

    const widened = await store.dispatch(
      settingsApi.endpoints.listProjectRuns.initiate({
        projectId: 'alpha',
        status: '',
        limit: 0,
        pageToken: '',
        includeUnscoped: true,
      }),
    );

    expect(widened.data?.runs.map((r) => r.runId)).toContain('alpha-unscoped-legacy');

    server.use(
      http.post(
        projectPath('ListProjectRuns'),
        () =>
          new HttpResponse(JSON.stringify({ code: 'permission_denied', message: 'non-admin' }), {
            status: 403,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    const denied = await store.dispatch(
      settingsApi.endpoints.listProjectRuns.initiate({
        projectId: 'alpha',
        status: '',
        limit: 0,
        pageToken: '',
        includeUnscoped: true,
      }, { forceRefetch: true }),
    );
    expect((denied.error as { status?: string }).status).toBe('permission_denied');
  });
});