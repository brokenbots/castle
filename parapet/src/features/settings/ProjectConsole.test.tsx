// KB-340: the per-project console tests drive the tab strip in
// ProjectDetailPage and the panels behind it — the acceptance flow runs
// entirely through these screens (runs incl. the admin unscoped arm,
// identity blocks, catalog + interface viewer, routes + config editors
// with the live validator preview, etag-swapping saves and conflict
// handling). Fixtures ride the shared run-viewer msw store.
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, test } from 'vitest';
import { ProjectDetailPage } from './ProjectDetailPage';
import { settingsApi, SETTINGS_API_REDUCER_PATH } from './settingsApi';
import { createRunViewerStore, sessionRecovered } from '@castle/run-viewer';
import { API_VERSION, KIND_INTERFACE, KIND_RECORD } from './record';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import { identityPath, projectPath, resetProjectRuns, seedProjectConfig, seedProjectRuns } from '@castle/run-viewer/src/test/mocks/handlers';

const configRecord = {
  apiVersion: API_VERSION,
  kind: KIND_RECORD,
  registryName: 'alpha',
  namespace: 'alpha-ns',
  catalog: {
    'ship-it': {
      type: 'url',
      url: 'https://git.example/alpha/ship-it',
      ref: 'main',
      interface: {
        apiVersion: API_VERSION,
        kind: KIND_INTERFACE,
        workflow: 'ship-it',
        configSlots: [
          { slot: 'buildCmd', variable: 'custom_build', target: 'ci' },
        ],
      },
    },
    'lint-only': { type: 'image', image: 'registry.example/lint@sha256:zz', class: 'triage' },
  },
  routes: [
    { name: 'triage-runs', workflow: 'lint-only', project: 'alpha', states: ['Triage'] },
  ],
  digests: { 'ship-it': 'a'.repeat(64) },
};

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

type Identity = {
  operator_id: string;
  name?: string;
  display_name?: string;
  role: 'admin' | 'operator' | string;
  membership: string[];
  membership_scoped: boolean;
};

function installIdentity(identity: Identity) {
  server.use(
    http.post(identityPath('GetCallerIdentity'), () =>
      HttpResponse.json({ attributes: { role: identity.role, display: identity.operator_id }, ...identity }),
    ),
  );
}

function seedAlpha(): string {
  return seedProjectConfig('alpha', JSON.stringify(configRecord));
}

beforeEach(() => {
  resetProjectRuns();
});

afterEach(() => {
  cleanup();
});

async function renderConsole(projectId: string) {
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
  await screen.findByTestId('project-detail');
}

async function openTab(tab: string) {
  const user = userEvent.setup();
  await user.click(screen.getByTestId(`project-tab-${tab}`));
  await screen.findByTestId(`project-panel-${tab}`);
}

describe('console tab strip', () => {
  test('policy is the default tab; new console tabs render on demand', async () => {
    await renderConsole('alpha');

    expect(screen.getByTestId('project-console-tabs')).toBeInTheDocument();
    expect(screen.getByTestId('project-tab-runs')).toBeInTheDocument();
    // Policy stack stays mounted by default: settings, refire, members.
    expect(screen.getByTestId('project-settings')).toBeInTheDocument();
    expect(screen.getByTestId('project-refire')).toBeInTheDocument();
    expect(screen.getByTestId('project-members')).toBeInTheDocument();
    expect(screen.queryByTestId('project-runs')).not.toBeInTheDocument();
  });

  test('switching tabs mounts the console panel and unmounts the policy stack', async () => {
    await renderConsole('alpha');
    await openTab('runs');
    expect(screen.getByTestId('project-runs')).toBeInTheDocument();
    expect(screen.queryByTestId('project-settings')).not.toBeInTheDocument();
  });
});

