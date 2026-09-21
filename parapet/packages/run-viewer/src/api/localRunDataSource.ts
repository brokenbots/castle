import type {
  Agent,
  EventEnvelope,
  InspectRunArgs,
  ListRunsArgs,
  Run,
  RunInspection,
  RunsPage,
} from './castleApi';
import { RUNS_PAGE_LIMIT } from './castleApi';
import {
  TERMINAL_EVENT_TYPES,
  type ListRunEventsArgs,
  type ResumeArgs,
  type RunDataSource,
  type RunEventsPage,
  type RunStreamArgs,
  type RunStreamEnd,
  type StopRunArgs,
} from './dataSource';

/**
 * The local implementation of the RunDataSource seam (CRI-257 standalone
 * mode): JSON over HTTP against the CRI-255 loopback control server, which
 * reads criteria run-state files. The loopback contract is the seam's own
 * plain-JSON vocabulary (Run / Agent / EventEnvelope / RunsPage /
 * RunEventsPage / RunInspection) — the same shapes the castle data source
 * maps the Connect wire to — so CRI-255 implements these endpoints without
 * learning a second schema:
 *
 *   GET  {base}/runs?agent=&status=&limit=&cursor=   → RunsPage
 *   GET  {base}/runs/{id}                            → Run
 *   GET  {base}/runs/{id}/inspect?session={id}       → RunInspection
 *   GET  {base}/runs/{id}/events?since_seq=&limit=   → RunEventsPage
 *   POST {base}/runs/{id}/resume|pause|stop          → {issuedAt?}
 *   GET  {base}/agents                               → Agent[]
 *   GET  {base}/agents/{criteriaId}                  → Agent
 *   GET  {base}/health                               → 204
 *
 * There is no run event stream on the loopback yet, so openRunStream polls
 * the events endpoint and replays new events through onEvent until a
 * terminal event lands; the watch reconnect/backoff logic in watchRun keeps
 * working unchanged on top of it. Until CRI-255 ships, every request fails
 * and the run screen degrades to its normal failure states — controls stay
 * disabled via the capability probe, not by hiding them.
 */

/** Base URL of the loopback run-view API (same origin by default). */
export const RUNVIEW_API_BASE: string =
  (import.meta.env?.VITE_RUNVIEW_API_BASE as string | undefined) ?? '/runview/api';

/** Interval between event polls backing openRunStream (ms). */
export const RUNVIEW_POLL_MS: number = Number(
  import.meta.env?.VITE_RUNVIEW_POLL_MS ?? 1_000,
);

/** Joins the API base with a path segment, tolerating a trailing slash. */
function url(base: string, path: string): string {
  return `${base.replace(/\/+$/, '')}/${path.replace(/^\/+/, '')}`;
}

async function request(path: string, init?: RequestInit): Promise<Response> {
  const resp = await fetch(url(RUNVIEW_API_BASE, path), init);
  if (!resp.ok) {
    throw new Error(`runview API ${path}: HTTP ${resp.status}`);
  }
  return resp;
}

async function requestJson<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await request(path, init);
  return (await resp.json()) as T;
}

/** Query string for optional parameters; empty values are omitted. */
function query(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== '') search.set(key, String(value));
  }
  const s = search.toString();
  return s ? `?${s}` : '';
}

export const localRunDataSource: RunDataSource = {
  async listRuns(args: ListRunsArgs): Promise<RunsPage> {
    return requestJson(
      `runs${query({ agent: args.criteriaId, status: args.status, limit: RUNS_PAGE_LIMIT, cursor: args.pageToken })}`,
    );
  },

  getRun(runId: string): Promise<Run> {
    return requestJson(`runs/${encodeURIComponent(runId)}`);
  },

  inspectRun({ runId, sessionId }: InspectRunArgs): Promise<RunInspection> {
    return requestJson(
      `runs/${encodeURIComponent(runId)}/inspect${query({ session: sessionId })}`,
    );
  },

  listRunEvents({ runId, sinceSeq, limit }: ListRunEventsArgs): Promise<RunEventsPage> {
    return requestJson(
      `runs/${encodeURIComponent(runId)}/events${query({ since_seq: sinceSeq, limit })}`,
    );
  },

  listAgents(): Promise<Agent[]> {
    return requestJson('agents');
  },

  getAgent(criteriaId: string): Promise<Agent> {
    return requestJson(`agents/${encodeURIComponent(criteriaId)}`);
  },

  // The loopback server answers 204 when it is up; any other result throws.
  async connectionStatus(): Promise<void> {
    await request('health');
  },

  resume({ runId, signal, payload }: ResumeArgs): Promise<{ issuedAt?: string }> {
    return requestJson(`runs/${encodeURIComponent(runId)}/resume`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ signal: signal ?? '', payload: payload ?? {} }),
    });
  },

  pauseRun(runId: string): Promise<{ issuedAt?: string }> {
    return requestJson(`runs/${encodeURIComponent(runId)}/pause`, { method: 'POST' });
  },

  stopRun({ runId, reason }: StopRunArgs): Promise<{ issuedAt?: string }> {
    return requestJson(`runs/${encodeURIComponent(runId)}/stop`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ reason: reason ?? '' }),
    });
  },

  /**
   * Polls the events endpoint until a terminal event is delivered (or the
   * signal aborts), resolving with the same RunStreamEnd classification the
   * castle stream produces so watchRun's reconnect logic composes unchanged.
   */
  async openRunStream(
    { runId, sinceSeq, signal }: RunStreamArgs,
    onEvent: (event: EventEnvelope) => void,
  ): Promise<RunStreamEnd> {
    let lastSeq = sinceSeq;
    let watchReadySent = false;
    while (!signal.aborted) {
      let page: RunEventsPage;
      try {
        page = await this.listRunEvents({ runId, sinceSeq: lastSeq, limit: RUNS_PAGE_LIMIT });
      } catch (err) {
        if (signal.aborted) return { kind: 'clean' };
        return {
          kind: 'error',
          message: err instanceof Error ? err.message : String(err),
        };
      }
      if (!watchReadySent) {
        onEvent({
          schemaVersion: 1,
          runId,
          seq: lastSeq,
          type: 'watchReady',
          correlationId: '',
          payload: null,
        });
        watchReadySent = true;
      }
      for (const event of page.events) {
        onEvent(event);
        lastSeq = event.seq;
        if (TERMINAL_EVENT_TYPES.has(event.type)) {
          return { kind: 'terminal' };
        }
      }
      // A non-full page means no newer events exist yet; wait for the next
      // poll. A full page may have a continuation, so re-poll immediately
      // to drain the backlog before settling into the polling cadence.
      if (page.events.length >= RUNS_PAGE_LIMIT) continue;
      // The abort may land while a poll is in flight; bail before sleeping.
      if (signal.aborted) return { kind: 'clean' };
      const wait = RUNVIEW_POLL_MS;
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, wait);
        signal.addEventListener('abort', () => {
          clearTimeout(timer);
          resolve();
        }, { once: true });
      });
    }
    return { kind: 'clean' };
  },
};