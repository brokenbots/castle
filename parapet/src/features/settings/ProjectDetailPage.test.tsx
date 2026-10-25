import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
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
import { identityPath, projectPath } from '@castle/run-viewer/src/test/mocks/handlers';

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

  test('renders the invalid state for a percent-bearing deep link without crashing', async () => {
    // /settings/projects/100%25 arrives as the param '100%' (react-router
    // already percent-decodes path params); the page must reject the slug
    // via validateProjectID instead of throwing a URIError and blanking.
    renderProjectDetail('100%25');

    expect(await screen.findByTestId('project-detail-invalid')).toBeInTheDocument();
    expect(screen.queryByTestId('project-detail')).not.toBeInTheDocument();
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
    // The removal empties the draft: no entry rows remain in the form.
    await user.click(screen.getByTestId('project-settings-save'));

    // An entirely empty map is valid doc content; protojson leaves empty
    // maps off the wire and castle decodes the field as the empty doc.
    expect(bodies[0].projectId).toBe('alpha');
    expect(bodies[0].settings ?? {}).toEqual({});
  });

  test('surfaces a denied settings write', async () => {
    // Castle denies settings writes to protocol-class credentials; the
    // console must show the denial instead of a silent no-op.
    server.use(
      http.post(projectPath('SetProjectSettings'), () =>
        new HttpResponse(
          JSON.stringify({ code: 'permission_denied', message: 'requires project admin credentials' }),
          { status: 403, headers: { 'content-type': 'application/json' } },
        ),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('project-setting-value-0');
    await user.click(screen.getByTestId('project-settings-save'));

    expect(await screen.findByTestId('project-settings-denied')).toHaveTextContent(
      /denied the write/i,
    );
    expect(screen.queryByTestId('project-settings-saved')).not.toBeInTheDocument();
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

  test('an over-budget vocabulary line blocks the save client-side', async () => {
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
    // One 16 KiB + 1 byte line exceeds the per-value budget the server
    // enforces; the editor must refuse to send the shape-violating policy.
    fireEvent.change(screen.getByTestId('refire-transient'), {
      target: { value: 'x'.repeat(16 * 1024 + 1) },
    });

    expect(await screen.findByTestId('refire-vocab-error')).toHaveTextContent(
      /1\.\.16384 bytes/,
    );
    expect(screen.getByTestId('refire-save')).toBeDisabled();
    await user.click(screen.getByTestId('refire-save'));
    expect(bodies).toHaveLength(0);
  });

  test('surfaces a server-rejected refire save, not just denials', async () => {
    server.use(
      http.post(projectPath('SetRefirePolicy'), () =>
        new HttpResponse(
          JSON.stringify({ code: 'invalid_argument', message: 'transient vocabulary exceeds budget' }),
          { status: 400, headers: { 'content-type': 'application/json' } },
        ),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('refire-budgets');
    await user.type(screen.getByTestId('refire-transient'), 'OOMKILLED');
    await user.click(screen.getByTestId('refire-save'));

    expect(await screen.findByTestId('refire-save-error')).toHaveTextContent(
      /rejected the save/i,
    );
    expect(screen.getByTestId('refire-save-error')).toHaveTextContent(
      /transient vocabulary exceeds budget/,
    );
    expect(screen.queryByTestId('refire-saved')).not.toBeInTheDocument();
  });

  test('surfaces a denied refire write', async () => {
    server.use(
      http.post(projectPath('SetRefirePolicy'), () =>
        new HttpResponse(
          JSON.stringify({ code: 'permission_denied', message: 'requires project admin credentials' }),
          { status: 403, headers: { 'content-type': 'application/json' } },
        ),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('refire-budgets');
    await user.type(screen.getByTestId('refire-max-attempts'), '3');
    await user.click(screen.getByTestId('refire-save'));

    expect(await screen.findByTestId('refire-denied')).toHaveTextContent(/denied the write/i);
    expect(screen.queryByTestId('refire-saved')).not.toBeInTheDocument();
  });

  test('a custom route selection with an empty key is refused, not silently defaulted', async () => {
    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('refire-budgets');
    await user.selectOptions(screen.getByTestId('refire-route-select'), 'custom');

    // An empty custom route would target the project default row on the
    // wire; the editor requires the key the operator asked for.
    expect(await screen.findByTestId('refire-route-error')).toHaveTextContent(
      /Route key is required/,
    );
    expect(screen.getByTestId('refire-save')).toBeDisabled();
  });

  test('the saved banner survives the invalidation refetch of an existing row', async () => {
    let saved = false;
    server.use(
      http.post(projectPath('GetRefirePolicy'), () =>
        HttpResponse.json(
          saved
            ? {
                policy: { max_attempts: 4, backoff_seconds: 5, transient: ['timeout'] },
                updated_at: '2026-01-02T00:00:00.000Z',
              }
            : {
                policy: { max_attempts: 2, backoff_seconds: 5, transient: ['timeout'] },
                updated_at: '2026-01-01T00:00:00.000Z',
              },
        ),
      ),
      http.post(projectPath('SetRefirePolicy'), async () => {
        saved = true;
        return HttpResponse.json({ updated_at: '2026-01-02T00:00:00.000Z' });
      }),
    );

    const user = userEvent.setup();
    renderProjectDetail('alpha');

    await screen.findByTestId('refire-budgets');
    await user.clear(screen.getByTestId('refire-max-attempts'));
    await user.type(screen.getByTestId('refire-max-attempts'), '4');
    await user.click(screen.getByTestId('refire-save'));

    expect(await screen.findByTestId('refire-saved')).toBeInTheDocument();
    // The invalidation refetch lands the row the new updatedAt describes;
    // the form re-anchors to it and the banner must still be visible.
    await waitFor(() => expect(screen.getByTestId('refire-max-attempts')).toHaveValue(4));
    expect(screen.getByTestId('refire-saved')).toBeInTheDocument();
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

  test('surfaces a denied member removal instead of swallowing it', async () => {
    server.use(
      http.post(projectPath('RemoveProjectMember'), () =>
        new HttpResponse(
          JSON.stringify({ code: 'permission_denied', message: 'requires project admin credentials' }),
          { status: 403, headers: { 'content-type': 'application/json' } },
        ),
      ),
    );

    const user = userEvent.setup();
    renderProjectDetail('beta');

    await user.click(await screen.findByTestId('member-remove-ov-2'));

    expect(await screen.findByTestId('member-remove-error')).toHaveTextContent(
      /denied the write/i,
    );
  });
});
// KB-333: every project-admin write affordance gates on the operator's
// role attribute; non-admins get read-only editors while admins keep the
// page exactly as before. The server still enforces the writes.
describe('ProjectDetailPage admin pre-gate (KB-333)', () => {
  function installIdentity(admin: boolean) {
    server.use(
      http.post(identityPath('GetCallerIdentity'), () =>
        HttpResponse.json({
          operator_id: 'console-default',
          name: 'operator',
          display_name: 'operator',
          role: admin ? 'admin' : 'agent',
          attributes: {},
          membership_scoped: true,
          membership: ['alpha'],
        }),
      ),
    );
  }

  test('a non-admin operator sees read-only editors with no write affordances', async () => {
    installIdentity(false);
    renderProjectDetail('alpha');

    expect(await screen.findByTestId('project-detail')).toBeInTheDocument();

    // Settings editor: rows visible, inputs disabled, write controls gone.
    expect(await screen.findByTestId('project-settings-admin-required')).toBeInTheDocument();
    expect(await screen.findByTestId('project-setting-key-0')).toBeDisabled();
    expect(screen.getByTestId('project-setting-value-0')).toBeDisabled();
    expect(screen.queryByTestId('project-setting-add-row')).not.toBeInTheDocument();
    expect(screen.queryByTestId('project-settings-save')).not.toBeInTheDocument();

    // Refire editor: budgets and vocabularies read-only, save gone.
    expect(await screen.findByTestId('refire-admin-required')).toBeInTheDocument();
    expect(await screen.findByTestId('refire-max-attempts')).toBeDisabled();
    expect(screen.getByTestId('refire-transient')).toBeDisabled();
    expect(screen.queryByTestId('refire-save')).not.toBeInTheDocument();

    // Members editor: roster visible, add/remove gone.
    expect(await screen.findByTestId('member-admin-required')).toBeInTheDocument();
    expect(screen.getByTestId('project-members')).toBeInTheDocument();
    expect(screen.queryByTestId('member-add')).not.toBeInTheDocument();
    expect(screen.queryByTestId('member-remove-ov-1')).not.toBeInTheDocument();
  });

  test('an admin identity keeps every write affordance (as today)', async () => {
    installIdentity(true);
    renderProjectDetail('alpha');

    expect(await screen.findByTestId('project-settings-save')).toBeInTheDocument();
    expect(screen.getByTestId('project-setting-add-row')).toBeInTheDocument();
    expect(screen.getByTestId('project-setting-key-0')).toBeEnabled();
    // Each editor loads its own data; await each affordance separately so
    // one editor's slower fetch cannot fail the next editor's assertion.
    expect(await screen.findByTestId('refire-save')).toBeInTheDocument();
    expect(await screen.findByTestId('refire-max-attempts')).toBeEnabled();
    expect(await screen.findByTestId('member-add')).toBeInTheDocument();
    expect(await screen.findByTestId('member-remove-ov-1')).toBeInTheDocument();
    expect(screen.queryByTestId('project-settings-admin-required')).not.toBeInTheDocument();
  });

  test('a failed identity read keeps the editors closed', async () => {
    server.use(
      http.post(identityPath('GetCallerIdentity'), () =>
        new HttpResponse(JSON.stringify({ code: 'unavailable', message: 'offline' }), {
          status: 503,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );
    renderProjectDetail('alpha');

    expect(await screen.findByTestId('project-settings-admin-required')).toHaveTextContent(
      'could not be verified',
    );
    expect(await screen.findByTestId('refire-admin-required')).toHaveTextContent(
      'could not be verified',
    );
    expect(await screen.findByTestId('member-admin-required')).toHaveTextContent(
      'could not be verified',
    );
    expect(screen.queryByTestId('project-settings-save')).not.toBeInTheDocument();
    expect(screen.queryByTestId('refire-save')).not.toBeInTheDocument();
    expect(screen.queryByTestId('member-add')).not.toBeInTheDocument();
  });
});
