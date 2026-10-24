import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, test, vi } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import { REPLAY_STEP_DELAY_MS, useEventReplay } from './useEventReplay';

function entered(seq: number, step: string): EventEnvelope {
  return { schemaVersion: 1, runId: 'run-1', seq, type: 'stepEntered', ts: new Date(0).toISOString(), correlationId: '', payload: { step } };
}
function outcome(seq: number, step: string): EventEnvelope {
  return { schemaVersion: 1, runId: 'run-1', seq, type: 'stepOutcome', ts: new Date(0).toISOString(), correlationId: '', payload: { step, outcome: 'success' } };
}

// Two steps: entered(1) outcome(2) entered(3) outcome(4).
const events = [entered(1, 'build'), outcome(2, 'build'), entered(3, 'test'), outcome(4, 'test')];

afterEach(() => {
  vi.useRealTimers();
});

describe('useEventReplay', () => {
  test('idle by default: the replayed window is the untouched live list', () => {
    const { result } = renderHook(() => useEventReplay(events));
    expect(result.current.active).toBe(false);
    expect(result.current.replayed).toBe(events);
  });

  test('start pins the first step; 5s ticks advance cursor to the next stepEntered', () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useEventReplay(events));

    act(() => result.current.start());
    // The view shows only the first step's entry.
    expect(result.current.active).toBe(true);
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1]);
    expect(result.current.consumed).toBe(1);

    // One tick: the first step's outcome lands plus the next entry.
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS);
    });
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1, 2, 3]);
    expect(result.current.consumed).toBe(2);

    // A second tick reaches the tail; the replay has finished (no further
    // ticks scheduled, the cursor stays at the end).
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS);
    });
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1, 2, 3, 4]);
    expect(result.current.finished).toBe(true);

    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS * 3);
    });
    expect(result.current.replayed).toHaveLength(4);
  });

  test('manual next skips ahead one boundary without waiting', () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useEventReplay(events));

    act(() => result.current.start());
    act(() => result.current.next());
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1, 2, 3]);
    act(() => result.current.next());
    expect(result.current.replayed).toHaveLength(4);
    // At the tail, next is a no-op.
    act(() => result.current.next());
    expect(result.current.replayed).toHaveLength(4);
  });

  test('pause freezes the cursor; resume restarts the ticker', () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useEventReplay(events));

    act(() => result.current.start());
    act(() => result.current.pause());
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS * 3);
    });
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1]);
    expect(result.current.paused).toBe(true);

    act(() => result.current.resume());
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS);
    });
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1, 2, 3]);
  });

  test('stop returns the view to the live stream and resets the cursor', () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useEventReplay(events));

    act(() => result.current.start());
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS);
    });
    act(() => result.current.stop());
    expect(result.current.active).toBe(false);
    expect(result.current.replayed).toBe(events);
    expect(result.current.cursor).toBe(0);

    // Restarting begins from the first step again.
    act(() => result.current.start());
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1]);
  });

  test('reset jumps back to the first step mid-replay', () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useEventReplay(events));

    act(() => result.current.start());
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS * 2);
    });
    act(() => result.current.reset());
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1]);
    expect(result.current.paused).toBe(false);
  });

  test('a stream without step boundaries starts finished and never ticks', () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useEventReplay([outcome(1, 'build')]));

    act(() => result.current.start());
    expect(result.current.finished).toBe(true);
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS * 2);
    });
    expect(result.current.replayed).toHaveLength(1);
  });

  test('growing the live list mid-replay leaves the replayed window untouched until the next tick', () => {
    vi.useFakeTimers();
    const { result, rerender } = renderHook(
      ({ list }: { list: EventEnvelope[] }) => useEventReplay(list),
      { initialProps: { list: events } },
    );

    act(() => result.current.start());
    const liveGrown = [...events, entered(5, 'deploy'), outcome(6, 'deploy')];
    rerender({ list: liveGrown });
    // The replay window still stops at the cursor — new live arrivals do
    // not leak into the replayed view.
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1]);

    // Ticks continue through the grown tail. Each tick's timer is only
    // scheduled after the previous tick's state flushes, so advance one
    // tick at a time: first lands the build/test tail events, then
    // deploy's entry, then the final tick past deploy's entry completes
    // the replay.
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS);
    });
    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS);
    });
    expect(result.current.replayed.map((e) => e.seq)).toEqual([1, 2, 3, 4, 5]);
    expect(result.current.finished).toBe(false);

    act(() => {
      vi.advanceTimersByTime(REPLAY_STEP_DELAY_MS);
    });
    expect(result.current.finished).toBe(true);
    expect(result.current.replayed).toHaveLength(6);
  });
});