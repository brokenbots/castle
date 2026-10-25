import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test } from 'vitest';
import {
  settingsApi,
  SETTINGS_API_REDUCER_PATH,
  policyId,
  type MembershipPolicy,
} from './settingsApi';
import { createRunViewerStore } from '@castle/run-viewer';
import { selectAuthExpired, sessionRecovered } from '@castle/run-viewer';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import { projectPath } from '@castle/run-viewer/src/test/mocks/handlers';

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