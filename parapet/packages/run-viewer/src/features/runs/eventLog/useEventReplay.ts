import { useCallback, useEffect, useMemo, useState } from 'react';
import type { EventEnvelope } from '../../../api/castleApi';

/** Delay between replayed steps: each step visibly runs for this long. */
export const REPLAY_STEP_DELAY_MS = 5000;

/** Replay control state: the cursor indexes into the live event list. */
interface ReplayState {
  active: boolean;
  paused: boolean;
  /** Events [0, cursor) are shown. */
  cursor: number;
}

// Step-start index of a 1-based replay step (0-based boundary index).
function stepStart(stepStarts: number[], step: number): number | null {
  if (stepStarts.length === 0) return null;
  const clamped = Math.max(1, Math.min(step, stepStarts.length));
  return stepStarts[clamped - 1];
}

/**
 * Indices of `stepEntered` envelopes — the replay boundaries. A tick moves
 * the cursor to the next boundary, so every tick lands the previous step's
 * outcome/log chunk and the new step's entry: each step then pulses as
 * running (follow pans to it) until the next tick, 5 seconds later.
 */
function stepStartIndices(events: EventEnvelope[]): number[] {
  const indices: number[] = [];
  for (let i = 0; i < events.length; i += 1) {
    if (events[i].type === 'stepEntered') indices.push(i);
  }
  return indices;
}

/**
 * Event-stream replay for offline debugging (agent-first): walks a run's
 * loaded history over the graph instead of against a live run. `start()`
 * pins the view at the first step; a timer advances the cursor to the next
 * `stepEntered` every {@link REPLAY_STEP_DELAY_MS}, so the graph overlay,
 * follow mode, mini-map tints and the event log all animate through the
 * run while the live stream keeps flowing underneath (invisible until
 * `stop()` returns the view to it).
 *
 * The replay only covers the loaded window (the anchor walk's newest page
 * plus whatever "Load earlier" pulled); replaying a fully paged-back log
 * replays the whole run.
 */
export function useEventReplay(events: EventEnvelope[]) {
  const stepStarts = useMemo(() => stepStartIndices(events), [events]);
  const [state, setState] = useState<ReplayState>({ active: false, paused: false, cursor: 0 });
  // Playback speed (ms per step): 5s default, user-tunable live. No manual
  // preset: pausing IS manual mode (any isolating action pauses).
  const [speedMs, setSpeedMs] = useState(REPLAY_STEP_DELAY_MS);

  const nextCursor = useCallback(
    (cursor: number): number | null => {
      if (cursor >= events.length) return null;
      // The window is exclusive at `cursor` (slice end), so a boundary tick
      // lands one past the boundary index: the step's own chunks ride in.
      for (const index of stepStarts) if (index + 1 > cursor) return index + 1;
      // No boundary remains ahead: one final tick to the end completes the
      // last step (its closing events land on the replayed tail).
      return events.length;
    },
    [stepStarts, events.length],
  );

  const start = useCallback(() => {
    setState({
      active: true,
      paused: false,
      cursor: stepStarts.length > 0 ? stepStarts[0] + 1 : events.length,
    });
  }, [stepStarts, events.length]);

  const stop = useCallback(() => {
    setState({ active: false, paused: false, cursor: 0 });
  }, []);

  const pause = useCallback(() => setState((s) => (s.active ? { ...s, paused: true } : s)), []);
  // Un-pause: back to the ticking timeline (the manual Next resumes too).
  const resume = useCallback(() => setState((s) => (s.active ? { ...s, paused: false } : s)), []);

  // Step-through: Next ADVANCES AND RESUMES the ticker (inspection over —
  // back to the timeline); it is also the un-pause affordance.
  const next = useCallback(() => {
    setState((s) => {
      if (!s.active) return s;
      return { ...s, paused: false, cursor: nextCursor(s.cursor) ?? s.cursor };
    });
  }, [nextCursor]);

  // Isolating actions PAUSE the ticker (step inspection mode):
  const prevCursor = useCallback(
    (cursor: number): number | null => {
      // The largest boundary whose exclusive end sits below the cursor.
      for (let i = stepStarts.length - 1; i >= 0; i -= 1) {
        const end = stepStarts[i] + 1;
        if (end < cursor) return end;
      }
      return null;
    },
    [stepStarts],
  );
  const previous = useCallback(() => {
    setState((s) => ({ ...s, paused: true, cursor: prevCursor(s.cursor) ?? s.cursor }));
  }, [prevCursor]);
  // Jump-to-step (the entry box; 1-based): jumps and pauses on that step.
  const jumpToStep = useCallback((step: number) => {
    setState((s) => {
      if (!s.active) return s;
      const start = stepStart(stepStarts, step);
      if (start === null) return s;
      return { ...s, paused: true, cursor: start + 1 };
    });
  }, [stepStarts]);

  const reset = useCallback(() => {
    setState((s) => (s.active ? { ...s, paused: false, cursor: stepStarts.length > 0 ? stepStarts[0] + 1 : events.length } : s));
  }, [stepStarts, events.length]);

  // The tick timer: one boundary per tick at the current speed; stops
  // itself once the cursor has consumed the whole list. A mid-replay
  // speed change reschedules the pending tick without losing the cursor.
  // DOUBLE-STEP GUARD: the fired tick no-ops when the cursor already sits
  // at/past its target (a manual Next consumed it first).
  useEffect(() => {
    if (!state.active || state.paused) return;
    const target = nextCursor(state.cursor);
    if (target === null) return;
    const timer = setTimeout(() => {
      setState((s) => (s.active && !s.paused && s.cursor < target ? { ...s, cursor: target } : s));
    }, speedMs);
    return () => clearTimeout(timer);
  }, [state.active, state.paused, state.cursor, nextCursor, speedMs]);

  const finished = state.active && nextCursor(state.cursor) === null;
  // Steps consumed so far, for the progress readout.
  const consumed = useMemo(() => {
    let count = 0;
    for (const index of stepStarts) if (index + 1 <= state.cursor) count += 1;
    return count;
  }, [stepStarts, state.cursor]);

  return {
    ...state,
    finished,
    speedMs,
    setSpeedMs,
    /** Replayed event window; the untouched live list while inactive. */
    replayed: state.active ? events.slice(0, state.cursor) : events,
    stepCount: stepStarts.length,
    consumed,
    start,
    stop,
    pause,
    resume,
    next,
    previous,
    jumpToStep,
    reset,
  };
}

export type EventReplay = ReturnType<typeof useEventReplay>;