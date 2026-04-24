import { describe, expect, test } from 'vitest';
import { runsSlice, selectRunEvents, type RunsState } from './runsSlice';
import type { EventEnvelope } from '../../api/castleApi';

function makeEnv(runId: string, seq: number, type = 'stepLog'): EventEnvelope {
  return {
    schemaVersion: 1,
    runId,
    seq,
    type,
    ts: new Date(0).toISOString(),
    correlationId: '',
    payload: { chunk: `#${seq}` },
  };
}

function run(events: EventEnvelope[]): RunsState {
  let state: RunsState = { events: {} };
  for (const e of events) {
    state = runsSlice.reducer(state, runsSlice.actions.eventReceived(e));
  }
  return state;
}

describe('runsSlice.eventReceived', () => {
  test('appends events in order', () => {
    const state = run([makeEnv('r1', 1), makeEnv('r1', 2), makeEnv('r1', 3)]);
    expect(state.events.r1.map((e) => e.seq)).toEqual([1, 2, 3]);
  });

  test('deduplicates on seq', () => {
    const state = run([
      makeEnv('r1', 1),
      makeEnv('r1', 2),
      makeEnv('r1', 2),
      makeEnv('r1', 1),
    ]);
    expect(state.events.r1.map((e) => e.seq)).toEqual([1, 2]);
  });

  test('orders out-of-order arrivals', () => {
    const state = run([
      makeEnv('r1', 3),
      makeEnv('r1', 1),
      makeEnv('r1', 4),
      makeEnv('r1', 2),
    ]);
    expect(state.events.r1.map((e) => e.seq)).toEqual([1, 2, 3, 4]);
  });

  test('keeps events for different runs isolated', () => {
    const state = run([makeEnv('r1', 1), makeEnv('r2', 1), makeEnv('r1', 2)]);
    expect(state.events.r1.map((e) => e.seq)).toEqual([1, 2]);
    expect(state.events.r2.map((e) => e.seq)).toEqual([1]);
  });
});

describe('runsSlice.runCleared', () => {
  test('drops events for the cleared run', () => {
    const primed = run([makeEnv('r1', 1), makeEnv('r2', 1)]);
    const cleared = runsSlice.reducer(primed, runsSlice.actions.runCleared('r1'));
    expect(cleared.events.r1).toBeUndefined();
    expect(cleared.events.r2?.map((e) => e.seq)).toEqual([1]);
  });
});

describe('selectRunEvents', () => {
  test('returns a stable empty reference when a run is unknown', () => {
    const state = { runs: { events: {} } };
    const a = selectRunEvents('missing')(state);
    const b = selectRunEvents('missing')(state);
    expect(a).toBe(b);
    expect(a).toEqual([]);
  });
});
