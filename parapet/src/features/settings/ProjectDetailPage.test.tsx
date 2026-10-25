import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test } from 'vitest';
import { ProjectDetailPage } from './ProjectDetailPage';
import { settingsApi, SETTINGS_API_REDUCER_PATH } from './settingsApi';
import { createRunViewerStore } from '@castle/run-viewer';
import { sessionRecovered } from '@castle/run-viewer';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import { projectPath } from '@castle/run-viewer/src/test/mocks/handlers';

function renderProjectDetail(projectId: string) {
  const store = createRunViewerStore({
    extraReducers: { [SETTINGS_API_REDUCER_PATH]: settingsApi.reducer },
    extraMiddleware: [settingsApi.middleware],
    extraApiPaths: [SETTINGS_API_REDUCER_PATH],
  });
  store.dispatch(sessionRecovered());
  render(
    <Provider store={store}>
      <MemoryRouter
        initialEntries={[`/settings/projects/${projectId}`]}
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <Routes>
          <Route path="/settings/projects/:projectId" element={<ProjectDetailPage />} />
        </Routes>
      </MemoryRouter>
    </Provider>,
  );
}

afterEach(() => {
  cleanup();
});

describe('ProjectDetailPage overview', () => {
  test('renders the project editors for an open project', async () => {
    renderProjectDetail('alpha');

    const detail = await screen.findByTestId('project-detail');
    expect(detail).toBeInTheDocument();
    expect(await screen.findByTestId('project-overview')).toHaveTextContent('open');
    expect(screen.getByTestId('project-settings')).toBeInTheDocument();
    expect(screen.getByTestId('project-refire')).toBeInTheDocument();
    expect(screen.getByTestId('project-members')).toBeInTheDocument();
  });

  test('renders the not-found state for an unknown slug', async () => {
    renderProjectDetail('missing-project');

    expect(await screen.findByTestId('project-detail-error')).toBeInTheDocument();
  });
});