describe('runs panel', () => {
  test('renders scoped runs for an admin with the unscoped toggle', async () => {
    seedAlpha();
    seedProjectRuns('alpha', [runWire('alpha-run-1', 'running', 'alpha')]);
    seedProjectRuns(null, [runWire('legacy-run', 'running', null)]);
    await renderConsole('alpha');
    await openTab('runs');

    expect(await screen.findByTestId('runs-table')).toBeInTheDocument();
    expect(screen.getByTestId('runs-row-alpha-run-1')).toBeInTheDocument();
    // Scoped-only by default: a legacy unscoped row is invisible until the
    // admin opts in.
    expect(screen.queryByTestId('runs-row-legacy-run')).not.toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByTestId('runs-include-unscoped'));
    expect(await screen.findByTestId('runs-row-legacy-run')).toBeInTheDocument();
  });

  test('non-members get the membership honesty state instead of a denied fetch', async () => {
    seedAlpha();
    seedProjectRuns('alpha', [runWire('alpha-run-2', 'running', 'alpha')]);
    installIdentity({
      operator_id: 'outside-ops',
      role: 'operator',
      membership: [],
      membership_scoped: true,
    });
    const runCalls: number[] = [];
    server.use(
      http.post(projectPath('ListProjectRuns'), () => {
        runCalls.push(1);
        return HttpResponse.json({ runs: [], next_page_token: '' });
      }),
    );
    await renderConsole('alpha');
    await openTab('runs');

    const honest = await screen.findByTestId('runs-no-membership');
    expect(honest).toHaveTextContent("This project's runs are not visible to you.");
    expect(within(honest).getByText(/not a member of this project/)).toBeInTheDocument();
    // The query stayed skipped — castle would reject, so the UI does not ask.
    await screen.findByTestId('project-runs');
    expect(runCalls).toHaveLength(0);
    // The filter + unscoped toggle disable while the caller cannot read.
    expect(screen.getByTestId('runs-status-filter')).toBeDisabled();
    expect(screen.getByTestId('runs-include-unscoped')).toBeDisabled();
  });

  test('status filter requeries and paging walks pages then back', async () => {
    seedAlpha();
    const rows = Array.from({ length: 51 }, (_, i) => runWire(`page-run-${i + 1}`, i === 40 ? 'pending' : 'running', 'alpha'));
    seedProjectRuns('alpha', rows);
    await renderConsole('alpha');
    await openTab('runs');

    expect(await screen.findByTestId('runs-table')).toBeInTheDocument();
    // limit 50, so page 1 ends with a next-token.
    expect(screen.getByTestId('runs-next-page')).toBeInTheDocument();
    expect(screen.getByTestId('runs-row-page-run-50')).toBeInTheDocument();
    expect(screen.queryByTestId('runs-row-page-run-51')).not.toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByTestId('runs-next-page'));
    await waitFor(() => expect(screen.getByTestId('runs-row-page-run-51')).toBeInTheDocument());
    expect(screen.getByTestId('runs-prev-page')).toBeInTheDocument();

    // Going back to the first page.
    await user.click(screen.getByTestId('runs-prev-page'));
    await waitFor(() => expect(screen.getByTestId('runs-row-page-run-1')).toBeInTheDocument());

    // A pending filter requeries from the first page and keeps the one pending row.
    await user.selectOptions(screen.getByTestId('runs-status-filter'), 'pending');
    await waitFor(() => {
      expect(screen.getByTestId('runs-row-page-run-41')).toBeInTheDocument();
      expect(screen.queryByTestId('runs-row-page-run-1')).not.toBeInTheDocument();
    });
  });
});

describe('agents panel', () => {
  test('admitted ai members get identity blocks; non-members do not', async () => {
    seedAlpha();
    await renderConsole('alpha');
    await openTab('agents');

    const blocks = await screen.findByTestId('agents-identity-blocks');
    const ov1 = within(blocks).getByTestId('agent-block-ov-1');
    expect(ov1).toHaveTextContent('ov-1');
    // The human member gets no identity block (agents are ai operators).
    expect(within(blocks).queryByTestId('agent-block-human-ops')).not.toBeInTheDocument();
  });
});

describe('catalog panel', () => {
  test('lists catalog entries with digests and opens the interface viewer', async () => {
    seedAlpha();
    await renderConsole('alpha');
    await openTab('catalog');

    const row = await screen.findByTestId('catalog-row-ship-it');
    expect(row).toHaveTextContent('https://git.example/alpha/ship-it@main');
    expect(row).toHaveTextContent('a'.repeat(64));

    const user = userEvent.setup();
    await user.click(screen.getByTestId('catalog-select-ship-it'));
    const viewer = await screen.findByTestId('catalog-interface-viewer');
    // The KB-310 slot→variable chips: the record carries variable "custom_build";
    // the viewer renders the resolved KB-310 variable for the slot.
    expect(viewer).toHaveTextContent('ship-it');
    expect(viewer.textContent).toContain('buildCmd');
    expect(viewer.textContent).toContain('custom_build');
  });

  test('an unset record gets the config-first honesty empty state', async () => {
    await renderConsole('beta');
    await openTab('catalog');
    expect(await screen.findByTestId('catalog-empty-unset')).toHaveTextContent(/create one in the Config tab/);
  });
});

