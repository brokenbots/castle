import { useEffect, useMemo, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useDispatch, useSelector } from 'react-redux';
import { castleApi, useListRunsQuery, type Run } from '../../api/castleApi';
import { classifyError } from '../../api/errors';
import type { AppDispatch, RootState } from '../../store';
import { PageHeader } from '../../components/PageHeader';
import { PageState } from '../../components/PageState';
import { RUN_STATUS_TEXT_COLORS, RUN_TERMINAL_STATUSES } from './runStatus';
import { DurationCell, StartedCell, useDocumentVisible, useNow } from './runCells';

// Poll cadence while the tab is visible and at least one loaded run is not
// in a terminal state. When either condition stops holding, pollingInterval
// drops to 0 (RTK Query stops scheduling polls) instead of hard-polling
// forever.
export const RUN_LIST_POLL_INTERVAL_MS = 12_000;

const STATUS_FILTERS = [
  { value: '', label: 'all' },
  { value: 'running', label: 'running' },
  { value: 'succeeded', label: 'succeeded' },
  { value: 'failed', label: 'failed' },
  { value: 'cancelled', label: 'cancelled' },
] as const;

// A page fetched through "Load more". Page 1 lives in the listRuns cache;
// these entries hold the older pages. `pageToken` is the cursor this page
// was requested with; `nextPageToken` is the continuation token the server
// returned for it — the cursor chain.
interface CursorPage {
  pageToken: string;
  nextPageToken: string;
  runs: Run[];
}

// Builds the URL query for a run-list status filter; the empty filter clears
// the param so the unfiltered view is the canonical `/runs` URL.
function statusSearchParams(status: string): URLSearchParams {
  const params = new URLSearchParams();
  if (status) params.set('status', status);
  return params;
}

