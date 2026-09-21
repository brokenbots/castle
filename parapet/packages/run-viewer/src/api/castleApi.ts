import { createApi } from '@reduxjs/toolkit/query/react';
import { fakeBaseQuery } from '@reduxjs/toolkit/query';
import { ConnectError } from '@connectrpc/connect';
import { Timestamp } from '@bufbuild/protobuf';
import { getRunDataSource } from './dataSource';
import { connectCodeName } from './errors';
import type { Run as PbRun } from '../gen/criteria/v1/criteria_pb';
import type { Agent as PbAgent, InspectRunResponse as PbInspectRunResponse } from '../gen/criteria/v1/server_pb';
import type { Envelope } from '../gen/criteria/v1/events_pb';

export interface Run {
  runId: string;
  criteriaId: string;
  workflowName: string;
  workflowHash: string;
  status: string;
  createdAt?: string;
  startedAt?: string;
  endedAt?: string;
  finalState: string;
  failureReason: string;
  ticket?: string;
  repoUrl?: string;
  prUrl?: string;
}

export interface Agent {
  criteriaId: string;
  name: string;
  labels: Record<string, string>;
  status: string;
  registeredAt?: string;
  lastSeenAt?: string;
}

// Adapter inspection summary returned by ServerService.InspectRun.
export interface RunInspection {
  runId: string;
  sessionId: string;
  adapter: string;
  currentStep: string;
  pendingPermissions: number;
  lastActivityAt?: string;
  // Opaque adapter state; the host pretty-prints any well-formed JSON.
  stateJson: string;
}

export interface InspectRunArgs {
  runId: string;
  // Optional adapter session id; empty asks the server for the summary.
  sessionId?: string;
}

export interface EventEnvelope {
  schemaVersion: number;
  runId: string;
  seq: number;
  type: string;
  ts?: string;
  correlationId: string;
  payload: unknown;
}

export function tsToIso(ts?: Timestamp): string | undefined {
  if (!ts) return undefined;
  try {
    return ts.toDate().toISOString();
  } catch {
    return undefined;
  }
}

function orUndefined(s?: string): string | undefined {
  return s ? s : undefined;
}

export function mapRun(r: PbRun): Run {
  return {
    runId: r.runId,
    criteriaId: r.criteriaId,
    workflowName: r.workflowName,
    workflowHash: r.workflowHash,
    status: r.status,
    createdAt: tsToIso(r.createdAt),
    startedAt: tsToIso(r.startedAt),
    endedAt: tsToIso(r.endedAt),
    finalState: r.finalState,
    failureReason: r.failureReason,
    ticket: orUndefined(r.ticket),
    repoUrl: orUndefined(r.repoUrl),
    prUrl: orUndefined(r.prUrl),
  };
}

export function mapAgent(a: PbAgent): Agent {
  return {
    criteriaId: a.criteriaId,
    name: a.name,
    labels: { ...a.labels },
    status: a.status,
    registeredAt: tsToIso(a.registeredAt),
    lastSeenAt: tsToIso(a.lastSeenAt),
  };
}

export function mapRunInspection(r: PbInspectRunResponse): RunInspection {
  return {
    runId: r.runId,
    sessionId: r.sessionId,
    adapter: r.adapter,
    currentStep: r.currentStep,
    pendingPermissions: Number(r.pendingPermissions),
    lastActivityAt: tsToIso(r.lastActivityAt),
    stateJson: r.stateJson ?? '',
  };
}

export function mapEnvelope(e: Envelope): EventEnvelope {
  const payload = e.payload;
  let type = '';
  let value: unknown = undefined;
  if (payload && payload.case) {
    type = payload.case;
    const msg = payload.value as { toJson?: () => unknown } | undefined;
    value = typeof msg?.toJson === 'function' ? msg.toJson() : msg;
  }
  return {
    schemaVersion: e.schemaVersion,
    runId: e.runId,
    seq: Number(e.seq),
    type,
    ts: tsToIso(e.ts),
    correlationId: e.correlationId,
    payload: value,
  };
}

// connect-es types Code as a numeric enum; connectCodeName surfaces the
// canonical lower_snake connect code string (e.g. "failed_precondition") so
// the UI can render readable inline errors.
function toError(err: unknown) {
  if (err instanceof ConnectError) {
    return { status: connectCodeName(err.code), data: err.rawMessage };
  }
  return { status: 'CUSTOM_ERROR', data: err instanceof Error ? err.message : String(err) };
}

export interface ListRunsArgs {
  // Optional agent filter ('' means no filter, i.e. runs of any agent).
  criteriaId?: string;
  // Optional status filter ('' means no filter, i.e. all runs).
  status?: string;
  // Pagination cursor from a previous ListRunsResponse.next_page_token.
  pageToken?: string;
}

