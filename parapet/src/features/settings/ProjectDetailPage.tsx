import { useEffect, useMemo, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { PageHeader, PageState, StickyTableHead, classifyError } from '@castle/run-viewer';
import {
  useGetProjectQuery,
  useGetProjectSettingsQuery,
  useSetProjectSettingsMutation,
  useGetRefirePolicyQuery,
  useSetRefirePolicyMutation,
  useListProjectMembersQuery,
  useAddProjectMemberMutation,
  useRemoveProjectMemberMutation,
  type RefirePolicyDoc,
  type RefirePolicyArgs,
} from './settingsApi';
import {
  ALLOW_ANON_READS_KEY,
  parseVocabulary,
  validateMemberID,
  validateProjectID,
  validateRefirePolicy,
  validateRefireRoute,
  validateSettingsDoc,
} from './validation';

// Settings console project detail (KB-327): the per-project editors. The
// settings editor performs the etag swap (SetProjectSettings carries the
// etag the user last read; a failed_precondition means someone else changed
// the doc first and the user reloads instead of clobbering). The refire
// editor is per-route get/set with the project default row at route ''. The
// members editor is the only console path that admits members into CLOSED
// projects. All writes ride the console session castle already authorized.

interface Entry {
  key: string;
  value: string;
}

export function ProjectDetailPage() {
  const params = useParams();
  const projectId = decodeURIComponent(params.projectId ?? '');

  const { data, isLoading, error, refetch } = useGetProjectQuery(projectId, {
    skip: projectId === '',
  });
  const errorKind = error ? classifyError(error) : undefined;

  if (projectId === '' || validateProjectID(projectId)) {
    return <PageState error kind="not_found" testId="project-detail-invalid" detail="Project slugs are lowercase (a-z, 0-9, -, _, .)." />;
  }
  if (isLoading) {
    return <PageState loading title={`Loading project ${projectId}…`} testId="project-detail-loading" />;
  }
  if (error || !data) {
    return errorKind === 'unauthenticated' ? (
      <PageState error kind="unauthenticated" />
    ) : (
      <PageState
        error
        kind={errorKind}
        title={`Failed to load project ${projectId}.`}
        detail="Castle is unreachable or failed to answer. Try again."
        onRetry={() => void refetch()}
        testId="project-detail-error"
      />
    );
  }

  return (
    <div data-testid="project-detail">
      <PageHeader
        title={data.id}
        actions={
          <Link to="/settings/projects" className="text-body text-ink-muted hover:text-ink">
            ← All projects
          </Link>
        }
      />
      <dl data-testid="project-overview" className="mb-6 grid grid-cols-2 gap-x-6 gap-y-1 rounded-lg border border-line bg-surface p-3 text-sm">
        <dt className="text-ink-muted">Membership</dt>
        <dd className="text-ink">{data.membershipPolicy}</dd>
        <dt className="text-ink-muted">Default refire policy</dt>
        <dd className="text-ink">{data.defaultRefirePolicy || '—'}</dd>
        <dt className="text-ink-muted">Queues</dt>
        <dd className="text-ink">{data.queues.join(', ') || '—'}</dd>
        <dt className="text-ink-muted">Created</dt>
        <dd className="text-ink">{data.createdAt ? new Date(data.createdAt).toLocaleString() : '—'}</dd>
      </dl>

      <div className="flex flex-col gap-6">
        <SettingsEditor projectId={projectId} />
        <RefireEditor projectId={projectId} />
        <MembersCard projectId={projectId} membershipPolicy={data.membershipPolicy} />
      </div>
    </div>
  );
}

function SettingsEditor({ projectId }: { projectId: string }) {
  const { data, isLoading, error, refetch } = useGetProjectSettingsQuery(projectId);
  const settingsErrorKind = error ? classifyError(error) : undefined;
  const [setSettings, { isLoading: saving, error: saveError, reset: resetSave }] =
    useSetProjectSettingsMutation();

  const [draft, setDraft] = useState<Entry[] | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  const entries: Entry[] = useMemo(
    () =>
      draft ??
      Object.entries(data?.settings ?? {})
        // Display order: sorted keys so the form is deterministic.
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([key, value]) => ({ key, value })),
    [draft, data],
  );

  // A fetched etag change (external edit or reload) retires the local draft
  // so the form always shows the doc the current etag hash describes. The
  // save status message is owned by the save lifecycle (set at success,
  // cleared when the next save starts) so it survives the invalidating
  // refetch that follows a successful save.
  useEffect(() => {
    setDraft(null);
  }, [data?.etag]);

  const validation = useMemo(() => validateSettingsDoc(entries), [entries]);
  const fieldError = (i: number): string | null =>
    validation.keys[i] ?? validation.values[i] ?? null;
  const invalid =
    validation.doc !== null || entries.some((_, i) => fieldError(i) !== null);

  const conflicted = errorOf(saveError) === 'failed_precondition';
  const denied = errorOf(saveError) === 'permission_denied';

  async function save() {
    if (invalid) return;
    setSaved(null);
    resetSave();
    const record: Record<string, string> = {};
    for (const e of entries) {
      if (e.key !== '' || e.value !== '') record[e.key] = e.value;
    }
    try {
      await setSettings({ projectId, settings: record, expectedEtag: data?.etag ?? '' }).unwrap();
      setSaved('Settings saved.');
    } catch {
      // Conflict/denied handled below; refetch offers recovery.
    }
  }

  return (
    <section aria-label="Project settings" data-testid="project-settings" className="rounded-lg border border-line bg-surface p-4">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-title font-semibold text-ink">Settings</h2>
        <span data-testid="project-settings-etag" className="text-meta text-ink-faint" title="Content-addressed doc etag">
          etag {data?.etag || '(unset)'}
        </span>
      </div>
      <p className="mb-3 text-meta text-ink-muted">
        Convention-keyed JSON entries swapped whole. An empty value means the entry is absent.
        <code className="mx-1 rounded bg-surface-raised px-1">{ALLOW_ANON_READS_KEY}</code>
        must be a JSON boolean.
      </p>

      {isLoading ? (
        <PageState loading title="Loading settings…" testId="project-settings-loading" />
      ) : error ? (
        settingsErrorKind === 'unauthenticated' ? (
          <PageState error kind="unauthenticated" />
        ) : (
          <PageState
            error
            kind={settingsErrorKind}
            title="Failed to load settings."
            onRetry={() => void refetch()}
            testId="project-settings-error"
          />
        )
      ) : (
        <>
          {entries.map((entry, i) => (
            <div key={i} data-testid="project-setting-row" className="mb-2 flex flex-wrap items-start gap-2">
              <input
                data-testid={`project-setting-key-${i}`}
                aria-label={`Setting key ${i + 1}`}
                value={entry.key}
                onChange={(e) => updateEntry(i, { ...entry, key: e.target.value })}
                placeholder="key"
                className="w-52 rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink placeholder:text-ink-faint"
              />
              {entry.key === ALLOW_ANON_READS_KEY ? (
                <select
                  data-testid={`project-setting-value-${i}`}
                  aria-label={`Setting value ${i + 1}`}
                  value={entry.value}
                  onChange={(e) => updateEntry(i, { ...entry, value: e.target.value })}
                  className="rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink"
                >
                  <option value="">(absent)</option>
                  <option value="true">true</option>
                  <option value="false">false</option>
                </select>
              ) : (
                <textarea
                  data-testid={`project-setting-value-${i}`}
                  aria-label={`Setting value ${i + 1}`}
                  value={entry.value}
                  onChange={(e) => updateEntry(i, { ...entry, value: e.target.value })}
                  placeholder='JSON value e.g. {"retries": 3}'
                  rows={1}
                  className="min-w-64 flex-1 rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink placeholder:text-ink-faint"
                />
              )}
              {fieldError(i) && (
                <p role="alert" className="w-full text-meta text-danger" data-testid={`project-setting-error-${i}`}>
                  {fieldError(i)}
                </p>
              )}
              <button
                type="button"
                data-testid={`project-setting-remove-row-${i}`}
                aria-label={`Remove setting row ${i + 1}`}
                onClick={() => setDraft(entries.filter((_, j) => j !== i))}
                className="rounded-md border border-line px-2 py-1 text-meta text-ink-muted hover:bg-surface-raised hover:text-ink"
              >
                Remove
              </button>
            </div>
          ))}
          <button
            type="button"
            data-testid="project-setting-add-row"
            onClick={() => setDraft([...entries, { key: '', value: '' }])}
            className="mr-2 rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface-raised"
          >
            Add entry
          </button>
          <button
            type="button"
            data-testid="project-settings-save"
            onClick={() => void save()}
            disabled={saving || invalid}
            className="rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong disabled:opacity-60"
          >
            {saving ? 'Saving…' : 'Save settings'}
          </button>
          {validation.doc && (
            <p role="alert" data-testid="project-settings-doc-error" className="mt-2 text-body text-danger">
              {validation.doc}
            </p>
          )}
          {saved && (
            <p role="status" data-testid="project-settings-saved" className="mt-2 text-body text-success">
              {saved}
            </p>
          )}
          {conflicted && (
            <div role="alert" data-testid="project-settings-conflict" className="mt-2 rounded-md border border-line bg-surface-raised p-2">
              <p className="text-body text-danger">
                The settings changed while you were editing — your swap was rejected and nothing was clobbered.
              </p>
              <button
                type="button"
                data-testid="project-settings-reload"
                onClick={() => {
                  resetSave();
                  void refetch();
                }}
                className="mt-1 rounded-md border border-line px-2 py-1 text-body text-ink hover:bg-surface"
              >
                Reload current doc
              </button>
            </div>
          )}
          {denied && (
            <p role="alert" data-testid="project-settings-denied" className="mt-2 text-body text-danger">
              Castle denied the write: this console session lacks project admin credentials.
            </p>
          )}
        </>
      )}
    </section>
  );

  function updateEntry(i: number, next: Entry) {
    setDraft(entries.map((entry, j) => (j === i ? next : entry)));
  }
}

