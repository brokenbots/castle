import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test } from 'vitest';
import { ProjectListPage } from './ProjectListPage';
import { ProjectDetailPage } from './ProjectDetailPage';
import { settingsApi, SETTINGS_API_REDUCER_PATH } from './settingsApi';
import { createRunViewerStore } from '@castle/run-viewer';
import { sessionRecovered } from '@castle/run-viewer';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import { identityPath, projectPath } from '@castle/run-viewer/src/test/mocks/handlers';

function renderProjectsPage(initialPath = '/settings/projects') {
  const store = createRunViewerStore({
    extraReducers: { [SETTINGS_API_REDUCER_PATH]: settingsApi.reducer },
    extraMiddleware: [settingsApi.middleware],
    extraApiPaths: [SETTINGS_API_REDUCER_PATH],
  });
  store.dispatch(sessionRecovered());
  const utils = render(
    <Provider store={store}>
      <MemoryRouter initialEntries={[initialPath]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <Routes>
          <Route path="/settings/projects" element={<ProjectListPage />} />
          <Route path="/settings/projects/:projectId" element={<ProjectDetailPage />} />
        </Routes>
      </MemoryRouter>
    </Provider>,
  );
  return { store, ...utils };
}

afterEach(() => {
  cleanup();
});

describe('ProjectListPage', () => {
  test('lists projects with their membership policy and links into the detail editor', async () => {
    renderProjectsPage();

    const rows = await screen.findAllByTestId('project-list-row');
    expect(rows).toHaveLength(2);
    expect(screen.getByRole('link', { name: 'alpha' })).toHaveAttribute(
      'href',
      '/settings/projects/alpha',
    );
    expect(screen.getByRole('link', { name: 'beta' })).toHaveAttribute(
      'href',
      '/settings/projects/beta',
    );
    // Membership policy lands in the row text so operators see the
    // CLOSED-before-clicking distinction.
    const betaRow = screen.getByRole('link', { name: 'beta' }).closest('tr');
    expect(betaRow).toHaveTextContent('closed');
  });

  test('creates a project and navigates into its detail editor', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('CreateProject'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({
          project: {
            id: 'gamma',
            membership_policy: 'PROJECT_MEMBERSHIP_POLICY_CLOSED',
          },
        });
      }),
    );
    // The detail page the navigation lands on needs its reads.
    server.use(
      http.post(projectPath('GetProject'), () =>
        HttpResponse.json({
          project: { id: 'gamma', membership_policy: 'PROJECT_MEMBERSHIP_POLICY_CLOSED' },
        }),
      ),
    );
    server.use(
      http.post(projectPath('GetProjectSettings'), () => HttpResponse.json({ settings: {}, etag: '' })),
    );
    server.use(
      http.post(projectPath('GetRefirePolicy'), () =>
        HttpResponse.json({ code: 'not_found', message: 'refire policy not set' }, { status: 404 }),
      ),
    );
    server.use(
      http.post(projectPath('ListProjectMembers'), () => HttpResponse.json({ members: [] })),
    );

    const user = userEvent.setup();
    renderProjectsPage();

    await user.type(await screen.findByTestId('project-create-slug'), 'gamma');
    await user.selectOptions(screen.getByTestId('project-create-policy'), 'closed');
    await user.click(screen.getByTestId('project-create-submit'));

    expect(bodies).toHaveLength(1);
    const project = bodies[0].project as Record<string, unknown>;
    expect(project.id).toBe('gamma');
    expect(project.membershipPolicy).toBe('PROJECT_MEMBERSHIP_POLICY_CLOSED');
    // The created project's editor renders after the navigation.
    expect(await screen.findByTestId('project-detail')).toBeInTheDocument();
    expect(await screen.findByText('gamma')).toBeInTheDocument();
  });

  test('rejects a malformed slug client-side without touching the wire', async () => {
    const bodies: Array<Record<string, unknown>> = [];
    server.use(
      http.post(projectPath('CreateProject'), async ({ request }) => {
        bodies.push((await request.json().catch(() => ({}))) as Record<string, unknown>);
        return HttpResponse.json({});
      }),
    );

    const user = userEvent.setup();
    renderProjectsPage();

    await user.type(await screen.findByTestId('project-create-slug'), 'Nope!');
    await user.click(screen.getByTestId('project-create-submit'));

    const alert = await screen.findByTestId('project-create-error');
    expect(alert).toHaveTextContent(/lowercase slug/i);
    expect(bodies).toHaveLength(0);
  });

  test('surfaces already_exists from the server', async () => {
    server.use(
      http.post(projectPath('CreateProject'), () =>
        new HttpResponse(JSON.stringify({ code: 'already_exists', message: 'project already exists' }), {
          status: 409,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );

    const user = userEvent.setup();
    renderProjectsPage();

    await user.type(await screen.findByTestId('project-create-slug'), 'alpha');
    await user.click(screen.getByTestId('project-create-submit'));

    expect(await screen.findByTestId('project-create-error')).toHaveTextContent(
      /already exists/i,
    );
  });

  test('shows the empty state when castle holds no projects', async () => {
    server.use(
      http.post(projectPath('ListProjects'), () => HttpResponse.json({ projects: [] })),
    );

    renderProjectsPage();

    expect(await screen.findByTestId('project-list-empty')).toBeInTheDocument();
  });
});
// KB-333: the creation form is a project-admin surface gated on the
// operator's role attribute, not on the console credential class. The
// server keeps enforcing the write either way.
describe('ProjectListPage admin pre-gate (KB-333)', () => {
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
          membership: ['alpha', 'beta'],
        }),
      ),
    );
  }

  test('a non-admin operator gets a notice instead of the creation form', async () => {
    installIdentity(false);
    renderProjectsPage();

    expect(await screen.findByTestId('project-create-admin-required')).toBeInTheDocument();
    expect(screen.queryByTestId('project-create')).not.toBeInTheDocument();
    // The read-only inventory still renders.
    expect((await screen.findAllByTestId('project-list-row')).length).toBe(2);
  });

  test('an admin identity keeps the creation form reachable (as today)', async () => {
    installIdentity(true);
    renderProjectsPage();

    expect(await screen.findByTestId('project-create')).toBeInTheDocument();
    expect(screen.queryByTestId('project-create-admin-required')).not.toBeInTheDocument();
  });

  test('a failed identity read keeps the creation form closed', async () => {
    server.use(
      http.post(identityPath('GetCallerIdentity'), () =>
        new HttpResponse(JSON.stringify({ code: 'unavailable', message: 'offline' }), {
          status: 503,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );
    renderProjectsPage();

    expect(await screen.findByTestId('project-create-admin-required')).toHaveTextContent(
      'could not be verified',
    );
    expect(screen.queryByTestId('project-create')).not.toBeInTheDocument();
  });
});
