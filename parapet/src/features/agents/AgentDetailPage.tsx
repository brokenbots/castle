import { useEffect, useMemo, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useDispatch } from 'react-redux';
import { castleApi, useGetAgentQuery, useListRunsQuery, type Run } from '@castle/run-viewer';
import { classifyError } from '@castle/run-viewer';
import type { AppDispatch } from '../../store';
import { PageHeader } from '@castle/run-viewer';
import { PageState } from '@castle/run-viewer';
import { Breadcrumbs } from '@castle/run-viewer';
import { useDocumentTitle } from '@castle/run-viewer';
import { RUN_STATUS_TEXT_COLORS } from '@castle/run-viewer';
import { StartedCell, useDocumentVisible, useNow } from '@castle/run-viewer';

// A page of the agent's runs fetched through "Load more" (page 1 lives in
// the listRuns cache; these entries hold the older pages).
interface CursorPage {
  pageToken: string;
  nextPageToken: string;
  runs: Run[];
}

// Agent detail: identity, labels, status and registered/last-seen info plus
// the agent's runs, fetched through ListRuns with the criteria_id filter.
// Reached from the agents list via /agents/:criteriaId.
export function AgentDetailPage() {
  const { criteriaId = '' } = useParams();
  const dispatch = useDispatch<AppDispatch>();
  const agent = useGetAgentQuery(criteriaId);
  // The agent's runs: page 1 through the cached listRuns entry; "Load more"
  // appends older cursor pages on demand. No polling — the detail view is a
  // point-in-time read, refreshed on navigation.
  const runsPage = useListRunsQuery({ criteriaId }, { refetchOnMountOrArgChange: true });
  const visible = useDocumentVisible();
  // Relative "started" labels advance on a 30s clock.
  const now = useNow(visible, 30_000);
  useDocumentTitle(agent.data?.name);

  const [cursorPages, setCursorPages] = useState<CursorPage[]>([]);
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState(false);

  // Cursor pages belong to the agent they were loaded under: navigating to
  // another agent's detail view (the element stays mounted) invalidates
  // them.
  useEffect(() => {
    setCursorPages([]);
    setLoadMoreError(false);
  }, [criteriaId]);

  const firstPageRuns = runsPage.data?.runs ?? [];
  const cursor =
    cursorPages.length > 0
      ? cursorPages[cursorPages.length - 1].nextPageToken
      : (runsPage.data?.nextPageToken ?? '');
  // First occurrence wins so a run refreshed in page 1 keeps its live
  // status over a stale cursor-page copy.
  const runs = useMemo(() => {
    const seen = new Set<string>();
    const out: Run[] = [];
    for (const r of [...firstPageRuns, ...cursorPages.flatMap((p) => p.runs)]) {
      if (seen.has(r.runId)) continue;
      seen.add(r.runId);
      out.push(r);
    }
    return out;
  }, [firstPageRuns, cursorPages]);

  const loadMore = async () => {
    const requestedCursor = cursor;
    setLoadingMore(true);
    setLoadMoreError(false);
    const subscription = dispatch(
      castleApi.endpoints.listRuns.initiate(
        { criteriaId, pageToken: requestedCursor },
        { forceRefetch: true },
      ),
    );
    try {
      const page = await subscription.unwrap();
      setCursorPages((pages) =>
        pages.some((p) => p.pageToken === requestedCursor)
          ? pages
          : [
              ...pages,
              { pageToken: requestedCursor, nextPageToken: page.nextPageToken, runs: page.runs },
            ],
      );
    } catch {
      setLoadMoreError(true);
    } finally {
      subscription.unsubscribe();
      setLoadingMore(false);
    }
  };

  if (agent.isLoading) {
    return <PageState loading title="Loading agent…" testId="agent-detail-loading" />;
  }
  if (agent.error || !agent.data) {
    const kind = agent.error ? classifyError(agent.error) : 'unknown';
    if (kind === 'unauthenticated') {
      return <PageState error kind="unauthenticated" />;
    }
    return (
      <PageState
        error
        kind={kind}
        title={kind === 'not_found' ? 'Agent not found.' : 'Failed to load this agent.'}
        detail={
          kind === 'not_found'
            ? "This agent doesn't exist or was removed."
            : 'Castle is unreachable or failed to answer. Try again.'
        }
        onRetry={kind === 'not_found' ? undefined : () => void agent.refetch()}
        action={
          <Link
            to="/agents"
            className="text-body text-ink-muted hover:text-ink hover:underline"
          >
            Back to agents
          </Link>
        }
      />
    );
  }
  const agentData = agent.data;
  const labels = Object.entries(agentData.labels);

  return (
    <div>
      <Breadcrumbs items={[{ label: 'Agents', to: '/agents' }, { label: agentData.name }]} />
      <div className="mt-4">
        <PageHeader title={agentData.name} meta={agentData.criteriaId} />
      </div>

      <section className="mt-4 grid gap-6 md:grid-cols-2" data-testid="agent-detail">
        <div>
          <h3 className="text-lg font-semibold mb-2">Details</h3>
          <dl className="text-sm">
            <div className="flex gap-2 py-1">
              <dt className="text-slate-400 w-32 shrink-0">Criteria ID</dt>
              <dd className="font-mono" data-testid="agent-criteria-id">
                {agentData.criteriaId}
              </dd>
            </div>
            <div className="flex gap-2 py-1">
              <dt className="text-slate-400 w-32 shrink-0">Status</dt>
              <dd
                data-testid="agent-status"
                className={agentData.status === 'online' ? 'text-emerald-400' : 'text-slate-500'}
              >
                {agentData.status}
              </dd>
            </div>
            <div className="flex gap-2 py-1">
              <dt className="text-slate-400 w-32 shrink-0">Registered</dt>
              <dd data-testid="agent-registered">
                {agentData.registeredAt
                  ? new Date(agentData.registeredAt).toLocaleString()
                  : '—'}
              </dd>
            </div>
            <div className="flex gap-2 py-1">
              <dt className="text-slate-400 w-32 shrink-0">Last seen</dt>
              <dd data-testid="agent-last-seen">
                {agentData.lastSeenAt ? new Date(agentData.lastSeenAt).toLocaleString() : '—'}
              </dd>
            </div>
          </dl>
        </div>
        <div>
          <h3 className="text-lg font-semibold mb-2">Labels</h3>
          {labels.length === 0 ? (
            <p className="text-sm text-slate-400" data-testid="agent-labels">
              No labels.
            </p>
          ) : (
            <ul className="flex flex-wrap gap-2" data-testid="agent-labels">
              {labels.map(([key, value]) => (
                <li
                  key={key}
                  className="rounded-full border border-line bg-surface px-2 py-0.5 font-mono text-xs"
                >
                  <span className="text-slate-400">{key}:</span> {value}
                </li>
              ))}
            </ul>
          )}
        </div>
      </section>

      <section className="mt-6">
        <h3 className="text-lg font-semibold mb-2">Runs</h3>
        {runsPage.isLoading ? (
          <PageState loading title="Loading runs…" testId="agent-runs-loading" />
        ) : runsPage.error && !runsPage.data ? (
          classifyError(runsPage.error) === 'unauthenticated' ? (
            <PageState error kind="unauthenticated" />
          ) : (
            <PageState
              error
              kind={classifyError(runsPage.error)}
              title="Failed to load runs."
              detail="Castle is unreachable or failed to answer. Try again."
              onRetry={() => void runsPage.refetch()}
            />
          )
        ) : runs.length === 0 ? (
          <PageState
            empty
            title="No runs for this agent yet."
            detail="Runs appear here once this agent starts executing workflows."
          />
        ) : (
          <table className="w-full text-left text-sm">
            <thead className="text-slate-400">
              <tr>
                <th className="px-2 py-1">ID</th>
                <th className="px-2 py-1">Workflow</th>
                <th className="px-2 py-1">Status</th>
                <th className="px-2 py-1">Started</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((run) => (
                <tr key={run.runId} className="border-t border-slate-800">
                  <td className="px-2 py-1 font-mono">
                    <Link className="text-sky-400 hover:underline" to={`/runs/${run.runId}`}>
                      {run.runId}
                    </Link>
                  </td>
                  <td className="px-2 py-1">{run.workflowName}</td>
                  <td className={`px-2 py-1 ${RUN_STATUS_TEXT_COLORS[run.status] ?? ''}`}>
                    {run.status}
                  </td>
                  <td className="px-2 py-1">
                    <StartedCell run={run} now={now} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {cursor !== '' && !runsPage.isLoading && (
          <div className="mt-4">
            <button
              type="button"
              className="rounded border border-slate-600 px-3 py-1 text-sm text-sky-300 hover:bg-slate-800"
              onClick={loadMore}
              disabled={loadingMore}
            >
              {loadingMore ? 'Loading…' : 'Load more'}
            </button>
            {loadMoreError && <p className="mt-2 text-rose-400">Failed to load more runs.</p>}
          </div>
        )}
      </section>
    </div>
  );
}