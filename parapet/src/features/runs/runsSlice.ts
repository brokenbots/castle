import { createSelector, createSlice, PayloadAction } from '@reduxjs/toolkit';
import type { EventEnvelope } from '../../api/castleApi';

export interface RunsState {
  events: Record<string, EventEnvelope[]>;
}

const initialState: RunsState = { events: {} };

export const runsSlice = createSlice({
  name: 'runs',
  initialState,
  reducers: {
    eventReceived(state, action: PayloadAction<EventEnvelope>) {
      const env = action.payload;
      const list = state.events[env.runId] ?? [];
      // Castle assigns monotonic seq per run, so the stream is typically
      // already ordered. Handle dedup (replay window) and the rare
      // out-of-order arrival with an ordered insert — avoid an O(n log n)
      // sort on every event.
      let i = list.length;
      while (i > 0 && list[i - 1].seq > env.seq) i--;
      if (i > 0 && list[i - 1].seq === env.seq) return;
      list.splice(i, 0, env);
      state.events[env.runId] = list;
    },
    runCleared(state, action: PayloadAction<string>) {
      delete state.events[action.payload];
    },
  },
});

export const selectRunEvents = (runId: string) => (state: { runs: RunsState }) =>
  state.runs.events[runId] ?? EMPTY;

export const selectPauseState = (runId: string) =>
  createSelector(
    [(state: { runs: RunsState }) => state.runs.events[runId] ?? EMPTY],
    (events) => {
      // Walk events backwards to find the most recent pause-related event
      for (let i = events.length - 1; i >= 0; i--) {
        const event = events[i];
        
        if (event.type === 'waitEntered') {
          return { isPaused: true, pauseEvent: event };
        }
        
        if (event.type === 'approvalRequested') {
          return { isPaused: true, pauseEvent: event };
        }
        
        // Resume events clear the pause state
        if (event.type === 'waitResumed' || event.type === 'approvalDecision') {
          return { isPaused: false, pauseEvent: null };
        }
      }
      
      return { isPaused: false, pauseEvent: null };
    }
  );

const EMPTY: EventEnvelope[] = [];