describe('ProjectDetailPage settings editor (etag swap)', () => {
  test('swaps the doc under the last-read etag and surfaces the new etag', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    let saved = false;
    server.use(
      http.post(projectPath('SetProjectSettings'), async ({ request }) => {
      bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
      saved = true;
      return HttpResponse.json({ etag: 'etag-alpha-2' });
      }),
      // The swap is followed by the invalidating refetch: serve the doc the
      // new etag describes from that point on.
      http.post(projectPath('GetProjectSettings'), () =>
      HttpResponse.json(
        saved
          ? { settings: { allow_anonymous_reads: 'true' }, etag: 'etag-alpha-2' }
          : { settings: { allow_anonymous_reads: 'false' }, etag: 'etag-alpha-1' },
      ),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    // The seeded doc renders one interpreted-key entry with its boolean
    // select (the ONE interpreted settings key).
    const valueSelect = await screen.findByTestId('project-setting-value-0');
    expect(valueSelect).toHaveValue('false');
    await user.selectOptions(valueSelect, 'true');
    await user.click(screen.getByTestId('project-settings-save'));

    expect(bodies).toHaveLength(1);
    expect(bodies[0].projectId).toBe('alpha');
    expect(bodies[0].settings).toEqual({ allow_anonymous_reads: 'true' });
    expect(bodies[0].expectedEtag).toBe('etag-alpha-1');
    expect(await screen.findByTestId('project-settings-saved')).toBeInTheDocument();
    // The swap chains: the etag chip now describes the doc the UI shows.
    expect(await screen.findByTestId('project-settings-etag')).toHaveTextContent('etag-alpha-2');
  });

  test('rejects a concurrent swap without clobbering and recovers via reload', async () => {
    server.use(
      http.post(projectPath('SetProjectSettings'), () =>
      new HttpResponse(
        JSON.stringify({ code: 'failed_precondition', message: 'settings were modified concurrently' }),
        { status: 412, headers: { 'content-type': 'application/json' } },
      ),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('project-setting-value-0');
    await user.selectOptions(screen.getByTestId('project-setting-value-0'), 'true');
    await user.click(screen.getByTestId('project-settings-save'));

    const conflict = await screen.findByTestId('project-settings-conflict');
    expect(conflict).toHaveTextContent(/nothing was clobbered/i);

    // Reload retires the local draft (that is what "reload current doc"
    // means: abandon the in-flight edit) and re-reads the current doc/etag.
    await user.click(screen.getByTestId('project-settings-reload'));

    await waitFor(() =>
      expect(screen.queryByTestId('project-settings-conflict')).not.toBeInTheDocument(),
    );
    // The re-read doc back at the seeded etag; the draft was reset to it.
    expect(screen.getByTestId('project-setting-value-0')).toHaveValue('false');
    expect(screen.getByTestId('project-settings-etag')).toHaveTextContent('etag-alpha-1');
  });

  test('blocks saving an invalid JSON value with an inline error', async () => {
    server.use(
      http.post(projectPath('SetProjectSettings'), async () =>
      HttpResponse.json({ etag: 'etag-alpha-x' }),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('project-setting-value-0');
    await user.click(screen.getByTestId('project-setting-add-row'));
    await user.type(screen.getByTestId('project-setting-key-1'), 'feature_flags');
    await user.type(screen.getByTestId('project-setting-value-1'), 'not json');

    expect(await screen.findByTestId('project-setting-error-1')).toHaveTextContent(
      /valid JSON/i,
    );
    expect(screen.getByTestId('project-settings-save')).toBeDisabled();
  });

  test('removing an entry drops it from the swap doc', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('SetProjectSettings'), async ({ request }) => {
      bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
      return HttpResponse.json({ etag: 'etag-alpha-2' });
      }),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('project-setting-value-0');
    await user.click(screen.getByTestId('project-setting-remove-row-0'));
    // The draft is empty now: one blank row remains in the form.
    await user.click(screen.getByTestId('project-settings-save'));

    // An entirely empty map is valid doc content; protojson leaves empty
    // maps off the wire and castle decodes the field as the empty doc.
    expect(bodies[0].projectId).toBe('alpha');
    expect(bodies[0].settings ?? {}).toEqual({});
  });
});

describe('ProjectDetailPage refire editor', () => {
  test('an unset policy row reads as "not set" and save creates it', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('SetRefirePolicy'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({ updated_at: '2026-01-02T00:00:00.000Z' });
      }),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    expect(await screen.findByText(/not set — saving creates it/i)).toBeInTheDocument();
    await screen.findByTestId('refire-budgets');
    await user.type(screen.getByTestId('refire-max-attempts'), '3');
    // React controlled numeric input: clear then type to avoid appending.
    await user.clear(screen.getByTestId('refire-backoff-seconds'));
    await user.type(screen.getByTestId('refire-backoff-seconds'), '30');
    await user.type(screen.getByTestId('refire-transient'), 'OOMKILLED');
    await user.click(screen.getByTestId('refire-save'));

    expect(await screen.findByTestId('refire-saved')).toBeInTheDocument();
    expect(bodies).toHaveLength(1);
    expect(bodies[0].projectId).toBe('alpha');
    // Protojson omits the empty-string default route from the wire; castle
    // decodes the missing field as the project default row ('').
    expect((bodies[0].route ?? '')).toBe('');
    expect(bodies[0].policy).toEqual({
      maxAttempts: 3,
      backoffSeconds: 30,
      transient: ['OOMKILLED'],
    });
  });

  test('a set policy row loads its budgets for editing', async () => {
    server.use(
      http.post(projectPath('GetRefirePolicy'), () =>
        HttpResponse.json({
          policy: { max_attempts: 5, backoff_seconds: 60, transient: ['timeout'], parks: [], cosmetics: [] },
          updated_at: '2026-01-01T00:00:00.000Z',
        }),
      ),
    );

    renderProjectDetail('alpha');

    await screen.findByTestId('refire-budgets');
    expect(await screen.findByTestId('refire-max-attempts')).toHaveValue(5);
    expect(screen.getByTestId('refire-backoff-seconds')).toHaveValue(60);
    expect(screen.getByTestId('refire-transient')).toHaveValue('timeout');
  });

  test('saves a custom per-route row by exact route key', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('SetRefirePolicy'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({ updated_at: '2026-01-02T00:00:00.000Z' });
      }),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('refire-budgets');
    await user.selectOptions(screen.getByTestId('refire-route-select'), 'custom');
    await user.type(screen.getByTestId('refire-route-input'), 'deploy/build');
    // Switching the route key starts a fresh policy read; wait for the
    // reloaded budgets row before editing it.
    await screen.findByTestId('refire-budgets');
    await user.type(screen.getByTestId('refire-max-attempts'), '2');
    await user.click(screen.getByTestId('refire-save'));

    expect(bodies).toHaveLength(1);
    expect(bodies[0].projectId).toBe('alpha');
    expect(bodies[0].route).toBe('deploy/build');
  });
});