describe('routes editor', () => {
  test('renders the recorded routes and swaps the record under the etag', async () => {
    let currentEtag = seedAlpha();
    const saveBodies: Record<string, unknown>[] = [];
    server.use(
      http.post(projectPath('SetProjectConfig'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        saveBodies.push(body);
        const etag = seedProjectConfig('alpha', decodeRecordDoc(body.document));
        currentEtag = etag;
        return HttpResponse.json({ etag });
      }),
    );
    await renderConsole('alpha');
    await openTab('routes');

    const readEtag = currentEtag;
    await screen.findByTestId('routes-etag');
    await waitFor(() => expect(screen.getByTestId('routes-etag')).toHaveTextContent('etag ' + readEtag));
    expect(await screen.findByTestId('routes-row-0')).toBeInTheDocument();
    expect(screen.getByTestId('routes-states-0')).toHaveValue('Triage');

    // Edit the recorded route's tags and save: the whole record rides through
    // with the etag it was read under.
    const user = userEvent.setup();
    await user.type(screen.getByTestId('routes-tags-0'), 'nightly');
    await user.click(screen.getByTestId('routes-save'));
    expect(saveBodies).toHaveLength(1);
    expect(String(saveBodies[0].expected_etag ?? saveBodies[0].expectedEtag)).toBe(readEtag);
    // The swapped etag moved the record forward.
    expect(currentEtag).not.toBe(readEtag);
    expect(await screen.findByTestId('routes-saved')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('routes-etag')).toHaveTextContent('etag ' + currentEtag));
  });

  test('an added route previews the record validator and a conflict offers reload', async () => {
    seedAlpha();
    let rejectSave = true;
    server.use(
      http.post(projectPath('SetProjectConfig'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        if (rejectSave) {
          return HttpResponse.json(
            { code: 'failed_precondition', message: 'config record was modified concurrently' },
            { status: 412 },
          );
        }
        return HttpResponse.json({ etag: seedProjectConfig('alpha', decodeRecordDoc(body.document)) });
      }),
    );
    await renderConsole('alpha');
    await openTab('routes');

    const user = userEvent.setup();
    await user.click(await screen.findByTestId('routes-add'));
    const row1 = await screen.findByTestId('routes-row-1');
    // The new route is blank: the per-row honesty note asks for a workflow pick.
    expect(within(row1).getByText(/Pick the workflow this route targets\./)).toBeInTheDocument();
    // The live record preview mirrors the server validator before any save.
    const preview = await screen.findByTestId('routes-validation');
    expect(preview).toHaveTextContent(/routes\[1\]\.name/);
    expect(preview).toHaveTextContent(/workflow "" is not present/);

    // The save hits the 412 (stale etag in this fixture) → conflict banner + reload.
    await user.click(screen.getByTestId('routes-save'));
    expect(await screen.findByTestId('routes-conflict')).toHaveTextContent(/changed while you were editing/);
    rejectSave = false;
    // Simulate the concurrent change having landed: re-read shows a fresh etag.
    seedProjectConfig('alpha', JSON.stringify({ ...configRecord, namespace: 'alpha-ns-v2' }));
    await user.click(screen.getByTestId('routes-reload'));

    // After reload the editor re-renders with a fresh record and the same route count.
    await waitFor(() => expect(screen.getByTestId('routes-etag')).toHaveTextContent(/etag cfg-etag-alpha/));
    expect(screen.getByTestId('routes-row-0')).toBeInTheDocument();
    expect(screen.queryByTestId('routes-conflict')).not.toBeInTheDocument();
  });

  test('non-admins see the routes read-only', async () => {
    seedAlpha();
    installIdentity({
      operator_id: 'member-ops',
      role: 'operator',
      membership: ['alpha'],
      membership_scoped: true,
    });
    await renderConsole('alpha');
    await openTab('routes');

    expect(await screen.findByTestId('routes-row-0')).toBeInTheDocument();
    expect(screen.queryByTestId('routes-save')).not.toBeInTheDocument();
    expect(screen.queryByTestId('routes-add')).not.toBeInTheDocument();
    expect(screen.getByTestId('routes-states-0')).toBeDisabled();
  });
});

