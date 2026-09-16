// Terminal run statuses, mirroring castle/internal/rpc/overseer.go isTerminal.
// A run in one of these states never transitions again, so the run list
// stops polling once every loaded run is terminal.
export const RUN_TERMINAL_STATUSES = new Set(['succeeded', 'failed', 'cancelled']);