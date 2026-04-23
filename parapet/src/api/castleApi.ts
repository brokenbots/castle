import { createApi, fetchBaseQuery } from '@reduxjs/toolkit/query/react';

export interface Run {
  ID: string;
  OverseerID: string;
  WorkflowName: string;
  Status: string;
  CurrentStep: string;
  LastSeq: number;
  CreatedAt: string;
  EndedAt?: string | null;
}

export interface Overseer {
  id: string;
  name: string;
  hostname?: string;
  version?: string;
  status: string;
  last_seen_at: string;
}

export interface EventEnvelope {
  schema_version: number;
  run_id: string;
  seq: number;
  type: string;
  ts: string;
  correlation_id?: string;
  payload: unknown;
}

export const castleApi = createApi({
  reducerPath: 'castleApi',
  baseQuery: fetchBaseQuery({ baseUrl: '/api/v0/' }),
  tagTypes: ['Run', 'Overseer', 'Events'],
  endpoints: (b) => ({
    listRuns: b.query<Run[], void>({
      query: () => 'runs',
      providesTags: ['Run'],
    }),
    getRun: b.query<Run, string>({
      query: (id) => `runs/${id}`,
      providesTags: (_r, _e, id) => [{ type: 'Run', id }],
    }),
    listOverseers: b.query<Overseer[], void>({
      query: () => 'overseers',
      providesTags: ['Overseer'],
    }),
    listEvents: b.query<EventEnvelope[], { runId: string; since?: number }>({
      query: ({ runId, since = 0 }) => `runs/${runId}/events?since=${since}`,
      providesTags: (_r, _e, { runId }) => [{ type: 'Events', id: runId }],
    }),
  }),
});

export const {
  useListRunsQuery,
  useGetRunQuery,
  useListOverseersQuery,
  useListEventsQuery,
} = castleApi;
