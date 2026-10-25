import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { PageHeader, PageState, StickyTableHead, classifyError, useGetCallerIdentityQuery } from '@castle/run-viewer';
import { useCreateProjectMutation, useListProjectsQuery } from './settingsApi';
import { validateProjectID } from './validation';

// Settings console landing page (KB-327): the project inventory plus the
// one creation form. Membership policy is the create-time decision castle
// enforces — OPEN lets agents self-join at registration, CLOSED admits
// members only through this console's members editor.
//
// KB-333: the creation form is an admin surface gated on the caller's own
// operator role attribute, not on the credential class; the server keeps
// enforcing authorization on every write either way.
export function ProjectListPage() {
  const navigate = useNavigate();
  const { data, isLoading, error, refetch } = useListProjectsQuery();
  const errorKind = error ? classifyError(error) : undefined;
  const [createProject, { isLoading: creating }] = useCreateProjectMutation();
  const { data: identity, isError: identityFailed } = useGetCallerIdentityQuery();
  const isAdmin = identity?.role === 'admin';

  const [slug, setSlug] = useState('');
  const [policy, setPolicy] = useState<'open' | 'closed'>('open');
  const slugError = slug === '' ? null : validateProjectID(slug);
  const [createError, setCreateError] = useState<string | null>(null);

  const projects = useMemo(() => data ?? [], [data]);

  async function submit() {
    setCreateError(null);
    const validation = slug === '' ? 'Slug is required.' : validateProjectID(slug);
    if (validation) return setCreateError(validation);
    try {
      const created = await createProject({ slug, membershipPolicy: policy }).unwrap();
      setSlug('');
      navigate(`/settings/projects/${encodeURIComponent(created.id)}`);
    } catch (err) {
      setCreateError(createDetail(err));
    }
  }

  return (
    <div>
      <PageHeader title="Projects" />
      {identity === undefined && !identityFailed ? null : isAdmin ? (
        <form
          data-testid="project-create"
          aria-label="Create project"
          className="mb-4 flex flex-wrap items-start gap-2 rounded-lg border border-line bg-surface p-3"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
        <label className="flex flex-col text-meta text-ink-muted">
          Slug
          <input
            data-testid="project-create-slug"
            aria-label="Project slug"
            value={slug}
            onChange={(e) => setSlug(e.target.value)}
            placeholder="lowercase-slug"
            className="mt-1 w-56 rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink placeholder:text-ink-faint"
          />
        </label>
        <label className="flex flex-col text-meta text-ink-muted">
          Membership policy
          <select
            data-testid="project-create-policy"
            aria-label="Membership policy"
            value={policy}
            onChange={(e) => setPolicy(e.target.value as 'open' | 'closed')}
            className="mt-1 rounded-md border border-line bg-surface-raised px-2 py-1.5 text-body text-ink"
          >
            <option value="open">open — agents may self-join</option>
            <option value="closed">closed — members added via console</option>
          </select>
        </label>
        <button
          type="submit"
          data-testid="project-create-submit"
          disabled={creating}
          className="self-end rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong disabled:opacity-60"
        >
          {creating ? 'Creating…' : 'Create project'}
        </button>
        {(slugError || createError) && (
          <p role="alert" data-testid="project-create-error" className="w-full text-body text-danger">
            {createError ?? slugError}
          </p>
        )}
      </form>
      ) : (
        <p
          data-testid="project-create-admin-required"
          className="mb-4 rounded-lg border border-line bg-surface p-3 text-body text-ink-muted"
        >
          {identityFailed
            ? 'Your operator role could not be verified, so project creation is unavailable.'
            : 'Creating projects needs an operator with the admin role; this session is not one.'}
        </p>
      )}

      {isLoading ? (
        <PageState loading title="Loading projects…" testId="project-list-loading" />
      ) : error ? (
        errorKind === 'unauthenticated' ? (
          <PageState error kind="unauthenticated" />
        ) : (
          <PageState
            error
            kind={errorKind}
            title="Failed to load projects."
            detail="Castle is unreachable or failed to answer. Try again."
            onRetry={() => void refetch()}
            testId="project-list-error"
          />
        )
      ) : projects.length === 0 ? (
        <PageState
          empty
          title="No projects yet."
          detail="Create the first project above; agents register into projects and dispatch is restricted to their member set."
          testId="project-list-empty"
        />
      ) : (
        <table className="w-full text-sm" data-testid="project-list-table">
          <StickyTableHead>
            <tr className="border-b border-line">
              <th className="py-2 pr-4 text-left font-medium">Slug</th>
              <th className="py-2 pr-4 text-left font-medium">Membership</th>
              <th className="py-2 pr-4 text-left font-medium">Default refire</th>
              <th className="py-2 text-left font-medium">Queues</th>
            </tr>
          </StickyTableHead>
          <tbody>
            {projects.map((p) => (
              <tr key={p.id} data-testid="project-list-row" className="border-b border-line transition-colors hover:bg-surface-raised/50">
                <td className="py-2 pr-4">
                  <Link to={`/settings/projects/${encodeURIComponent(p.id)}`} className="text-ink hover:text-accent-strong hover:underline">
                    {p.id}
                  </Link>
                </td>
                <td className="py-2 pr-4 text-ink-muted">{p.membershipPolicy}</td>
                <td className="py-2 pr-4 text-ink-muted">{p.defaultRefirePolicy || '—'}</td>
                <td className="py-2 text-ink-muted">{p.queues.join(', ') || '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// Mirrors the settingsApi error map ({status, data}) into one line of copy.
function createDetail(err: unknown): string {
  const { status, data } = (err ?? {}) as { status?: unknown; data?: unknown };
  const message = typeof data === 'string' ? data : '';
  if (status === 'already_exists') return `Project already exists. ${message}`.trim();
  return message || 'Create failed. Try again.';
}