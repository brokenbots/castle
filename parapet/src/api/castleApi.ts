import { createApi, fetchBaseQuery } from '@reduxjs/toolkit/query/react';
import { getAuthToken } from '../authToken';

export interface Run {
  ID: string;
  OverseerID: string;
  WorkflowName: string;
  WorkflowHCL: string;
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
  baseQuery: fetchBaseQuery({
    baseUrl: '/api/v0/',
    prepareHeaders: (headers) => {
      const token = getAuthToken();
      if (token) {
        headers.set('X-Overseer-Token', token);
      }
      return headers;
    },
  }),
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
      async onCacheEntryAdded(arg, { updateCachedData, cacheDataLoaded, cacheEntryRemoved }) {
        await cacheDataLoaded;

        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const token = getAuthToken();
        const params = new URLSearchParams();
        if (token) {
          params.set('token', token);
        }
        const query = params.toString();
        const wsURL = `${protocol}//${window.location.host}/api/v0/runs/${arg.runId}/stream${query ? `?${query}` : ''}`;
        const ws = new WebSocket(wsURL);

        ws.onmessage = (event) => {
          const incoming = JSON.parse(event.data) as EventEnvelope;
          updateCachedData((draft) => {
            if (!draft.find((e) => e.seq === incoming.seq)) {
              draft.push(incoming);
            }
          });
        };

        await cacheEntryRemoved;
        ws.close();
      },
    }),
  }),
});

export const {
  useListRunsQuery,
  useGetRunQuery,
  useListOverseersQuery,
  useListEventsQuery,
} = castleApi;
