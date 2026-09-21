import { castleRunDataSource } from './castleDataSource';
import type {
  Agent,
  EventEnvelope,
  InspectRunArgs,
  ListRunsArgs,
  Run,
  RunInspection,
  RunsPage,
} from './castleApi';

/** Argument shape for the resume control RPC. */
export interface ResumeArgs {
  runId: string;
  signal?: string;
  payload?: Record<string, string>;
}

/** Argument shape for the stop control RPC. */
export interface StopRunArgs {
  runId: string;
  reason?: string;
}

/** How a single opened run stream ended. */
export interface RunStreamEnd {
  /**
   * 'terminal': a terminal event (runCompleted / runFailed) was delivered —
   * normal end. 'clean': the stream ended without an error before any
   * terminal event (server restart or eviction). 'unauthenticated': the
   * stream was rejected as unauthenticated (no retry, host re-auth applies).
   * 'error': the stream failed (retried with bounded backoff).
   */
  kind: 'terminal' | 'clean' | 'unauthenticated' | 'error';
  /** Canonical connect code name for an 'error' end, when available. */
  codeName?: string;
  /** Safe, human-readable reason for the end (truncated). */
  message?: string;
}

export interface RunStreamArgs {
  runId: string;
  /** Resume from the first seq strictly greater than this. */
  sinceSeq: number;
  subscriberId: string;
  signal: AbortSignal;
}

/**
 * The run-viewer data seam (CRI-257): everything the runs feature needs
 * from its host environment. The castle implementation (Connect client +
 * console session) and the local implementation (CRI-255 loopback control
 * RPC + criteria run-state files) both satisfy this contract, so the same
 * UI components drive both hosts.
 *
 * Event vocabulary: envelopes are the mapped plain-JSON shape (see
 * EventEnvelope); 'watchReady' marks the watch as established and
 * TERMINAL_EVENT_TYPES close the stream.
 */
export const TERMINAL_EVENT_TYPES = new Set(['runCompleted', 'runFailed']);

/** Argument shape for the event-log history walk. */
export interface ListRunEventsArgs {
  runId: string;
  /** Return events with seq strictly greater than this. */
  sinceSeq: number;
  limit: number;
}

/** One page of the event-log history walk. */
export interface RunEventsPage {
  events: EventEnvelope[];
  /** Highest seq in the whole run history at fetch time. */
  lastSeq: number;
  /** Cursor for the next older page, or null when there is no continuation. */
  nextSinceSeq: number | null;
}

export interface RunDataSource {
  listRuns(args: ListRunsArgs): Promise<RunsPage>;
  getRun(runId: string): Promise<Run>;
  inspectRun(args: InspectRunArgs): Promise<RunInspection>;
  listRunEvents(args: ListRunEventsArgs): Promise<RunEventsPage>;
  listAgents(): Promise<Agent[]>;
  getAgent(criteriaId: string): Promise<Agent>;
  /** Lightweight reachability probe backing the connection indicator. */
  connectionStatus(): Promise<void>;
  resume(args: ResumeArgs): Promise<{ issuedAt?: string }>;
  pauseRun(runId: string): Promise<{ issuedAt?: string }>;
  stopRun(args: StopRunArgs): Promise<{ issuedAt?: string }>;
  /**
   * Opens the run event stream, delivering each mapped envelope to
   * onEvent (including 'watchReady'); resolves when the stream ends.
   */
  openRunStream(
    args: RunStreamArgs,
    onEvent: (event: EventEnvelope) => void,
  ): Promise<RunStreamEnd>;
}

let current: RunDataSource | undefined;

/**
 * The data source backing runViewerApi and startWatch. Defaults to the
 * castle (Connect client) implementation; the standalone viewer swaps in
 * the local implementation via setRunDataSource before rendering.
 */
export function getRunDataSource(): RunDataSource {
  return current ?? castleRunDataSource;
}

export function setRunDataSource(dataSource: RunDataSource): void {
  current = dataSource;
}

/** Test hook: drop the current data source so the default is re-resolved. */
export function resetRunDataSource(): void {
  current = undefined;
}