export function RunListPage() {
  const dispatch = useDispatch<AppDispatch>();
  // The status filter lives in the URL query (`/runs?status=running`) so
  // filtered views are shareable and deep-linkable: the select writes the
  // param, the param is the single source of truth for the filter, and
  // opening a URL carrying it restores the filtered view.
  const [searchParams, setSearchParams] = useSearchParams();
  const statusFilter = searchParams.get('status') ?? '';
  // Pages fetched through "Load more". Page 1 stays in the listRuns cache
  // (and is what polling refreshes); these entries hold the older pages.
  const [cursorPages, setCursorPages] = useState<CursorPage[]>([]);
  // Cursor pages are refreshed in lockstep with the page-1 poll: while at
  // least one loaded run is non-terminal, every poll tick also re-dispatches
  // each loaded cursor (see the refresh effect below), so page 2+ rows
  // transition in place instead of going stale.
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState(false);
  const visible = useDocumentVisible();
  // Guards "Load more" appends against a filter change that raced the
  // in-flight request: a response for the previous filter must not land
  // under the new one. Synced during render (the latest-value ref pattern)
  // so every commit that changes statusFilter updates the guard before any
  // in-flight response continuation can run.
  const statusRef = useRef(statusFilter);
  statusRef.current = statusFilter;

  // Cursor pages belong to the filter they were loaded under: any filter
  // change (select, status chip, back/forward) invalidates them.
  useEffect(() => {
    setCursorPages([]);
    setLoadMoreError(false);
  }, [statusFilter]);

  // The polling gate needs the runs currently loaded (a poll must stop once
  // every run is terminal). Page 1 is read from the cache entry polling
  // actually refreshes — via the endpoint selector, sharing data without a
  // second request. Cursor pages are component state, read directly: a
  // non-terminal older row keeps the poll alive so the lockstep cursor
  // refresh (below) can transition it in place; once every loaded run is
  // terminal the poll stops.
  const page1HasActiveRuns = useSelector((state: RootState) => {
    const cached = castleApi.endpoints.listRuns.select({ status: statusFilter })(state).data;
    return (cached?.runs ?? []).some((r) => !RUN_TERMINAL_STATUSES.has(r.status));
  });
  const cursorPagesHaveActiveRuns = cursorPages.some((p) =>
    p.runs.some((r) => !RUN_TERMINAL_STATUSES.has(r.status)),
  );
  const hasActiveRuns = page1HasActiveRuns || cursorPagesHaveActiveRuns;
  // Page 1's last fulfillment timestamp: every (re)fetch of the page-1 cache
  // entry (initial load, poll tick) bumps it. It is the poll tick the
  // lockstep cursor-page refresh below keys on. Polls target the page-1
  // entry only, so a cursor refresh landing never re-triggers it.
  const page1FulfilledAt = useSelector((state: RootState) =>
    castleApi.endpoints.listRuns.select({ status: statusFilter })(state).fulfilledTimeStamp,
  );

  const { data, isLoading, error, refetch } = useListRunsQuery(
    { status: statusFilter },
    {
      // A fresh visit to the list always re-issues page 1 so runs created
      // elsewhere show up without waiting for a poll.
      refetchOnMountOrArgChange: true,
      pollingInterval:
        visible && hasActiveRuns ? RUN_LIST_POLL_INTERVAL_MS : 0,
    },
  );
  const firstPageRuns = data?.runs ?? [];
  const errorKind = error ? classifyError(error) : undefined;
  // The next "Load more" cursor: once older pages exist, the most recent
  // page's own continuation token is the source of truth (page 1's token is
  // only consumed by the first click). Chain it so every click advances.
  const cursor =
    cursorPages.length > 0
      ? cursorPages[cursorPages.length - 1].nextPageToken
      : (data?.nextPageToken ?? '');
  // Page 1 first; older cursor pages fill in behind it. First occurrence
  // wins so a run that reappeared in a refreshed page 1 keeps its live
  // status instead of the stale cursor-page copy.
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
  // Relative "started" labels advance on a 30s clock even when no run is
  // active (DurationCell keeps its own 1s live clock for running rows).
  const now = useNow(visible, 30_000);

  const loadMore = async () => {
    const requestedFor = statusFilter;
    const requestedCursor = cursor;
    setLoadingMore(true);
    setLoadMoreError(false);
    const subscription = dispatch(
      castleApi.endpoints.listRuns.initiate(
        { status: requestedFor, pageToken: requestedCursor },
        { forceRefetch: true },
      ),
    );
    try {
      const page = await subscription.unwrap();
      if (statusRef.current !== requestedFor) return;
      setCursorPages((pages) =>
        pages.some((p) => p.pageToken === requestedCursor)
          ? pages
          : [
              ...pages,
              { pageToken: requestedCursor, nextPageToken: page.nextPageToken, runs: page.runs },
            ],
      );
    } catch {
      if (statusRef.current === requestedFor) setLoadMoreError(true);
    } finally {
      subscription.unsubscribe();
      setLoadingMore(false);
    }
  };

  // Cursor pages refresh in lockstep with the page-1 poll: whenever page 1
  // is (re)fetched while the poll gate is open — each poll tick — every
  // loaded cursor is re-dispatched so page 2+ rows transition in place, and
  // once the refreshed pages are all terminal the gate closes, polling
  // stops, and no further cursor refreshes are dispatched. lastPage1FetchedAt
  // pins the effect to actual page-1 fetches: the remaining dependencies
  // (gate inputs, cursor-page commits) can change between polls and must not
  // dispatch refreshes on their own.
  const lastPage1FetchedAt = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (page1FulfilledAt === lastPage1FetchedAt.current) return;
    lastPage1FetchedAt.current = page1FulfilledAt;
    if (!visible || !hasActiveRuns) return;
    if (cursorPages.length === 0) return;
    const requestedFor = statusFilter;
    for (const page of cursorPages) {
      const requestedCursor = page.pageToken;
      const subscription = dispatch(
        castleApi.endpoints.listRuns.initiate(
          { status: requestedFor, pageToken: requestedCursor },
          { forceRefetch: true },
        ),
      );
      void subscription
        .unwrap()
        .then((refreshed) => {
          if (statusRef.current !== requestedFor) return;
          setCursorPages((current) =>
            current.map((p) =>
              p.pageToken === requestedCursor ? { ...p, runs: refreshed.runs } : p,
            ),
          );
        })
        .catch(() => {
          // A failed cursor refresh keeps the previously loaded rows; the
          // poll stays alive while they are non-terminal and the next tick
          // retries.
        })
        .finally(() => subscription.unsubscribe());
    }
  }, [page1FulfilledAt, visible, hasActiveRuns, statusFilter, cursorPages, dispatch]);

  return (
    <div>
      <PageHeader
        title="Runs"
        actions={
          <label htmlFor="run-status-filter" className="flex items-center gap-2 text-body">
            Status
            <select
              id="run-status-filter"
              className="rounded-md border border-line bg-surface px-2 py-1 text-body"
              value={statusFilter}
              onChange={(e) => setSearchParams(statusSearchParams(e.target.value))}
            >
              {/* The filter accepts any status the server knows; when a deep
                  link carries a status outside the fixed list (e.g. pending
                  reached via a chip) keep it selectable instead of showing a
                  blank select. */}
              {(STATUS_FILTERS.some((f) => f.value === statusFilter)
                ? STATUS_FILTERS
                : [...STATUS_FILTERS, { value: statusFilter, label: statusFilter }]
              ).map((f) => (
                <option key={f.value} value={f.value}>
                  {f.label}
                </option>
              ))}
            </select>
          </label>
        }
      />
      {!isLoading && error && data && (
        <div className="mb-2 flex items-center gap-3">
          <p className="text-danger">
            Refresh failed.{runs.length > 0 ? ' Showing the last loaded runs.' : ''}
          </p>
          <button
            type="button"
            data-testid="run-list-retry-refresh"
            className="rounded-md border border-line-strong px-2 py-1 text-body text-ink hover:bg-surface-raised"
            onClick={() => void refetch()}
          >
            Try again
          </button>
        </div>
      )}
      {isLoading ? (
        <PageState loading title="Loading runs…" testId="run-list-loading" />
      ) : error && !data ? (
        errorKind === 'unauthenticated' ? (
          <PageState error kind="unauthenticated" />
        ) : (
          <PageState
            error
            kind={errorKind}
            title="Failed to load runs."
            detail="Castle is unreachable or failed to answer. Try again."
            onRetry={() => void refetch()}
          />
        )
      ) : runs.length === 0 ? (
        <PageState
          empty
          title="No runs yet."
          detail="Runs appear here once Castle starts executing workflows."
          testId="run-list-empty"
        />
      ) : (
        <table className="w-full text-left text-sm">
          <thead className="text-slate-400">
            <tr>
              <th className="px-2 py-1">ID</th>
              <th className="px-2 py-1">Ticket</th>
              <th className="px-2 py-1">Workflow</th>
              <th className="px-2 py-1">Status</th>
              <th className="px-2 py-1">Started</th>
              <th className="px-2 py-1">Duration</th>
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
                <td className="px-2 py-1">{run.ticket ?? '—'}</td>
                <td className="px-2 py-1">{run.workflowName}</td>
                <td className="px-2 py-1">
                  {/* Status chip links into the run list filtered by this
                      status, so a scan of mixed rows is one click from the
                      matching filtered view. */}
                  <Link
                    to={`/runs?status=${encodeURIComponent(run.status)}`}
                    className={`inline-flex items-center rounded-full border border-line px-2 py-0.5 text-xs font-semibold hover:bg-surface-raised ${
                      RUN_STATUS_TEXT_COLORS[run.status] ?? 'text-slate-300'
                    }`}
                  >
                    {run.status}
                  </Link>
                </td>
                <td className="px-2 py-1">
                  <StartedCell run={run} now={now} />
                </td>
                <td className="px-2 py-1">
                  <DurationCell run={run} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {cursor !== '' && !isLoading && (
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
    </div>
  );
}

