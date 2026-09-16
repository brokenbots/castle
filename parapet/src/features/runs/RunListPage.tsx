import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { useDispatch, useSelector } from 'react-redux';
import { castleApi, useListRunsQuery, type Run } from '../../api/castleApi';
import type { AppDispatch, RootState } from '../../store';
import { RUN_TERMINAL_STATUSES } from './runStatus';
import {
  durationBetweenMs,
  formatAbsoluteTime,
  formatDuration,
  formatRelativeTime,
} from './time';

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

function useDocumentVisible(): boolean {
  const [visible, setVisible] = useState(() => document.visibilityState === 'visible');
  useEffect(() => {
    const onChange = () => setVisible(document.visibilityState === 'visible');
    document.addEventListener('visibilitychange', onChange);
    return () => document.removeEventListener('visibilitychange', onChange);
  }, []);
  return visible;
}

// A clock that ticks at intervalMs while enabled; used for live elapsed
// durations and relative "started" labels.
function useNow(enabled: boolean, intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!enabled) return;
    setNow(Date.now());
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [enabled, intervalMs]);
  return now;
}

// statusColor mirrors the run-detail page's StatusPill palette.
const statusColor: Record<string, string> = {
  running: 'text-amber-400',
  succeeded: 'text-emerald-400',
  failed: 'text-rose-400',
  pending: 'text-slate-400',
  cancelled: 'text-slate-500',
};

function StartedCell({ run }: { run: Run }) {
  const startedIso = run.startedAt ?? run.createdAt;
  if (!startedIso) return <>—</>;
  return (
    <span title={formatAbsoluteTime(startedIso)}>
      {formatRelativeTime(startedIso, Date.now())}
    </span>
  );
}

function DurationCell({ run }: { run: Run }) {
  const live = !RUN_TERMINAL_STATUSES.has(run.status) && !run.endedAt && Boolean(run.startedAt);
  const now = useNow(live, 1_000);
  const ms = durationBetweenMs(run.startedAt, run.endedAt, now);
  return <>{ms === undefined ? '—' : formatDuration(ms)}</>;
}

export function RunListPage() {
  const dispatch = useDispatch<AppDispatch>();
  const [statusFilter, setStatusFilter] = useState('');
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreError, setLoadMoreError] = useState(false);
  const visible = useDocumentVisible();

  // The polling gate needs the runs currently in cache (a poll must stop
  // once every run is terminal). Subscribing to the same cache entry via
  // the endpoint selector shares the data without a second request.
  const hasActiveRuns = useSelector((state: RootState) => {
    const cached = castleApi.endpoints.listRuns.select({ status: statusFilter })(state).data;
    return (cached?.runs ?? []).some((r) => !RUN_TERMINAL_STATUSES.has(r.status));
  });

  // The hook arg stays the first page; polling always refreshes page 1.
  const { data, isLoading, error } = useListRunsQuery(
    { status: statusFilter },
    {
      // A fresh visit to the list always re-issues page 1 so runs created
      // elsewhere show up without waiting for a poll.
      refetchOnMountOrArgChange: true,
      pollingInterval:
        visible && hasActiveRuns ? RUN_LIST_POLL_INTERVAL_MS : 0,
    },
  );
  const runs = data?.runs ?? [];
  const cursor = data?.nextPageToken ?? '';

  const loadMore = async () => {
    setLoadingMore(true);
    setLoadMoreError(false);
    const subscription = dispatch(
      castleApi.endpoints.listRuns.initiate(
        { status: statusFilter, pageToken: cursor },
        { forceRefetch: true },
      ),
    );
    try {
      await subscription.unwrap();
    } catch {
      setLoadMoreError(true);
    } finally {
      subscription.unsubscribe();
      setLoadingMore(false);
    }
  };

  return (
    <div className="p-6">
      <div className="mb-4 flex items-center justify-between">
        <h1 className="text-xl font-semibold">Runs</h1>
        <label htmlFor="run-status-filter" className="flex items-center gap-2 text-sm">
          Status
          <select
            id="run-status-filter"
            className="rounded border border-slate-700 bg-slate-900 px-2 py-1 text-sm"
            value={statusFilter}
            onChange={(e) => {
              setStatusFilter(e.target.value);
              setLoadMoreError(false);
            }}
          >
            {STATUS_FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </label>
      </div>
      {isLoading ? (
        <p>Loading runs…</p>
      ) : error && !data ? (
        <p className="text-rose-400">Failed to load runs.</p>
      ) : runs.length === 0 ? (
        <p className="text-slate-400">No runs.</p>
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
                <td className={`px-2 py-1 ${statusColor[run.status] ?? ''}`}>{run.status}</td>
                <td className="px-2 py-1">
                  <StartedCell run={run} />
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

