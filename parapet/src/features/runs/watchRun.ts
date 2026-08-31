import type { Dispatch } from '@reduxjs/toolkit';
import { server } from '../../api/client';
import { mapEnvelope } from '../../api/castleApi';
import { runsSlice } from './runsSlice';

export async function startWatch(
  runId: string,
  sinceSeq: number,
  subscriberId: string,
  dispatch: Dispatch,
  signal: AbortSignal,
): Promise<void> {
  try {
    for await (const env of server.watchRun(
      { runId, sinceSeq: BigInt(sinceSeq), subscriberId },
      { signal },
    )) {
      // Skip the WatchReady sentinel — it has no run state.
      if (env.payload?.case === 'watchReady') continue;
      dispatch(runsSlice.actions.eventReceived(mapEnvelope(env)));
    }
  } catch (err) {
    if (signal.aborted) return;
    // Surface to console; the UI already has initial events via listEvents.
    // eslint-disable-next-line no-console
    console.warn('watchRun terminated:', err);
  }
}