export interface RunsPage {
  runs: Run[];
  // '' when the server has no further page.
  nextPageToken: string;
}

// Page size requested for every ListRuns call (page_token cursor paging).
export const RUNS_PAGE_LIMIT = 50;

// Fixed reducer path: parapet's middleware and tests match on it; hosts must
// keep it stable across versions of this package.
export const RUN_VIEWER_API_REDUCER_PATH = 'castleApi';

export const castleApi = createApi({
  reducerPath: RUN_VIEWER_API_REDUCER_PATH,
  baseQuery: fakeBaseQuery<{ status: string | number; data: string }>(),
  tagTypes: ['Run', 'Agent'],
  endpoints: (b) => ({
    listRuns: b.query<RunsPage, ListRunsArgs>({
      queryFn: async ({ criteriaId = '', status = '', pageToken = '' }) => {
        try {
          const resp = await getRunDataSource().listRuns({ criteriaId, status, pageToken });
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      // One cache entry per (criteriaId, status, pageToken) so "Load more"
      // pages are separate entries the component accumulates itself. RTK Query
      // refetches (polling, tag invalidation) re-initiate a cache entry with
      // its stored originalArgs, so a shared key would let the cursor args
      // from "Load more" hijack page 1's poll and make every poll refetch
      // the last cursor page. Keeping pageToken in the key pins page 1's
      // entry to pageToken '' so polls always refresh page 1; the cursor
      // entries are unsubscribed one-shot fetches that no poll targets.
      serializeQueryArgs: ({ endpointName, queryArgs }) =>
        `${endpointName}(${queryArgs.criteriaId ?? ''}|${queryArgs.status ?? ''}|${queryArgs.pageToken ?? ''})`,
      providesTags: ['Run'],
    }),
    getRun: b.query<Run, string>({
      queryFn: async (runId) => {
        try {
          const resp = await getRunDataSource().getRun(runId);
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, id) => [{ type: 'Run', id }],
    }),
    inspectRun: b.query<RunInspection, InspectRunArgs>({
      queryFn: async ({ runId, sessionId = '' }) => {
        try {
          const resp = await getRunDataSource().inspectRun({ runId, sessionId });
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      // Shares the Run id tag with getRun so control mutations
      // (pause/resume/stop) also refresh the adapter inspection.
      providesTags: (_r, _e, { runId }) => [{ type: 'Run', id: runId }],
    }),
    listAgents: b.query<Agent[], void>({
      queryFn: async () => {
        try {
          const resp = await getRunDataSource().listAgents();
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: ['Agent'],
    }),
    // Single-agent lookup backing the agent detail route
    // (/agents/:criteriaId). Keyed by the Agent id tag so cache
    // invalidations for an agent refresh its detail view.
    getAgent: b.query<Agent, string>({
      queryFn: async (criteriaId) => {
        try {
          const resp = await getRunDataSource().getAgent(criteriaId);
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, criteriaId) => [{ type: 'Agent', id: criteriaId }],
    }),
    // Lightweight probe for the shell's connection indicator: a minimal
    // authenticated RPC (one agent) that answers "is Castle reachable and is
    // the token still accepted". Deliberately untagged so it never
    // participates in Agent cache invalidations.
    getConnectionStatus: b.query<void, void>({
      queryFn: async () => {
        try {
          await getRunDataSource().connectionStatus();
          // Deliberately no payload: consumers derive the indicator state
          // from the request lifecycle (fulfilled vs errored).
          return { data: undefined };
        } catch (err) {
          return { error: toError(err) };
        }
      },
    }),
    resume: b.mutation<
      { issuedAt?: string },
      { runId: string; signal?: string; payload?: Record<string, string> }
    >({
      queryFn: async ({ runId, signal, payload }) => {
        try {
          const resp = await getRunDataSource().resume({ runId, signal, payload });
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { runId }) => [{ type: 'Run', id: runId }],
    }),
    pauseRun: b.mutation<{ issuedAt?: string }, { runId: string }>({
      queryFn: async ({ runId }) => {
        try {
          const resp = await getRunDataSource().pauseRun(runId);
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { runId }) => [{ type: 'Run', id: runId }],
    }),
    stopRun: b.mutation<
      { issuedAt?: string },
      { runId: string; reason?: string }
    >({
      queryFn: async ({ runId, reason }) => {
        try {
          const resp = await getRunDataSource().stopRun({ runId, reason });
          return { data: resp };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { runId }) => [{ type: 'Run', id: runId }],
    }),
  }),
});

export const {
  useListRunsQuery,
  useGetRunQuery,
  useInspectRunQuery,
  useListAgentsQuery,
  useGetAgentQuery,
  useGetConnectionStatusQuery,
  useResumeMutation,
  usePauseRunMutation,
  useStopRunMutation,
} = castleApi;
