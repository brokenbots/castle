import { useCallback, useEffect, useMemo, useReducer, useRef } from 'react';
import { useDispatch, useSelector } from 'react-redux';
import { server } from '../../../api/client';
import { mapEnvelope, type EventEnvelope } from '../../../api/castleApi';
import { runsSlice, selectRunEvents, selectWatchStatus, type WatchStatus } from '../runsSlice';
import { subscriberIdForSession } from '../subscriberId';
import { startWatch } from '../watchRun';
import {
  anchorRunEventLog,
  EVENT_PAGE_SIZE,
  initialRunEventLogState,
  loadEarlierCursor,
  runEventLogReducer,
  type RunEventLogState,
} from './runEventLog';

export interface RunEventLogView {
  events: EventEnvelope[];
  /** Pagination state of the event log (anchor, hasEarlier, …). */
  log: RunEventLogState;
  loadEarlier: () => void;
  /** Liveness of the watchRun stream for this run. */
  watch: WatchStatus | undefined;
  /** Manually restart the watch stream (after it was reported lost). */
  reconnect: () => void;
}

export function useRunEventLog(runId: string): RunEventLogView {
  const dispatch = useDispatch();
  const [log, dispatchLog] = useReducer(runEventLogReducer, initialRunEventLogState);
  const events = useSelector(selectRunEvents(runId));
  const watch = useSelector(selectWatchStatus(runId));
  const subscriberId = useMemo(() => subscriberIdForSession(), []);

  // Refs so stable callbacks can read the latest state / liveness without
  // being re-created on every render.
  const logRef = useRef(log);
  logRef.current = log;
  const eventsRef = useRef(events);
  eventsRef.current = events;
  const aliveRef = useRef(true);
  const ctrlRef = useRef<AbortController | null>(null);

  useEffect(() => {
    if (!runId) return;
    const ctrl = new AbortController();
    ctrlRef.current = ctrl;
    aliveRef.current = true;

    void anchorRunEventLog(async (since, limit) => {
      const resp = await server.listRunEvents(
        { runId, sinceSeq: BigInt(since), limit },
        { signal: ctrl.signal },
      );
      // next_since_seq is only set on full pages; 0 means "no continuation".
      const nextSinceSeq = resp.nextSinceSeq === 0n ? null : Number(resp.nextSinceSeq);
      return {
        events: resp.events.map(mapEnvelope),
        lastSeq: Number(resp.lastSeq),
        nextSinceSeq,
      };
    })
      .then((outcome) => {
        if (!aliveRef.current) return;
        // Seed the store with everything the walk fetched, in seq order, so
        // derived views see the full history. The store dedupes by seq, so
        // re-dispatching the retained page below is a no-op.
        for (const e of outcome.walked) {
          dispatch(runsSlice.actions.eventReceived(e));
        }
        for (const e of outcome.retained) {
          dispatch(runsSlice.actions.eventReceived(e));
        }
        dispatchLog({
          type: 'anchored',
          anchor: outcome.anchor,
          oldestLoaded: outcome.oldestLoaded,
        });
        // Anchor the watch at the newest loaded seq so the replay does not
        // re-deliver the history pagination controls.
        void startWatch(runId, outcome.anchor, subscriberId, dispatch, ctrl.signal);
      })
      .catch((err) => {
        if (!aliveRef.current || ctrl.signal.aborted) return;
        // Surface to console; the log degrades to the watch replay, as before.
        // eslint-disable-next-line no-console
        console.warn(`listRunEvents failed for run ${runId}:`, err);
        dispatchLog({ type: 'anchorFailed' });
        void startWatch(runId, 0, subscriberId, dispatch, ctrl.signal);
      });

    return () => {
      aliveRef.current = false;
      ctrl.abort();
      dispatch(runsSlice.actions.runCleared(runId));
    };
  }, [runId, dispatch, subscriberId]);

  // Manual reconnect (exposed for the "lost" state's reconnect affordance):
  // resume from the newest delivered seq on the still-current effect signal.
  const reconnect = useCallback(() => {
    const ctrl = ctrlRef.current;
    if (!ctrl || ctrl.signal.aborted || !aliveRef.current) return;
    const lastSeq = eventsRef.current.at(-1)?.seq ?? 0;
    void startWatch(runId, lastSeq, subscriberId, dispatch, ctrl.signal);
  }, [runId, subscriberId, dispatch]);

  const loadEarlier = useCallback(() => {
    const current = logRef.current;
    if (!current.hasEarlier || current.loadingEarlier || current.oldestLoaded === null) return;
    dispatchLog({ type: 'loadEarlierStart' });
    const since = loadEarlierCursor(current);
    void server
      .listRunEvents({ runId, sinceSeq: BigInt(since), limit: EVENT_PAGE_SIZE })
      .then((resp) => {
        if (!aliveRef.current) return;
        const events = resp.events.map(mapEnvelope);
        for (const e of events) dispatch(runsSlice.actions.eventReceived(e));
        dispatchLog({ type: 'earlierPageLoaded', events });
      })
      .catch((err) => {
        if (!aliveRef.current) return;
        // eslint-disable-next-line no-console
        console.warn(`loading earlier events failed for run ${runId}:`, err);
        dispatchLog({ type: 'earlierFailed' });
      });
  }, [runId, dispatch]);

  return { events, log, loadEarlier, watch, reconnect };
}