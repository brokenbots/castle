import { ConnectError, server } from './client';
import {
  mapAgent,
  mapEnvelope,
  mapRun,
  mapRunInspection,
  RUNS_PAGE_LIMIT,
  tsToIso,
  type Agent,
  type EventEnvelope,
  type InspectRunArgs,
  type ListRunsArgs,
  type Run,
  type RunInspection,
  type RunsPage,
} from './castleApi';
import { connectCodeName, isUnauthenticatedError } from './errors';
import {
  TERMINAL_EVENT_TYPES,
  type ResumeArgs,
  type RunDataSource,
  type RunStreamArgs,
  type RunStreamEnd,
  type StopRunArgs,
} from './dataSource';

/**
 * The castle implementation of the RunDataSource seam: the Connect client
 * against the Castle server with the console session token, exactly the
 * data path parapet has always used. Envelope/run proto messages are
 * mapped to plain JSON shapes here so the rest of the package never
 * touches generated protobuf types.
 */
export const castleRunDataSource: RunDataSource = {
  async listRuns({ criteriaId = '', status = '', pageToken = '' }: ListRunsArgs): Promise<RunsPage> {
    const resp = await server.listRuns({
      criteriaId,
      status,
      limit: RUNS_PAGE_LIMIT,
      pageToken,
    });
    return { runs: resp.runs.map(mapRun), nextPageToken: resp.nextPageToken };
  },

  async getRun(runId: string): Promise<Run> {
    return mapRun(await server.getRun({ runId }));
  },

  async inspectRun({ runId, sessionId = '' }: InspectRunArgs): Promise<RunInspection> {
    return mapRunInspection(await server.inspectRun({ runId, sessionId }));
  },

  async listRunEvents({ runId, sinceSeq, limit }: ListRunEventsArgs): Promise<RunEventsPage> {
    const resp = await server.listRunEvents({ runId, sinceSeq: BigInt(sinceSeq), limit });
    // next_since_seq is only set on full pages; 0 means "no continuation".
    return {
      events: resp.events.map(mapEnvelope),
      lastSeq: Number(resp.lastSeq),
      nextSinceSeq: resp.nextSinceSeq === 0n ? null : Number(resp.nextSinceSeq),
    };
  },

  async listAgents() {
    const resp = await server.listAgents({});
    return resp.agents.map(mapAgent);
  },

  async getAgent(criteriaId: string) {
    return mapAgent(await server.getAgent({ criteriaId }));
  },

  async connectionStatus(): Promise<void> {
    await server.listAgents({ limit: 1 });
  },

  async resume({ runId, signal, payload }: ResumeArgs) {
    const resp = await server.resumeRun({ runId, signal: signal ?? '', payload: payload ?? {} });
    return { issuedAt: tsToIso(resp.issuedAt) };
  },

  async pauseRun(runId: string) {
    const resp = await server.pauseRun({ runId });
    return { issuedAt: tsToIso(resp.issuedAt) };
  },

  async stopRun({ runId, reason }: StopRunArgs) {
    const resp = await server.stopRun({ runId, reason: reason ?? '' });
    return { issuedAt: tsToIso(resp.issuedAt) };
  },

  async openRunStream(
    { runId, sinceSeq, subscriberId, signal }: RunStreamArgs,
    onEvent: (event: EventEnvelope) => void,
  ): Promise<RunStreamEnd> {
    let terminalSeen = false;
    try {
      for await (const env of server.watchRun(
        { runId, sinceSeq: BigInt(sinceSeq), subscriberId },
        { signal },
      )) {
        const mapped = mapEnvelope(env);
        onEvent(mapped);
        if (TERMINAL_EVENT_TYPES.has(mapped.type)) {
          terminalSeen = true;
          break;
        }
      }
    } catch (err) {
      if (signal.aborted) return { kind: 'clean' };
      if (isUnauthenticatedError(err)) return { kind: 'unauthenticated', message: safeMessage(err) };
      return {
        kind: 'error',
        codeName: err instanceof ConnectError ? connectCodeName(err.code) : undefined,
        message: safeMessage(err),
      };
    }
    return terminalSeen ? { kind: 'terminal' } : { kind: 'clean' };
  },
};

// Re-exported event vocabulary: terminal event types close the stream
// (the seam contract's shared vocabulary between castle and local hosts).
export { TERMINAL_EVENT_TYPES };

function safeMessage(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  return raw.length > 200 ? `${raw.slice(0, 200)}…` : raw;
}