describe('config editor', () => {
  test('bootstraps an unset record, validates the draft live, and saves through the etag swap', { timeout: 20_000 }, async () => {
    // Step-dense admin flow (bootstrap → header → catalog import → KB-310
    // configLibrary entry → etag swap); under a saturated runner the 5s
    // default trips mid-flow.
    const saveBodies: Record<string, unknown>[] = [];
    server.use(
      http.post(projectPath('SetProjectConfig'), async ({ request }) => {
        const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
        saveBodies.push(body);
        return HttpResponse.json({ etag: seedProjectConfig('alpha', decodeRecordDoc(body.document)) });
      }),
    );
    await renderConsole('beta');
    await openTab('config');

    expect(await screen.findByTestId('config-empty-unset')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByTestId('config-bootstrap'));

    // The drafted record shell: empty namespace shows in the validator preview.
    const preview = await screen.findByTestId('config-validation');
    expect(preview).toHaveTextContent(/namespace "" is not a DNS-1123 label/);
    expect(screen.getByTestId('config-registryname')).toHaveValue('beta');

    await user.type(screen.getByTestId('config-namespace'), 'alpha-ns');
    await waitFor(() => expect(screen.queryByTestId('config-validation')).not.toBeInTheDocument());

    // Import a workflow through the record catalog.
    await user.type(screen.getByTestId('config-catalog-add-name'), 'ship-it');
    await user.click(screen.getByTestId('config-catalog-add-submit'));
    await user.type(screen.getByTestId('config-catalog-ship-it-url'), 'https://git.example/alpha/ship-it');
    expect(screen.getByTestId('config-catalog-ship-it-url')).toHaveValue('https://git.example/alpha/ship-it');

    // Add a config entry that feeds the KB-310 slot variables.
    await user.type(screen.getByTestId('config-add-name'), 'ci');
    await user.click(screen.getByTestId('config-add-submit'));
    const buildCmd = await screen.findByTestId('config-ci-buildCmd');
    expect(buildCmd.parentElement as HTMLElement).toHaveTextContent('→ build_cmd');
    await user.type(buildCmd, 'make build');

    // A missing url for a url-type workflow is a live validator error.
    expect(screen.getByTestId('config-validation')).toBeInTheDocument();

    await user.click(screen.getByTestId('config-save'));
    await screen.findByTestId('config-saved');
    const saved = JSON.parse(decodeRecordDoc(saveBodies[0].document)) as Record<string, unknown>;
    expect((saved.catalog as Record<string, unknown>)['ship-it']).toMatchObject({
      type: 'url',
      url: 'https://git.example/alpha/ship-it',
    });
    expect((saved.configLibrary as Record<string, unknown>).ci).toEqual({ buildCmd: 'make build' });
  });

  test('an interface paste that is not JSON is flagged before save', async () => {
    seedAlpha();
    await renderConsole('alpha');
    await openTab('config');

    const iface = await screen.findByTestId('config-catalog-ship-it-interface');
    expect(await screen.findByTestId('config-catalog-row-ship-it')).toBeInTheDocument();
    expect((iface as HTMLTextAreaElement).value).toContain('configSlots');

    const user = userEvent.setup();
    await user.type(iface! as HTMLTextAreaElement, '{{not json');
    expect(await screen.findByTestId('config-catalog-ship-it-parse-error')).toHaveTextContent(/not valid JSON yet/);
    expect(screen.queryByText(/not a ProjectWorkflowConfig record/)).not.toBeInTheDocument();
  });
});

describe('binding panel', () => {
  test('renders the record binding shapes read-only with validator feedback', async () => {
    const withBinding = {
      ...configRecord,
      binding: {
        apiVersion: API_VERSION,
        kind: 'CriteriaBinding',
        images: { 'workflow-runner': 'registry.example/criteria@sha256:aa' },
        env: {},
        volumes: [],
        secrets: [],
        config: {},
      },
    };
    seedProjectConfig('alpha', JSON.stringify(withBinding));
    await renderConsole('alpha');
    await openTab('binding');

    expect(await screen.findByTestId('binding-block-images')).toHaveTextContent('registry.example/criteria@sha256:aa');
    expect(screen.getByTestId('binding-block-env')).toBeInTheDocument();
    expect(screen.getByTestId('binding-block-secrets')).toBeInTheDocument();
    expect(screen.queryByTestId('binding-errors')).not.toBeInTheDocument();
  });

  test('an unset record answers the empty-state honesty note', async () => {
    await renderConsole('beta');
    await openTab('binding');
    expect(await screen.findByTestId('binding-empty-unset')).toHaveTextContent(/config record is unset/);
  });
});

/** Decode the bytes document msw carries on the wire for assertions. */
function decodeRecordDoc(document: unknown): string {
  const rec = (document ?? {}) as { document?: string };
  const doc = typeof rec.document === 'string' ? rec.document : ((document as string) ?? '');
  if (!doc) return '';
  return decodeURIComponent(escape(atob(doc)));
}