describe('ProjectDetailPage members editor', () => {
  test('a CLOSED project carries the membership hint and lists members', async () => {
    renderProjectDetail('beta');

    expect(await screen.findByTestId('member-closed-hint')).toHaveTextContent(
      /only console path in/i,
    );
    const rows = await screen.findAllByTestId('member-row');
    expect(rows).toHaveLength(2);
    expect(screen.getByTestId(`member-remove-human-ops`)).toBeInTheDocument();
  });

  test('adds a human member into the CLOSED project (the only console path in)', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('ListProjectMembers'), () =>
        HttpResponse.json({
          members: [
            { project_id: 'beta', member_id: 'ov-2', kind: 'PROJECT_MEMBER_KIND_AI' },
            { project_id: 'beta', member_id: 'human-ops', kind: 'PROJECT_MEMBER_KIND_HUMAN' },
            { project_id: 'beta', member_id: 'human-2', kind: 'PROJECT_MEMBER_KIND_HUMAN' },
          ],
        }),
      ),
    );
    server.use(
      http.post(projectPath('AddProjectMember'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({
          member: {
            project_id: 'beta',
            member_id: 'human-2',
            kind: 'PROJECT_MEMBER_KIND_HUMAN',
          },
        });
      }),
    );

    const user = userEvent.setup();
    renderProjectDetail('beta');

    await user.type(await screen.findByTestId('member-add-id'), 'human-2');
    await user.selectOptions(screen.getByTestId('member-add-kind'), 'human');
    await user.click(screen.getByTestId('member-add-submit'));

    expect(bodies[0].projectId).toBe('beta');
    expect(bodies[0].memberId).toBe('human-2');
    expect(bodies[0].kind).toBe('PROJECT_MEMBER_KIND_HUMAN');
    expect(await screen.findByTestId('member-remove-human-2')).toBeInTheDocument();
  });

  test('removes a member and tolerates repeated removals (server idempotence)', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    // Keep the list stable so the second (idempotent) click finds its row
    // even though the default handler would drop it from module state.
    server.use(
      http.post(projectPath('ListProjectMembers'), () =>
        HttpResponse.json({
          members: [
            { project_id: 'beta', member_id: 'ov-2', kind: 'PROJECT_MEMBER_KIND_AI' },
            { project_id: 'beta', member_id: 'human-ops', kind: 'PROJECT_MEMBER_KIND_HUMAN' },
          ],
        }),
      ),
    );
    server.use(
      http.post(projectPath('RemoveProjectMember'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({});
      }),
    );

    const user = userEvent.setup();
    renderProjectDetail('beta');

    await user.click(await screen.findByTestId('member-remove-ov-2'));
    await user.click(screen.getByTestId('member-remove-ov-2'));

    expect(bodies).toHaveLength(2);
    expect(bodies[1].projectId).toBe('beta');
    expect(bodies[1].memberId).toBe('ov-2');
  });

  test('surfaces a denied membership write', async () => {
    server.use(
      http.post(projectPath('AddProjectMember'), () =>
        new HttpResponse(
          JSON.stringify({ code: 'permission_denied', message: 'requires project admin credentials' }),
          { status: 403, headers: { 'content-type': 'application/json' } },
        ),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('beta');

    await user.type(await screen.findByTestId('member-add-id'), 'denied');
    await user.click(screen.getByTestId('member-add-submit'));

    expect(await screen.findByTestId('member-add-error')).toHaveTextContent(
      /denied the write/i,
    );
  });

  test('blocks an empty member id client-side', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('AddProjectMember'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({});
      }),
    );

    const user = userEvent.setup();
    renderProjectDetail('beta');

    await user.click(await screen.findByTestId('member-add-submit'));

    expect(await screen.findByTestId('member-add-error')).toHaveTextContent(/required/i);
    expect(bodies).toHaveLength(0);
  });
});