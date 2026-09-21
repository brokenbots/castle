import { useEffect, useState } from 'react';
import type { Run } from '../../api/castleApi';
import {
  durationBetweenMs,
  formatAbsoluteTime,
  formatDuration,
  formatRelativeTime,
} from './time';
import { RUN_TERMINAL_STATUSES } from './runStatus';

// Table cells and small hooks shared by the run tables (run list page and
// the per-agent run list on the agent detail page). Relative and live
// values are computed against an explicit `now` so callers and tests stay
// deterministic.

export function useDocumentVisible(): boolean {
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
export function useNow(enabled: boolean, intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!enabled) return;
    setNow(Date.now());
    const id = window.setInterval(() => setNow(Date.now()), intervalMs);
    return () => window.clearInterval(id);
  }, [enabled, intervalMs]);
  return now;
}

export function StartedCell({ run, now }: { run: Run; now: number }) {
  const startedIso = run.startedAt ?? run.createdAt;
  if (!startedIso) return <>—</>;
  return (
    <span title={formatAbsoluteTime(startedIso)}>{formatRelativeTime(startedIso, now)}</span>
  );
}

export function DurationCell({ run }: { run: Run }) {
  const live = !RUN_TERMINAL_STATUSES.has(run.status) && !run.endedAt && Boolean(run.startedAt);
  const now = useNow(live, 1_000);
  const ms = durationBetweenMs(run.startedAt, run.endedAt, now);
  return <>{ms === undefined ? '—' : formatDuration(ms)}</>;
}