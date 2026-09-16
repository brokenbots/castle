// Time formatting shared by the run list. Relative and duration strings are
// built by hand (locale-free) and computed against an explicit `now` so
// callers and tests stay deterministic.

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0s';
  const totalSeconds = Math.floor(ms / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const seconds = totalSeconds % 60;
  const totalMinutes = Math.floor(totalSeconds / 60);
  const minutes = totalMinutes % 60;
  const hours = Math.floor(totalMinutes / 60);
  if (hours === 0) return `${minutes}m ${String(seconds).padStart(2, '0')}s`;
  const days = Math.floor(hours / 24);
  if (days === 0) return `${hours}h ${String(minutes).padStart(2, '0')}m`;
  return `${days}d ${String(hours % 24).padStart(2, '0')}h`;
}

export function formatRelativeTime(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const delta = Math.max(0, now - t);
  const seconds = Math.floor(delta / 1000);
  if (seconds < 45) return 'just now';
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return minutes === 1 ? '1 minute ago' : `${minutes} minutes ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return hours === 1 ? '1 hour ago' : `${hours} hours ago`;
  const days = Math.floor(hours / 24);
  return days === 1 ? '1 day ago' : `${days} days ago`;
}

// Absolute rendering for hover titles (`title` attribute).
export function formatAbsoluteTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleString();
}

// Elapsed milliseconds between startedAt and endedAt; when endedAt is absent
// the caller's `now` is used (live elapsed for in-flight runs).
export function durationBetweenMs(
  startedAt?: string,
  endedAt?: string,
  now: number = Date.now(),
): number | undefined {
  if (!startedAt) return undefined;
  const start = Date.parse(startedAt);
  if (Number.isNaN(start)) return undefined;
  const end = endedAt ? Date.parse(endedAt) : now;
  if (Number.isNaN(end)) return undefined;
  return Math.max(0, end - start);
}