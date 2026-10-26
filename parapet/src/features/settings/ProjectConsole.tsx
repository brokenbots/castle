// The project console's workflow surfaces (KB-340): Runs, Agents, the
// workflow catalog, the routes editor, the config editor and the binding
// payload viewer. Everything rides castle.v1.ProjectService through the
// settings slice; writes swap the whole config record under etag
// semantics, and editors preview the same record validation the server
// applies (the confighome mirror in ./recordValidation) so failures are
// fail-fast instead of save-round-trip.
import { useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import {
  PageState,
  RUN_STATUS_FILTER_VALUES,
  StickyTableHead,
  classifyError,
  useCallerIdentity,
  useGetAgentQuery,
  type PageErrorKind,
} from '@castle/run-viewer';
import type { Run } from '@castle/run-viewer';
import {
  emptyRecord,
  TYPE_URL,
  TYPE_IMAGE,
  KB310_CONFIG_SLOT_VARIABLES,
  type CfgRecord,
  type ConfigEntry,
  type Interface,
  type Route,
  type Workflow,
} from './record';
import {
  bindingErrors,
  newRoute,
  validateConfigEntryShape,
  validateRecord,
} from './recordValidation';
import {
  useGetProjectConfigQuery,
  useListProjectMembersQuery,
  useListProjectRunsQuery,
  useListWorkflowCatalogQuery,
  useSetProjectConfigMutation,
} from './settingsApi';
import type { ProjectMemberInfo } from './settingsApi';

// settingsApi errors arrive as {status, data} RTK error objects (shared
// with the settings editors).
export function errorOf(err: unknown): string {
  const { status } = (err ?? {}) as { status?: unknown };
  return typeof status === 'string' ? status : '';
}

const SECTION_BORDER = 'rounded-lg border border-line bg-surface p-4';

/** The caller's posture used by the console panels: admin vs project member. */
function useConsoleCaller(projectId: string): {
  resolved: boolean;
  isAdmin: boolean;
  isMember: boolean;
} {
  const { data, isError } = useCallerIdentity();
  const resolved = data !== undefined || isError;
  const isAdmin = data?.role === 'admin';
  const isMember = data?.membership.includes(projectId) ?? false;
  return { resolved, isAdmin, isMember };
}

// --- Runs panel: the project's work stream, newest-first ---

export function RunsPanel({ projectId }: { projectId: string }) {
  const { resolved, isAdmin, isMember } = useConsoleCaller(projectId);
  const [status, setStatus] = useState('');
  const [includeUnscoped, setIncludeUnscoped] = useState(false);
  const [token, setToken] = useState('');

  // Non-member non-admin callers are denied server-side; pre-empt the
  // guaranteed rejection and say why the view is empty (KB-333 honesty).
  const mayRead = isAdmin || isMember;
  const { data, isLoading, error, refetch } = useListProjectRunsQuery(
    { projectId, status, limit: 50, pageToken: token, includeUnscoped },
    { skip: !resolved || !mayRead },
  );
  const errorKind = error ? classifyError(error) : undefined;

  function requery(next: { status?: string; includeUnscoped?: boolean }) {
    // Changing the filter or the unscoped arm restarts the pagination.
    if (next.status !== undefined) setStatus(next.status);
    if (next.includeUnscoped !== undefined) setIncludeUnscoped(next.includeUnscoped);
    setToken('');
  }

  return (
    <section aria-label="Project runs" data-testid="project-runs" className={SECTION_BORDER}>
      <div className="mb-2 flex flex-wrap items-center gap-3">
        <h2 className="text-title font-semibold text-ink">Runs</h2>
        <select
          data-testid="runs-status-filter"
          aria-label="Status filter"
          value={status}
          disabled={!mayRead}
          onChange={(e) => requery({ status: e.target.value })}
          className="rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink disabled:opacity-70"
        >
          {RUN_STATUS_FILTER_VALUES.map((s) => (
            <option key={s} value={s}>
              {s === '' ? 'all statuses' : s}
            </option>
          ))}
        </select>
        <label className="flex items-center gap-1 text-meta text-ink-muted">
          <input
            type="checkbox"
            data-testid="runs-include-unscoped"
            checked={includeUnscoped}
            disabled={!isAdmin}
            onChange={(e) => requery({ includeUnscoped: e.target.checked })}
            className="size-4"
          />
          include legacy unscoped runs
        </label>
      </div>

      {!mayRead ? (
        <PageState
          error
          kind="not_found"
          title="This project's runs are not visible to you."
          detail={
            resolved
              ? 'Project runs require membership (or the admin role); your operator is not a member of this project.'
              : 'Verifying your operator membership…'
          }
          testId="runs-no-membership"
        />
      ) : isLoading ? (
        <PageState loading title="Loading runs…" testId="runs-loading" />
      ) : error ? (
        <PageState
          error
          kind={errorKind}
          title="Failed to load runs."
          onRetry={() => void refetch()}
          testId="runs-error"
        />
      ) : (data?.runs.length ?? 0) === 0 ? (
        <p data-testid="runs-empty" className="text-body text-ink-muted">
          {includeUnscoped
            ? 'No runs match this filter yet — the project has no scoped or legacy unscoped runs.'
            : 'No scoped runs yet.'}
        </p>
      ) : (
        <table className="mb-3 w-full text-sm" data-testid="runs-table">
          <StickyTableHead>
            <tr className="border-b border-line">
              <th className="py-2 pr-4 text-left font-medium">Run</th>
              <th className="py-2 pr-4 text-left font-medium">Status</th>
              <th className="py-2 pr-4 text-left font-medium">Workflow</th>
              <th className="py-2 text-left font-medium">Created</th>
            </tr>
          </StickyTableHead>
          <tbody>
            {data?.runs.map((r: Run) => (
              <tr key={r.runId} data-testid={`runs-row-${r.runId}`} className="border-b border-line">
                <td className="py-2 pr-4 font-mono text-ink">{r.runId}</td>
                <td className="py-2 pr-4 text-ink-muted">{r.status}</td>
                <td className="py-2 pr-4 text-ink">{r.workflowName}</td>
                <td className="py-2 text-ink-muted">
                  {r.createdAt ? new Date(r.createdAt).toLocaleString() : '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {data && (token !== '' || data.nextPageToken !== '') && (
        <div className="flex gap-2" data-testid="runs-paging">
          {token !== '' && (
            <button
              type="button"
              data-testid="runs-prev-page"
              onClick={() => setToken('')}
              className="rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface-raised"
            >
              First page
            </button>
          )}
          {data.nextPageToken !== '' && (
            <button
              type="button"
              data-testid="runs-next-page"
              onClick={() => setToken(data.nextPageToken)}
              className="rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface-raised"
            >
              Next page
            </button>
          )}
        </div>
      )}
    </section>
  );
}

// --- Agents panel: the project's admitted members + their identity blocks ---

export function AgentsPanel({
  projectId,
  children,
}: {
  projectId: string;
  /** The membership editor rides the policy tab; the agents tab embeds it. */
  children?: ReactNode;
}) {
  const { isMember, isAdmin } = useConsoleCaller(projectId);

  return (
    <section aria-label="Project agents" data-testid="project-agents" className={SECTION_BORDER}>
      <h2 className="text-title font-semibold text-ink">Agents</h2>
      <p className="mb-3 text-meta text-ink-muted">
        Admitted members and the operator identity blocks castle has registered for them.
      </p>
      {!isMember && !isAdmin ? (
        <PageState
          error
          kind="not_found"
          title="Members are not visible to you."
          detail="Project membership requires membership yourself (or the admin role)."
          testId="agents-no-membership"
        />
      ) : (
        <>
          {children}
          <AgentIdentityBlocks projectId={projectId} />
        </>
      )}
    </section>
  );
}

function AgentIdentityBlocks({ projectId }: { projectId: string }) {
  const { data: members, isLoading, error } = useListProjectMembersQuery(projectId);
  const aiMembers = (members ?? []).filter((m: ProjectMemberInfo) => m.kind === 'ai');

  if (error) {
    return (
      <PageState
        error
        kind={classifyError(error)}
        title="Failed to load project members."
        testId="agents-identity-error"
      />
    );
  }
  if (isLoading) {
    return (
      <p data-testid="agents-identity-loading" className="mb-2 text-meta text-ink-muted">
        Loading identity blocks…
      </p>
    );
  }
  return (
    <div className="mb-3 grid gap-3 sm:grid-cols-2" data-testid="agents-identity-blocks">
      {aiMembers.length === 0 ? (
        <p data-testid="agents-identity-empty" className="text-body text-ink-muted">
          No ai-operator members yet — admit one below to see its registered identity.
        </p>
      ) : (
        aiMembers.map((m) => <AgentIdentityBlock key={m.memberId} criteriaId={m.memberId} />)
      )}
    </div>
  );
}

function AgentIdentityBlock({ criteriaId }: { criteriaId: string }) {
  const { data, isLoading, error } = useGetAgentQuery(criteriaId);

  return (
    <div
      data-testid={`agent-block-${criteriaId}`}
      className="rounded-md border border-line bg-surface-raised p-3 text-sm"
    >
      <p className="font-mono text-body text-ink">{criteriaId}</p>
      {isLoading ? (
        <p data-testid={`agent-block-loading-${criteriaId}`} className="text-meta text-ink-muted">
          Loading identity…
        </p>
      ) : error || !data ? (
        <p data-testid={`agent-block-unknown-${criteriaId}`} className="text-meta text-ink-muted">
          No registered identity (the operator id may be a human id or not yet registered).
        </p>
      ) : (
        <dl className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-meta">
          <dt className="text-ink-faint">Name</dt>
          <dd className="text-ink">{data.name}</dd>
          <dt className="text-ink-faint">Status</dt>
          <dd className="text-ink">{data.status}</dd>
          <dt className="text-ink-faint">Labels</dt>
          <dd className="text-ink">
            {Object.entries(data.labels).map(([k, v]) => `${k}=${v}`).join(', ') || '—'}
          </dd>
          <dt className="text-ink-faint">Last seen</dt>
          <dd className="text-ink">{data.lastSeenAt ? new Date(data.lastSeenAt).toLocaleString() : '—'}</dd>
        </dl>
      )}
    </div>
  );
}

// --- Workflow catalog: trees + digests + interface viewer ---

export function CatalogPanel({ projectId }: { projectId: string }) {
  const { data, isLoading, error, refetch } = useListWorkflowCatalogQuery(projectId);
  const { data: doc } = useGetProjectConfigQuery(projectId);
  const [selected, setSelected] = useState('');
  const errorKind = error ? classifyError(error) : undefined;

  const record = doc?.record;
  const entry = selected ? (data?.entries.find((e) => e.name === selected) ?? null) : null;
  const entryInterface = selected ? record?.catalog?.[selected]?.interface : undefined;
  const routesUsing = (record?.routes ?? []).filter((r) => r.workflow === selected);

  return (
    <section aria-label="Workflow catalog" data-testid="project-catalog" className={SECTION_BORDER}>
      <h2 className="text-title font-semibold text-ink">Workflow catalog</h2>
      <p className="mb-3 text-meta text-ink-muted">
        The workflowLibrary entries the config record owns, resolved with the digests castle has learned. The server consults this catalog when validating assignments and routes.
      </p>
      {isLoading ? (
        <PageState loading title="Loading catalog…" testId="catalog-loading" />
      ) : error ? (
        <PageState
          error
          kind={errorKind}
          title="Failed to load the workflow catalog."
          onRetry={() => void refetch()}
          testId="catalog-error"
        />
      ) : !doc?.etag ? (
        <p data-testid="catalog-empty-unset" className="text-body text-ink-muted">
          The project has no workflow config record yet — create one in the Config tab and the catalog fills in from it.
        </p>
      ) : (data?.entries.length ?? 0) === 0 ? (
        <p data-testid="catalog-empty" className="text-body text-ink-muted">
          The config record is set but declares no workflows yet.
        </p>
      ) : (
        <>
          <table className="mb-3 w-full text-sm" data-testid="catalog-table">
            <StickyTableHead>
              <tr className="border-b border-line">
                <th className="py-2 pr-4 text-left font-medium">Name</th>
                <th className="py-2 pr-4 text-left font-medium">Type</th>
                <th className="py-2 pr-4 text-left font-medium">Source</th>
                <th className="py-2 pr-4 text-left font-medium">Class</th>
                <th className="py-2 text-right font-medium">Digest</th>
              </tr>
            </StickyTableHead>
            <tbody>
              {data?.entries.map((e) => (
                <tr key={e.name} data-testid={`catalog-row-${e.name}`} className="border-b border-line">
                  <td className="py-2 pr-4">
                    <button
                      type="button"
                      data-testid={`catalog-select-${e.name}`}
                      onClick={() => setSelected(e.name)}
                      className="font-mono text-accent underline-offset-2 hover:underline"
                    >
                      {e.name}
                    </button>
                  </td>
                  <td className="py-2 pr-4 text-ink-muted">{e.type}</td>
                  <td className="py-2 pr-4 font-mono text-ink-muted">
                    {e.type === 'image' ? e.image : `${e.url}@${e.ref}`}
                  </td>
                  <td className="py-2 pr-4 text-ink-muted">{e.class || 'dev'}</td>
                  <td className="py-2 text-right font-mono text-meta text-ink-faint">{e.digest || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {entry && (
            <div data-testid="catalog-interface-viewer" className="rounded-md border border-line bg-surface-raised p-3">
              <p className="mb-1 text-meta text-ink-muted">
                {selected} · interface {entry.interfaceName || '(none)'} · {routesUsing.length} route(s) reference it
              </p>
              {entryInterface ? (
                <pre className="overflow-x-auto text-meta text-ink">
                  {JSON.stringify({ ...entryInterface, configSlots: (entryInterface as { configSlots?: { slot: string; variable: string }[] }).configSlots?.map((s) => ({ ...s, variable: s.variable || KB310_CONFIG_SLOT_VARIABLES[s.slot] || s.slot })) }, null, 2)}
                </pre>
              ) : (
                <p className="text-meta text-ink-muted">This entry declares no interface (image workflow without KB-310 slots).</p>
              )}
            </div>
          )}
        </>
      )}
    </section>
  );
}

// --- The workflow draft: routes/config/binding editors share the doc draft ---

interface WorkflowDraft {
  /** The record being edited (draft ?? fetched); null while unset. */
  record: CfgRecord | null;
  etag: string;
  isLoading: boolean;
  error: string | undefined;
  serverError: string | null;
  setDraft: (r: CfgRecord | null) => void;
  save: () => void;
  saved: boolean;
  conflict: boolean;
  reload: () => void;
}

function useWorkflowDraft(projectId: string): WorkflowDraft {
  const { data, isLoading, error, refetch } = useGetProjectConfigQuery(projectId);
  const [setConfig, { error: saveError, reset: resetSave }] = useSetProjectConfigMutation();
  const [draft, setDraft] = useState<CfgRecord | null>(null);
  const [saved, setSaved] = useState(false);

  // Same draft-retirement rule the settings editor uses: the fetched etag
  // changed, so the local edits no longer describe the stored doc.
  useEffect(() => {
    setDraft(null);
  }, [data?.etag]);

  const record = draft ?? data?.record ?? null;
  const etag = data?.etag ?? '';
  const conflict = errorOf(saveError) === 'failed_precondition';
  const serverError = saveError !== undefined && !conflict ? errorOf(saveError) : null;

  async function save(): Promise<void> {
    if (record === null) return;
    setSaved(false);
    resetSave();
    try {
      await setConfig({ projectId, json: JSON.stringify(record), expectedEtag: etag }).unwrap();
      setSaved(true);
      setDraft(null);
    } catch {
      // Conflict / denial handled by the callers' banners.
    }
  }

  return {
    record,
    etag,
    isLoading,
    error: error === undefined ? undefined : classifyError(error),
    serverError,
    setDraft,
    save: () => void save(),
    saved,
    conflict,
    reload: () => {
      resetSave();
      setDraft(null);
      void refetch();
    },
  };
}

function RecordValidationPreview({ record, testId }: { record: CfgRecord | null; testId: string }) {
  const errors = useMemo(() => (record ? validateRecord(record) : []), [record]);
  if (errors.length === 0) return null;
  return (
    <div role="alert" data-testid={testId} className="mb-3 rounded-md border border-line bg-surface-raised p-2">
      <p className="mb-1 text-body text-danger">The server will reject this record as it stands:</p>
      <ul className="list-disc pl-5 text-meta text-danger">
        {errors.map((e, i) => (
          <li key={i}>{e}</li>
        ))}
      </ul>
      <p className="mt-1 text-meta text-ink-faint">Save stays available; castle validates the same rules server-side.</p>
    </div>
  );
}

const inputClass =
  'rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink placeholder:text-ink-faint';

// --- Routes editor: state/tag -> workflow ---

export function RoutesEditor({ projectId }: { projectId: string }) {
  const draft = useWorkflowDraft(projectId);
  const { resolved, isAdmin } = useConsoleCaller(projectId);
  const admin = isAdmin && resolved;
  const record = draft.record;

  const setRoute = (i: number, next: Partial<Route>) => {
    if (record === null) return;
    const routes = (record.routes ?? []).map((r, j) => (j === i ? { ...r, ...next } : r));
    draft.setDraft({ ...record, routes });
  };

  return (
    <section aria-label="Routes editor" data-testid="project-routes" className={SECTION_BORDER}>
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-title font-semibold text-ink">Routes</h2>
        <span data-testid="routes-etag" className="text-meta text-ink-faint" title="Content-addressed record etag">
          etag {draft.etag || '(unset)'}
        </span>
      </div>
      <p className="mb-3 text-meta text-ink-muted">
        state / tag → workflow assignments. Empty routes[] default to the record's payload route on the default state (Triage); an explicit empty states list is rejected.
      </p>

      {draft.isLoading ? (
        <PageState loading title="Loading config record…" testId="routes-loading" />
      ) : draft.error ? (
        <PageState error kind={draft.error as PageErrorKind} title="Failed to load the config record." testId="routes-error" />
      ) : record === null ? (
        <p data-testid="routes-empty-unset" className="text-body text-ink-muted">
          No config record yet — create one in the Config tab, then routes attach to its catalog.
        </p>
      ) : (
        <>
          {(record.routes ?? []).length === 0 && (
            <p data-testid="routes-empty" className="mb-2 text-body text-ink-muted">
              No explicit routes; dispatch falls back to the record's catalog workflows.
            </p>
          )}
          {(record.routes ?? []).map((rt, i) => {
            const configRefError = (rt.configRef ?? '') !== '' && !Object.keys(record.configLibrary ?? {}).includes(rt.configRef ?? '')
              ? `configRef "${rt.configRef}" is not in the record's configLibrary.`
              : null;
            return (
              <div key={i} data-testid={`routes-row-${i}`} className="mb-3 rounded-md border border-line bg-surface-raised p-3">
                <div className="mb-2 flex flex-wrap items-start gap-2">
                  <label className="flex flex-col text-meta text-ink-muted">
                    Name
                    <input
                      data-testid={`routes-name-${i}`}
                      aria-label={`Route name ${i + 1}`}
                      value={rt.name}
                      disabled={!admin}
                      onChange={(e) => setRoute(i, { name: e.target.value })}
                      className={`mt-1 ${inputClass}`}
                    />
                  </label>
                  <label className="flex flex-col text-meta text-ink-muted">
                    Workflow
                    <select
                      data-testid={`routes-workflow-${i}`}
                      aria-label={`Route workflow ${i + 1}`}
                      value={rt.workflow}
                      disabled={!admin}
                      onChange={(e) => setRoute(i, { workflow: e.target.value })}
                      className={`mt-1 ${inputClass}`}
                    >
                      <option value="">(none)</option>
                      {Object.keys(record.catalog ?? {}).map((name) => (
                        <option key={name} value={name}>
                          {name}
                        </option>
                      ))}
                    </select>
                  </label>
                  <label className="flex flex-col text-meta text-ink-muted">
                    States (comma list)
                    <input
                      data-testid={`routes-states-${i}`}
                      aria-label={`Route states ${i + 1}`}
                      value={(rt.states ?? []).join(', ')}
                      disabled={!admin}
                      onChange={(e) =>
                        setRoute(i, {
                          states: e.target.value.split(',').map((s) => s.trim()).filter((s) => s !== ''),
                        })
                      }
                      placeholder="Triage"
                      className={`mt-1 ${inputClass}`}
                    />
                  </label>
                  <label className="flex flex-col text-meta text-ink-muted">
                    Tags (comma list)
                    <input
                      data-testid={`routes-tags-${i}`}
                      aria-label={`Route tags ${i + 1}`}
                      value={(rt.tags ?? []).join(', ')}
                      disabled={!admin}
                      onChange={(e) =>
                        setRoute(i, { tags: e.target.value.split(',').map((s) => s.trim()).filter((s) => s !== '') })
                      }
                      className={`mt-1 ${inputClass}`}
                    />
                  </label>
                  <label className="flex flex-col text-meta text-ink-muted">
                    Config ref
                    <select
                      data-testid={`routes-configref-${i}`}
                      aria-label={`Route config ref ${i + 1}`}
                      value={rt.configRef ?? ''}
                      disabled={!admin}
                      onChange={(e) => setRoute(i, { configRef: e.target.value === '' ? undefined : e.target.value })}
                      className={`mt-1 ${inputClass}`}
                    >
                      <option value="">(none)</option>
                      {Object.keys(record.configLibrary ?? {}).map((n) => (
                        <option key={n} value={n}>
                          {n}
                        </option>
                      ))}
                    </select>
                  </label>
                  {admin && (
                    <button
                      type="button"
                      data-testid={`routes-remove-${i}`}
                      aria-label={`Remove route ${i + 1}`}
                      onClick={() => draft.setDraft({ ...record, routes: (record.routes ?? []).filter((_, j) => j !== i) })}
                      className="self-end rounded-md border border-line px-2 py-1 text-meta text-ink-muted hover:bg-surface hover:text-ink"
                    >
                      Remove
                    </button>
                  )}
                </div>
                {(configRefError || rt.workflow === '') && (
                  <p role="alert" data-testid={`routes-row-error-${i}`} className="text-meta text-danger">
                    {configRefError ?? 'Pick the workflow this route targets.'}
                  </p>
                )}
              </div>
            );
          })}
          {admin && (
            <>
              <button
                type="button"
                data-testid="routes-add"
                onClick={() => draft.setDraft({ ...record, routes: [...(record.routes ?? []), newRoute(record.registryName)] })}
                className="mr-2 rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface-raised"
              >
                Add route
              </button>
              <button
                type="button"
                data-testid="routes-save"
                onClick={() => draft.save()}
                className="rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong"
              >
                Save record
              </button>
            </>
          )}
        </>
      )}
      {record !== null && <RecordValidationPreview record={record} testId="routes-validation" />}
      {draft.saved && (
        <p role="status" data-testid="routes-saved" className="text-body text-success">
          Config record saved — the routes are live for the next dispatch.
        </p>
      )}
      {draft.conflict && (
        <div role="alert" data-testid="routes-conflict" className="mt-2 rounded-md border border-line bg-surface-raised p-2">
          <p className="text-body text-danger">The record changed while you were editing — nothing was clobbered.</p>
          <button type="button" data-testid="routes-reload" onClick={() => draft.reload()} className="mt-1 rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface">
            Reload current record
          </button>
        </div>
      )}
      {draft.serverError === 'permission_denied' && (
        <p role="alert" data-testid="routes-denied" className="mt-2 text-body text-danger">
          Castle denied the write: this session's operator lacks the admin role.
        </p>
      )}
      {draft.serverError !== null && draft.serverError !== 'permission_denied' && (
        <p role="alert" data-testid="routes-server-error" className="mt-2 text-body text-danger">
          Castle rejected the save ({draft.serverError}) — the record was not written; fix the record and retry.
        </p>
      )}
    </section>
  );
}

// --- Catalog editor: the workflowLibrary entries the record owns (---

/**
 * Workflow import/edit rides the config record: catalog entries are typed
 * url/image workflows; the interface editor takes the raw
 * criteria.brokenbots.dev/v1 WorkflowInterface JSON (parsed client-side,
 * shape-checked by the record mirror — castle re-validates on save).
 */
function CatalogSection({ draft, admin }: { draft: WorkflowDraft; admin: boolean }) {
  const record = draft.record;
  const [newName, setNewName] = useState('');
  const [newType, setNewType] = useState<string>(TYPE_URL);
  const [parseErrors, setParseErrors] = useState<Record<string, string>>( {});
  // Raw text per workflow name, held even mid-parse so no keystroke is
  // discarded: the draft only absorbs successfully parsed text. `installed`
  // marks the interface object WE last wrote; when the draft's interface
  // differs (reload/save/refetch) the textarea falls back to a display form.
  const [rawText, setRawText] = useState<Record<string, string>>({});
  const [installed, setInstalled] = useState<Record<string, Interface | undefined>>({});

  if (record === null) return null;

  const catalog = record.catalog ?? {};
  const setCatalog = (next: Record<string, Workflow>) => draft.setDraft({ ...record, catalog: next });
  const setEntryField = (name: string, next: Partial<Workflow>) =>
    setCatalog({ ...catalog, [name]: { ...catalog[name], ...next } });

  const setInterface = (name: string, raw: string) => {
    setRawText((prev) => ({ ...prev, [name]: raw }));
    if (raw.trim() === '') {
      const { interface: _drop, ...rest } = catalog[name];
      setParseErrors({ ...parseErrors, [name]: '' });
      setInstalled({ ...installed, [name]: undefined });
      setCatalog({ ...catalog, [name]: rest as Workflow });
      return;
    }
    try {
      const parsed = JSON.parse(raw) as Interface;
      setParseErrors({ ...parseErrors, [name]: '' });
      setInstalled({ ...installed, [name]: parsed });
      setCatalog({ ...catalog, [name]: { ...catalog[name], interface: parsed } });
    } catch {
      // Keep the draft as-is; the flag below pins the parse error until the
      // text parses, and the textarea holds the raw text meanwhile. Pin the
      // pre-edit interface as "ours" so the first invalid keystroke on an
      // externally-loaded value does not snap the textarea back to display.
      setInstalled((prev) => (prev[name] !== undefined ? prev : { ...prev, [name]: catalog[name]?.interface ?? undefined }));
      setParseErrors({ ...parseErrors, [name]: 'Interface is not valid JSON yet — castle would reject the record.' });
    }
  };

  return (
    <div className="mb-4">
      <h3 className="mb-2 text-heading font-medium text-ink">Workflow catalog</h3>
      {Object.keys(catalog).length === 0 && (
        <p data-testid="config-catalog-empty" className="mb-2 text-body text-ink-muted">
          No workflows yet — add the first one below to import it into the project record.
        </p>
      )}
      {Object.entries(catalog).map(([name, wf]) => (
        <div key={name} data-testid={'config-catalog-row-' + name} className="mb-3 rounded-md border border-line bg-surface-raised p-3">
          <div className="flex flex-wrap items-center gap-2 text-meta text-ink-muted">
            <span className="font-mono text-body text-ink">{name}</span>
            {admin && (
              <button type="button" data-testid={'config-catalog-remove-' + name} onClick={() => {
                const next = { ...catalog };
                delete next[name];
                setCatalog(next);
              }} className="rounded-md border border-line px-2 py-0.5 text-meta text-ink-muted hover:bg-surface hover:text-ink">
                Remove
              </button>
            )}
          </div>
          <div className="mt-2 grid gap-2 sm:grid-cols-4">
            <label className="flex flex-col text-meta text-ink-muted">
              type
              <select
                data-testid={'config-catalog-' + name + '-type'}
                value={wf.type}
                disabled={!admin}
                onChange={(e) => setEntryField(name, { type: e.target.value })}
                className={'mt-1 ' + inputClass}
              >
                <option value="url">url</option>
                <option value="image">image</option>
              </select>
            </label>
            <label className="flex flex-col text-meta text-ink-muted">
              url
              <input
                data-testid={'config-catalog-' + name + '-url'}
                value={wf.url ?? ''}
                disabled={!admin}
                onChange={(e) => setEntryField(name, { url: e.target.value === '' ? undefined : e.target.value })}
                className={'mt-1 ' + inputClass}
              />
            </label>
            <label className="flex flex-col text-meta text-ink-muted">
              ref
              <input
                data-testid={'config-catalog-' + name + '-ref'}
                value={wf.ref ?? ''}
                disabled={!admin}
                onChange={(e) => setEntryField(name, { ref: e.target.value === '' ? undefined : e.target.value })}
                className={'mt-1 ' + inputClass}
              />
            </label>
            <label className="flex flex-col text-meta text-ink-muted">
              image
              <input
                data-testid={'config-catalog-' + name + '-image'}
                value={wf.image ?? ''}
                disabled={!admin}
                onChange={(e) => setEntryField(name, { image: e.target.value === '' ? undefined : e.target.value })}
                className={'mt-1 ' + inputClass}
              />
            </label>
          </div>
          {admin && (
            <label className="mt-2 flex flex-col text-meta text-ink-muted">
              interface (JSON; KB-310 slots)
              <textarea
                data-testid={'config-catalog-' + name + '-interface'}
                rows={3}
                value={wf.interface === installed[name] ? (rawText[name] ?? '') : JSON.stringify(wf.interface)}
                onChange={(e) => setInterface(name, e.target.value)}
                className={'mt-1 font-mono ' + inputClass}
              />
            </label>
          )}
          {wf.interface === installed[name] && (parseErrors[name] ?? '') !== '' && (
            <p role="alert" data-testid={'config-catalog-' + name + '-parse-error'} className="mt-1 text-meta text-danger">
              {parseErrors[name]}
            </p>
          )}
        </div>
      ))}
      {admin && (
        <form
          data-testid="config-catalog-add"
          className="flex flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            const name = newName.trim();
            if (!name || catalog[name]) return;
            const seed: Workflow = newType === TYPE_IMAGE
              ? { type: TYPE_IMAGE, image: '' }
              : { type: TYPE_URL, url: '' };
            setCatalog({ ...catalog, [name]: seed });
            setNewName('');
          }}
        >
          <input
            data-testid="config-catalog-add-name"
            aria-label="New workflow name"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            placeholder="workflow name e.g. ship-it"
            className={inputClass + ' w-56'}
          />
          <select
            data-testid="config-catalog-add-type"
            aria-label="New workflow type"
            value={newType}
            onChange={(e) => setNewType(e.target.value)}
            className={inputClass}
          >
            <option value="url">url</option>
            <option value="image">image</option>
          </select>
          <button
            type="submit"
            data-testid="config-catalog-add-submit"
            disabled={!!newName.trim() && catalog[newName.trim()] !== undefined}
            className="rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface-raised disabled:opacity-60"
          >
            Add workflow
          </button>
        </form>
      )}
    </div>
  );
}

// --- Config editor: configLibrary entries + slot defaults + payload preview ---

const CONFIG_ENTRY_FIELDS = [
  'buildCmd',
  'testCmd',
  'ciGateCmd',
  'baseBranch',
] as const;

export function ConfigEditor({ projectId }: { projectId: string }) {
  const draft = useWorkflowDraft(projectId);
  const { resolved, isAdmin } = useConsoleCaller(projectId);
  const admin = isAdmin && resolved;
  const record = draft.record;
  const [newName, setNewName] = useState('');

  const entries = Object.entries(record?.configLibrary ?? {});

  const setEntry = (name: string, next: Partial<ConfigEntry>) => {
    if (record === null) return;
    draft.setDraft({ ...record, configLibrary: { ...(record.configLibrary ?? {}), [name]: next as ConfigEntry } });
  };

  return (
    <section aria-label="Config editor" data-testid="project-workflow-config" className={SECTION_BORDER}>
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-title font-semibold text-ink">Config</h2>
        <span data-testid="config-etag" className="text-meta text-ink-faint" title="Content-addressed record etag">
          etag {draft.etag || '(unset)'}
        </span>
      </div>
      <p className="mb-3 text-meta text-ink-muted">
        The record's configLibrary entries — rendered by the KB-103 pipeline into the namespace ConfigMaps — plus the slot mapping they deliver through (KB-310).
      </p>

      {draft.isLoading ? (
        <PageState loading title="Loading config record…" testId="config-loading" />
      ) : draft.error ? (
        <PageState error kind={draft.error as PageErrorKind} title="Failed to load the config record." testId="config-error" />
      ) : record === null ? (
        <>
          {admin && (
            <button
              type="button"
              data-testid="config-bootstrap"
              onClick={() => { draft.setDraft(emptyRecord(projectId)); }}
              className="mb-3 rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong"
            >
              Start a config record
            </button>
          )}
          <p data-testid="config-empty-unset" className="text-body text-ink-muted">
            No config record yet — the first save creates it.
          </p>
        </>
      ) : (
        <>
          {entries.length === 0 && (
            <p data-testid="config-empty" className="mb-2 text-body text-ink-muted">
              No configLibrary entries yet; add one with the KB-310 variable map below.
            </p>
          )}
          <div className="mb-3 rounded-md border border-line bg-surface-raised p-3">
            <p className="mb-2 text-meta font-medium text-ink">Record header</p>
            <div className="grid gap-2 sm:grid-cols-2">
              <label className="flex flex-col text-meta text-ink-muted">
                registryName
                <input
                  data-testid="config-registryname"
                  value={record.registryName}
                  disabled={!admin}
                  onChange={(e) => { if (record !== null) draft.setDraft({ ...record, registryName: e.target.value }); }}
                  className={'mt-1 ' + inputClass}
                />
              </label>
              <label className="flex flex-col text-meta text-ink-muted">
                namespace
                <input
                  data-testid="config-namespace"
                  value={record.namespace ?? ''}
                  disabled={!admin}
                  onChange={(e) => { if (record !== null) draft.setDraft({ ...record, namespace: e.target.value }); }}
                  className={'mt-1 ' + inputClass}
                />
              </label>
            </div>
          </div>
          <CatalogSection draft={draft} admin={admin} />
          {entries.map(([name, entry]) => {
            const shaped = entry as ConfigEntry;
            const shapeErrors = validateConfigEntryShape(name, shaped);
            return (
              <div key={name} data-testid={`config-entry-${name}`} className="mb-3 rounded-md border border-line bg-surface-raised p-3">
                <p className="mb-2 font-mono text-body text-ink">{name}</p>
                <div className="grid gap-2 sm:grid-cols-2">
                  {CONFIG_ENTRY_FIELDS.map((f) => {
                    const slotVar = KB310_CONFIG_SLOT_VARIABLES[f];
                    return (
                      <label key={f} className="flex flex-col text-meta text-ink-muted">
                        {f}
                        {slotVar && <span className="font-mono text-ink-faint">→ {slotVar}</span>}
                        <input
                          data-testid={`config-${name}-${f}`}
                          aria-label={`${name} ${f}`}
                          value={shaped[f] ?? ''}
                          disabled={!admin}
                          onChange={(e) => setEntry(name, { ...shaped, [f]: e.target.value === '' ? undefined : e.target.value })}
                          placeholder={f === 'baseBranch' ? 'e.g. main' : 'e.g. make build'}
                          className={`mt-1 ${inputClass}`}
                        />
                      </label>
                    );
                  })}
                  <label className="flex flex-col text-meta text-ink-muted">
                    tags (comma list)
                    <input
                      data-testid={`config-${name}-tags`}
                      aria-label={`${name} tags`}
                      value={(shaped.tags ?? []).join(', ')}
                      disabled={!admin}
                      onChange={(e) =>
                        setEntry(name, {
                          ...shaped,
                          tags: e.target.value.split(',').map((s) => s.trim()).filter((s) => s !== ''),
                        })
                      }
                      className={`mt-1 ${inputClass}`}
                    />
                  </label>
                  <label className="flex flex-col text-meta text-ink-muted">
                    tagMatch
                    <select
                      data-testid={`config-${name}-tagMatch`}
                      aria-label={`${name} tagMatch`}
                      value={shaped.tagMatch ?? ''}
                      disabled={!admin}
                      onChange={(e) => setEntry(name, { ...shaped, tagMatch: e.target.value === '' ? undefined : e.target.value })}
                      className={`mt-1 ${inputClass}`}
                    >
                      <option value="">(absent)</option>
                      <option value="all">all</option>
                      <option value="any">any</option>
                    </select>
                  </label>
                </div>
                {admin && (
                  <button
                    type="button"
                    data-testid={`config-remove-${name}`}
                    aria-label={`Remove config entry ${name}`}
                    onClick={() => {
                      if (record === null) return;
                      const next = { ...record.configLibrary };
                      delete next[name];
                      draft.setDraft({ ...record, configLibrary: next });
                    }}
                    className="mt-2 rounded-md border border-line px-2 py-1 text-meta text-ink-muted hover:bg-surface hover:text-ink"
                  >
                    Remove
                  </button>
                )}
                {shapeErrors.length > 0 && (
                  <ul role="alert" data-testid={`config-entry-errors-${name}`} className="mt-2 list-disc pl-5 text-meta text-danger">
                    {shapeErrors.map((e, i) => (
                      <li key={i}>{e}</li>
                    ))}
                  </ul>
                )}
              </div>
            );
          })}
          {admin && (
            <form
              data-testid="config-add"
              className="flex flex-wrap items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (record === null || !newName.trim()) return;
                draft.setDraft({
                  ...record,
                  configLibrary: { ...(record.configLibrary ?? {}), [newName.trim()]: {} },
                });
                setNewName('');
              }}
            >
              <input
                data-testid="config-add-name"
                aria-label="New config entry name"
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="configLibrary name e.g. ci"
                className={`${inputClass} w-64`}
              />
              <button
                type="submit"
                data-testid="config-add-submit"
                className="rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface-raised disabled:opacity-60"
                disabled={!!newName && entries.some(([n]) => n === newName.trim())}
              >
                Add config entry
              </button>
            </form>
          )}
          {admin && (
            <button
              type="button"
              data-testid="config-save"
              onClick={() => draft.save()}
              className="mt-3 rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong"
            >
              Save record
            </button>
          )}
        </>
      )}
      {record !== null && (
        <pre data-testid="config-record-preview" className="mt-3 overflow-x-auto rounded-md border border-line bg-surface-raised p-2 text-meta text-ink">
          {JSON.stringify(record, null, 2)}
        </pre>
      )}
      {record !== null && <RecordValidationPreview record={record} testId="config-validation" />}
      {draft.saved && (
        <p role="status" data-testid="config-saved" className="text-body text-success">
          Config record saved — the namespace ConfigMaps re-render from it.
        </p>
      )}
      {draft.conflict && (
        <div role="alert" data-testid="config-conflict" className="mt-2 rounded-md border border-line bg-surface-raised p-2">
          <p className="text-body text-danger">The record changed while you were editing — nothing was clobbered.</p>
          <button type="button" data-testid="config-reload" onClick={() => draft.reload()} className="mt-1 rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface">
            Reload current record
          </button>
        </div>
      )}
      {draft.serverError === 'permission_denied' && (
        <p role="alert" data-testid="config-denied" className="mt-2 text-body text-danger">
          Castle denied the write: this session's operator lacks the admin role.
        </p>
      )}
      {draft.serverError !== null && draft.serverError !== 'permission_denied' && (
        <p role="alert" data-testid="config-server-error" className="mt-2 text-body text-danger">
          Castle rejected the save ({draft.serverError}) — the record was not written; fix the record and retry.
        </p>
      )}
    </section>
  );
}

// --- Binding: the namespace binding payload, read-mostly ---

export function BindingPanel({ projectId }: { projectId: string }) {
  const { data: doc, isLoading, error, refetch } = useGetProjectConfigQuery(projectId);
  const errorKind = error ? classifyError(error) : undefined;
  const binding = doc?.record?.binding;
  const shaped = binding ?? null;
  const errors = shaped ? bindingErrors(shaped) : [];

  return (
    <section aria-label="Binding" data-testid="project-binding" className={SECTION_BORDER}>
      <h2 className="text-title font-semibold text-ink">Binding</h2>
      <p className="mb-3 text-meta text-ink-muted">
        The namespace binding payload rides the record; the binding tab is read-mostly — the server validates its shape on every record save, so edit via the config record instead of a dedicated write here.
      </p>
      {isLoading ? (
        <PageState loading title="Loading binding…" testId="binding-loading" />
      ) : error ? (
        <PageState error kind={errorKind} title="Failed to load the binding." onRetry={() => void refetch()} testId="binding-error" />
      ) : !doc?.etag ? (
        <p data-testid="binding-empty-unset" className="text-body text-ink-muted">
          No binding payload — the config record is unset.
        </p>
      ) : !shaped ? (
        <p data-testid="binding-empty" className="text-body text-ink-muted">
          The record declares no binding; the payload defaults hold.
        </p>
      ) : (
        <>
          {errors.length > 0 && (
            <ul role="alert" data-testid="binding-errors" className="mb-2 list-disc pl-5 text-meta text-danger">
              {errors.map((e, i) => (
                <li key={i}>{e}</li>
              ))}
            </ul>
          )}
          {[
            ['images', shaped.images ?? {}],
            ['env', shaped.env ?? {}],
            ['volumes', shaped.volumes ?? []],
            ['secrets', shaped.secrets ?? []],
            ['config', shaped.config ?? {}],
          ].map(([title, body]) => (
            <div key={title as string} data-testid={`binding-block-${title as string}`} className="mb-2 rounded-md border border-line bg-surface-raised p-3 text-sm">
              <p className="mb-1 font-medium text-ink">{title as string}</p>
              <pre className="overflow-x-auto text-meta text-ink">
                {JSON.stringify(body, null, 2)}
              </pre>
            </div>
          ))}
        </>
      )}
    </section>
  );
}