// RefirePolicyEditor — per-(project, route) policy get/set. The exact key is
// what castle serves: '' is the project default row; any other route is an
// opaque key settled by the engine's resolution ladder.
function RefireEditor({ projectId }: { projectId: string }) {
  const [custom, setCustom] = useState(false);
  const [route, setRoute] = useState('');

  const args: RefirePolicyArgs = useMemo(
    () => ({ projectId, route: custom ? route : '' }),
    [projectId, route, custom],
  );
  const { data, isLoading, error, refetch } = useGetRefirePolicyQuery(args);
  const refireErrorKind = error ? classifyError(error) : undefined;
  const [setPolicy, { isLoading: saving, error: saveError }] = useSetRefirePolicyMutation();
  const [draft, setDraft] = useState<RefirePolicyDoc | null>(null);
  const [saved, setSaved] = useState(false);

  const unset = errorOf(error) === 'not_found';
  const policy = draft ?? data?.policy ?? EMPTY_POLICY;

  useEffect(() => {
    setDraft(null);
    setSaved(false);
  }, [data?.updatedAt, args.route, args.projectId]);

  const policyError = validateRefirePolicy(policy);
  const routeError = custom ? validateRefireRoute(route) : null;

  async function save() {
    if (policyError || routeError) return;
    try {
      await setPolicy({ ...args, policy }).unwrap();
      setSaved(true);
    } catch {
      // Inline saveError rendering below.
    }
  }

  return (
    <section aria-label="Refire policy" data-testid="project-refire" className="rounded-lg border border-line bg-surface p-4">
      <h2 className="mb-2 text-title font-semibold text-ink">Refire policy</h2>
      <p className="mb-3 text-meta text-ink-muted">
        Per-route budgets and classification vocabularies. The project default row is route{' '}
        <code className="rounded bg-surface-raised px-1">''</code>; specific routes resolve by exact key.
      </p>

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <label className="text-meta text-ink-muted">
          Route
          <select
            data-testid="refire-route-select"
            aria-label="Refire route selection"
            value={custom ? 'custom' : ''}
            onChange={(e) => setCustom(e.target.value === 'custom')}
            className="ml-2 rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink"
          >
            <option value="">project default</option>
            <option value="custom">custom route…</option>
          </select>
        </label>
        {custom && (
          <input
            data-testid="refire-route-input"
            aria-label="Custom refire route"
            value={route}
            onChange={(e) => setRoute(e.target.value)}
            placeholder="route key"
            className="w-56 rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink placeholder:text-ink-faint"
          />
        )}
        <span data-testid="refire-state" className="text-meta text-ink-faint">
          {isLoading ? 'loading…' : unset ? 'not set — saving creates it' : `updated ${data?.updatedAt ? new Date(data.updatedAt).toLocaleString() : ''}`}
        </span>
        {routeError && (
          <p role="alert" data-testid="refire-route-error" className="text-body text-danger">
            {routeError}
          </p>
        )}
      </div>

      {isLoading ? (
        <PageState loading title="Loading policy…" testId="refire-loading" />
      ) : error && !unset ? (
        refireErrorKind === 'unauthenticated' ? (
          <PageState error kind="unauthenticated" />
        ) : (
          <PageState
            error
            kind={refireErrorKind}
            title="Failed to load the refire policy."
            onRetry={() => void refetch()}
            testId="refire-error"
          />
        )
      ) : (
        <>
          <div className="mb-3 flex flex-wrap items-center gap-3" data-testid="refire-budgets">
            <label className="text-meta text-ink-muted">
              Max attempts
              <input
                data-testid="refire-max-attempts"
                aria-label="Max attempts"
                type="number"
                min={0}
                step={1}
                value={policy.maxAttempts}
                onChange={(e) => setDraft({ ...policy, maxAttempts: num(e.target.value) })}
                className="ml-2 w-24 rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink"
              />
            </label>
            <label className="text-meta text-ink-muted">
              Backoff seconds
              <input
                data-testid="refire-backoff-seconds"
                aria-label="Backoff seconds"
                type="number"
                min={0}
                step={1}
                value={policy.backoffSeconds}
                onChange={(e) => setDraft({ ...policy, backoffSeconds: num(e.target.value) })}
                className="ml-2 w-24 rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink"
              />
            </label>
          </div>
          <div className="mb-3 grid gap-3 sm:grid-cols-3" data-testid="refire-vocabularies">
            <label className="text-meta text-ink-muted">
              transient (one substring per line)
              <textarea
                data-testid="refire-transient"
                aria-label="Transient vocabulary"
                rows={3}
                defaultValue={policy.transient.join('\n')}
                key={`transient-${args.route}-${data?.updatedAt ?? 'unset'}`}
                onChange={(e) => setDraft({ ...policy, transient: parseVocabulary(e.target.value) })}
                className="mt-1 w-full rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink"
              />
            </label>
            <label className="text-meta text-ink-muted">
              parks (designed park spellings)
              <textarea
                data-testid="refire-parks"
                aria-label="Parks vocabulary"
                rows={3}
                defaultValue={policy.parks.join('\n')}
                key={`parks-${args.route}-${data?.updatedAt ?? 'unset'}`}
                onChange={(e) => setDraft({ ...policy, parks: parseVocabulary(e.target.value) })}
                className="mt-1 w-full rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink"
              />
            </label>
            <label className="text-meta text-ink-muted">
              cosmetics (benign endings)
              <textarea
                data-testid="refire-cosmetics"
                aria-label="Cosmetics vocabulary"
                rows={3}
                defaultValue={policy.cosmetics.join('\n')}
                key={`cosmetics-${args.route}-${data?.updatedAt ?? 'unset'}`}
                onChange={(e) => setDraft({ ...policy, cosmetics: parseVocabulary(e.target.value) })}
                className="mt-1 w-full rounded-md border border-line bg-surface-raised px-2 py-1.5 font-mono text-body text-ink"
              />
            </label>
          </div>
          {policyError && (
            <p role="alert" data-testid="refire-policy-error" className="mb-2 text-body text-danger">
              {policyError}
            </p>
          )}
          {errorOf(saveError) === 'permission_denied' && (
            <p role="alert" data-testid="refire-denied" className="mb-2 text-body text-danger">
              Castle denied the write: this console session lacks project admin credentials.
            </p>
          )}
          <button
            type="button"
            data-testid="refire-save"
            onClick={() => void save()}
            disabled={saving || policyError !== null || routeError !== null}
            className="rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong disabled:opacity-60"
          >
            {saving ? 'Saving…' : 'Save refire policy'}
          </button>
          {saved && (
            <p role="status" data-testid="refire-saved" className="ml-2 inline text-body text-success">
              Refire policy saved.
            </p>
          )}
        </>
      )}
    </section>
  );
}

