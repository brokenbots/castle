import { useGetConnectionStatusQuery } from '@castle/run-viewer';

// Poll cadence for the shell's connection indicator.
export const CONNECTION_POLL_INTERVAL_MS = 30_000;

type ConnectionState = 'connecting' | 'online' | 'offline';

const STATE_DOT: Record<ConnectionState, string> = {
  connecting: 'bg-warning animate-pulse',
  online: 'bg-success',
  offline: 'bg-danger',
};

// Castle API reachability indicator: a lightweight authenticated probe
// (ListAgents, limit 1) polled on a slow cadence while the shell is mounted.
export function ConnectionStatus() {
  const { isLoading, isError } = useGetConnectionStatusQuery(undefined, {
    pollingInterval: CONNECTION_POLL_INTERVAL_MS,
  });
  const state: ConnectionState = isError ? 'offline' : isLoading ? 'connecting' : 'online';
  const label = `Castle connection: ${state}`;
  return (
    <div
      data-testid="connection-status"
      role="status"
      aria-label={label}
      title={label}
      className="flex items-center gap-2 text-body text-ink-muted"
    >
      <span aria-hidden className={`inline-block h-2 w-2 rounded-full ${STATE_DOT[state]}`} />
      {state}
    </div>
  );
}