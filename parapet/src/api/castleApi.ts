import { createApi } from '@reduxjs/toolkit/query/react';
import { fakeBaseQuery } from '@reduxjs/toolkit/query';
import { ConnectError } from '@connectrpc/connect';
import { Timestamp } from '@bufbuild/protobuf';
import { castle, overseer } from './client';
import type { Run as PbRun } from '../sdk/overseer';
import type { Overseer as PbOverseer } from '../gen/overlord/v1/castle_pb';
import type { Envelope } from '../sdk/overseer';

export interface Run {
  runId: string;
  overseerId: string;
  workflowName: string;
  workflowHash: string;
  status: string;
  createdAt?: string;
  startedAt?: string;
  endedAt?: string;
  finalState: string;
  failureReason: string;
}

export interface Overseer {
  overseerId: string;
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

function mapRun(r: PbRun): Run {
  return {
    runId: r.runId,
    overseerId: r.overseerId,
    workflowName: r.workflowName,
    workflowHash: r.workflowHash,
    status: r.status,
    createdAt: tsToIso(r.createdAt),
    startedAt: tsToIso(r.startedAt),
    endedAt: tsToIso(r.endedAt),
    finalState: r.finalState,
    failureReason: r.failureReason,
  };
}

function mapOverseer(o: PbOverseer): Overseer {
  return {
    overseerId: o.overseerId,
    name: o.name,
    labels: { ...o.labels },
    status: o.status,
    registeredAt: tsToIso(o.registeredAt),
    lastSeenAt: tsToIso(o.lastSeenAt),
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

function toError(err: unknown) {
  if (err instanceof ConnectError) {
    return { status: err.code, data: err.rawMessage };
  }
  return { status: 'CUSTOM_ERROR', data: err instanceof Error ? err.message : String(err) };
}

export const castleApi = createApi({
  reducerPath: 'castleApi',
  baseQuery: fakeBaseQuery<{ status: string | number; data: string }>(),
  tagTypes: ['Run', 'Overseer', 'Events'],
  endpoints: (b) => ({
    listRuns: b.query<Run[], void>({
      queryFn: async () => {
        try {
          const resp = await castle.listRuns({});
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
          const resp = await castle.getRun({ runId });
          return { data: mapRun(resp) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, id) => [{ type: 'Run', id }],
    }),
    listOverseers: b.query<Overseer[], void>({
      queryFn: async () => {
        try {
          const resp = await castle.listOverseers({});
          return { data: resp.overseers.map(mapOverseer) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: ['Overseer'],
    }),
    listEvents: b.query<EventEnvelope[], { runId: string; since?: number }>({
      queryFn: async ({ runId, since = 0 }) => {
        try {
          const resp = await castle.listRunEvents({
            runId,
            sinceSeq: BigInt(since),
          });
          return { data: resp.events.map(mapEnvelope) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, { runId }) => [{ type: 'Events', id: runId }],
    }),
    resume: b.mutation<
      { accepted: boolean; reason: string },
      { runId: string; signal: string; payload?: Record<string, string> }
    >({
      queryFn: async ({ runId, signal, payload = {} }) => {
        try {
          const resp = await overseer.resume({ runId, signal, payload });
          return { data: { accepted: resp.accepted, reason: resp.reason } };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { runId }) => [
        { type: 'Run', id: runId },
        { type: 'Events', id: runId },
      ],
    }),
  }),
});

export const {
  useListRunsQuery,
  useGetRunQuery,
  useListOverseersQuery,
  useListEventsQuery,
  useResumeMutation,
} = castleApi;