function MembersCard({ projectId, membershipPolicy }: { projectId: string; membershipPolicy: 'open' | 'closed' }) {
  const { data, isLoading, error, refetch } = useListProjectMembersQuery(projectId);
  const membersErrorKind = error ? classifyError(error) : undefined;
  const [addMember, { isLoading: adding }] = useAddProjectMemberMutation();
  const [removeMember] = useRemoveProjectMemberMutation();
  const [memberId, setMemberId] = useState('');
  const [kind, setKind] = useState<'ai' | 'human'>('ai');
  const [formError, setFormError] = useState<string | null>(null);

  const members = useMemo(() => data ?? [], [data]);

  async function add() {
    setFormError(null);
    const validation = validateMemberID(memberId);
    if (validation) return setFormError(validation);
    try {
      await addMember({ projectId, memberId, kind }).unwrap();
      setMemberId('');
    } catch (err) {
      const { status, data: message } = (err ?? {}) as { status?: unknown; data?: unknown };
      if (status === 'already_exists') setFormError(`Member already admitted. ${String(message ?? '')}`.trim());
      else if (status === 'permission_denied') setFormError('Castle denied the write: this console session lacks project admin credentials.');
      else setFormError(String(message ?? 'Add failed. Try again.'));
    }
  }

  async function remove(id: string) {
    try {
      await removeMember({ projectId, memberId: id }).unwrap();
    } catch {
      // Refresh surfaces the truth: removal is idempotent server-side.
      void refetch();
    }
  }

  return (
    <section aria-label="Project members" data-testid="project-members" className="rounded-lg border border-line bg-surface p-4">
      <h2 className="mb-2 text-title font-semibold text-ink">Members</h2>
      {membershipPolicy === 'closed' && (
        <p data-testid="member-closed-hint" className="mb-2 text-meta text-ink-muted">
          This project is CLOSED: registration self-joins are rejected, and adding members here is the only console path in.
        </p>
      )}

      {isLoading ? (
        <PageState loading title="Loading members…" testId="member-list-loading" />
      ) : error ? (
        membersErrorKind === 'unauthenticated' ? (
          <PageState error kind="unauthenticated" />
        ) : (
          <PageState
            error
            kind={membersErrorKind}
            title="Failed to load members."
            onRetry={() => void refetch()}
            testId="member-list-error"
          />
        )
      ) : (
        <table className="mb-3 w-full text-sm" data-testid="member-table">
          <StickyTableHead>
            <tr className="border-b border-line">
              <th className="py-2 pr-4 text-left font-medium">Member</th>
              <th className="py-2 pr-4 text-left font-medium">Kind</th>
              <th className="py-2 text-right font-medium">Actions</th>
            </tr>
          </StickyTableHead>
          <tbody>
            {members.length === 0 ? (
              <tr data-testid="member-table-empty">
                <td colSpan={3} className="py-2 text-ink-muted">
                  No members yet.
                </td>
              </tr>
            ) : (
              members.map((m) => (
                <tr key={m.memberId} data-testid="member-row" className="border-b border-line">
                  <td className="py-2 pr-4 text-ink">{m.memberId}</td>
                  <td className="py-2 pr-4 text-ink-muted">{m.kind}</td>
                  <td className="py-2 text-right">
                    <button
                      type="button"
                      data-testid={`member-remove-${m.memberId}`}
                      aria-label={`Remove member ${m.memberId}`}
                      onClick={() => void remove(m.memberId)}
                      className="rounded-md border border-line px-2 py-1 text-meta text-ink-muted hover:bg-surface-raised hover:text-ink"
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      )}

      <form
        data-testid="member-add"
        aria-label="Add member"
        className="flex flex-wrap items-start gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          void add();
        }}
      >
        <label className="flex flex-col text-meta text-ink-muted">
          Member id
          <input
            data-testid="member-add-id"
            aria-label="Member id"
            value={memberId}
            onChange={(e) => setMemberId(e.target.value)}
            placeholder="agent or human operator id"
            className="mt-1 w-56 rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink placeholder:text-ink-faint"
          />
        </label>
        <label className="flex flex-col text-meta text-ink-muted">
          Kind
          <select
            data-testid="member-add-kind"
            aria-label="Member kind"
            value={kind}
            onChange={(e) => setKind(e.target.value as 'ai' | 'human')}
            className="mt-1 rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink"
          >
            <option value="ai">ai operator</option>
            <option value="human">human operator</option>
          </select>
        </label>
        <button
          type="submit"
          data-testid="member-add-submit"
          disabled={adding}
          className="self-end rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong disabled:opacity-60"
        >
          {adding ? 'Adding…' : 'Add member'}
        </button>
        {formError && (
          <p role="alert" data-testid="member-add-error" className="w-full text-body text-danger">
            {formError}
          </p>
        )}
      </form>
    </section>
  );
}

// settingsApi errors arrive as {status, data} RTK error objects.
function errorOf(err: unknown): string {
  const { status } = (err ?? {}) as { status?: unknown };
  return typeof status === 'string' ? status : '';
}

function num(raw: string): number {
  const parsed = Number(raw);
  return Number.isFinite(parsed) ? Math.trunc(parsed) : 0;
}

const EMPTY_POLICY: RefirePolicyDoc = {
  maxAttempts: 0,
  backoffSeconds: 0,
  transient: [],
  parks: [],
  cosmetics: [],
};