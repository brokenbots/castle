import { createApi } from '@reduxjs/toolkit/query/react';
import { fakeBaseQuery } from '@reduxjs/toolkit/query';
import { Code, ConnectError } from '@connectrpc/connect';
import { Timestamp } from '@bufbuild/protobuf';
import { server } from './client';
import type { Run as PbRun } from '../gen/criteria/v1/criteria_pb';
import type { Agent as PbAgent } from '../gen/criteria/v1/server_pb';
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

export interface EventEnvelope {
  schemaVersion: number;
  runId: string;
  seq: number;
  type: string;
  ts?: string;
  correlationId: string;
  payload: unknown;
}

function tsToIso(ts?: Timestamp): string | undefined {
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

function mapRun(r: PbRun): Run {
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

function mapAgent(a: PbAgent): Agent {
  return {
    criteriaId: a.criteriaId,
    name: a.name,
    labels: { ...a.labels },
    status: a.status,
    registeredAt: tsToIso(a.registeredAt),
    lastSeenAt: tsToIso(a.lastSeenAt),
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

// connect-es types Code as a numeric enum; surface the canonical
// lower_snake connect code string (e.g. "failed_precondition") so the UI can
// render readable inline errors.
function connectCodeName(code: Code): string {
  const name = Code[code];
  if (!name) return String(code);
  return (
    name.charAt(0).toLowerCase() +
    name.slice(1).replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`)
  );
}

function toError(err: unknown) {
  if (err instanceof ConnectError) {
    return { status: connectCodeName(err.code), data: err.rawMessage };
  }
  return { status: 'CUSTOM_ERROR', data: err instanceof Error ? err.message : String(err) };
}

export const castleApi = createApi({
  reducerPath: 'castleApi',
  baseQuery: fakeBaseQuery<{ status: string | number; data: string }>(),
  tagTypes: ['Run', 'Agent'],
  endpoints: (b) => ({
    listRuns: b.query<Run[], void>({
      queryFn: async () => {
        try {
          const resp = await server.listRuns({});
          return { data: resp.runs.map(mapRun) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: ['Run'],
    }),
    getRun: b.query<Run, string>({
      queryFn: async (runId) => {
        try {
          const resp = await server.getRun({ runId });
          return { data: mapRun(resp) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, id) => [{ type: 'Run', id }],
    }),
    listAgents: b.query<Agent[], void>({
      queryFn: async () => {
        try {
          const resp = await server.listAgents({});
          return { data: resp.agents.map(mapAgent) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: ['Agent'],
    }),
    resume: b.mutation<
      { issuedAt?: string },
      { runId: string; signal?: string; payload?: Record<string, string> }
    >({
      queryFn: async ({ runId }) => {
        try {
          const resp = await server.resumeRun({ runId });
          return { data: { issuedAt: tsToIso(resp.issuedAt) } };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { runId }) => [{ type: 'Run', id: runId }],
    }),
    pauseRun: b.mutation<{ issuedAt?: string }, { runId: string }>({
      queryFn: async ({ runId }) => {
        try {
          const resp = await server.pauseRun({ runId });
          return { data: { issuedAt: tsToIso(resp.issuedAt) } };
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
          const resp = await server.stopRun({ runId, reason: reason ?? '' });
          return { data: { issuedAt: tsToIso(resp.issuedAt) } };
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
  useListAgentsQuery,
  useResumeMutation,
  usePauseRunMutation,
  useStopRunMutation,
} = castleApi;
