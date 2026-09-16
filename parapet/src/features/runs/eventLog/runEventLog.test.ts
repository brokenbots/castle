import { describe, expect, test } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import {
  anchorRunEventLog,
  EVENT_PAGE_SIZE,
  initialRunEventLogState,
  loadEarlierCursor,
  runEventLogReducer,
  type FetchEventPage,
} from './runEventLog';

function makeEnv(seq: number): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'r1',
    seq,
    type: 'stepLog',
    ts: new Date(0).toISOString(),
    correlationId: '',
    payload: { chunk: `chunk ${seq}` },
  };
}


function fakeServer(total: number) {
  const calls: { since: number; limit: number }[] = [];
  const fetchPage: FetchEventPage = async (sinceSeq, limit) => {
    calls.push({ since: sinceSeq, limit });
    if (sinceSeq >= total) return { events: [], lastSeq: 0, nextSinceSeq: null };
    const events: EventEnvelope[] = [];
    let seq = sinceSeq + 1;
    while (seq <= total && events.length < limit) {
      events.push(makeEnv(seq));
      seq += 1;
    }
    return {
      events,
      lastSeq: events[events.length - 1].seq,
      nextSinceSeq: events.length === limit ? events[events.length - 1].seq : null,
    };
  };
  return { calls, fetchPage };
}

describe('anchorRunEventLog', () => {
  test('seeds every walked event and retains the tail page of a multi-page run', async () => {
    const { calls, fetchPage } = fakeServer(1000);
    const outcome = await anchorRunEventLog(fetchPage);

    // Every event fetched while walking is handed back, in seq order —
    // not just the tail.
    expect(outcome.walked.map((e) => e.seq)).toEqual(
      Array.from({ length: 1000 }, (_, i) => i + 1),
    );
    // Anchor semantics are unchanged: they still come from the retained
    // tail page, not the walked history.
    expect(outcome.anchor).toBe(1000);
    expect(outcome.oldestLoaded).toBe(501);
    expect(outcome.retained.map((e) => e.seq)).toEqual(
      Array.from({ length: 500 }, (_, i) => i + 501),
    );
    // Walked 1-500, then 501-1000 (full page: continuation), then the
    // final probe at 1000 returns empty and the staged page is retained.
    expect(calls.map((c) => c.since)).toEqual([0, 500, 1000]);
    expect(calls.every((c) => c.limit === EVENT_PAGE_SIZE)).toBe(true);
  });

  test('retains the last full page when the tail probe is empty', async () => {
    // Exactly 1500 events: the walk ends with an empty page at since=1500.
    const { calls, fetchPage } = fakeServer(1500);
    const outcome = await anchorRunEventLog(fetchPage);

    expect(outcome.anchor).toBe(1500);
    expect(outcome.oldestLoaded).toBe(1001);
    expect(outcome.retained.map((e) => e.seq)).toEqual(
      Array.from({ length: 500 }, (_, i) => i + 1001),
    );
    // No truncation across multiple walked pages: the walk accumulates all
    // 1500 events in order without duplicates.
    expect(outcome.walked.map((e) => e.seq)).toEqual(
      Array.from({ length: 1500 }, (_, i) => i + 1),
    );
    expect(new Set(outcome.walked.map((e) => e.seq)).size).toBe(1500);
    expect(calls.map((c) => c.since)).toEqual([0, 500, 1000, 1500]);
  });

  test('keeps a single non-full page without walking', async () => {
    const { calls, fetchPage } = fakeServer(123);
    const outcome = await anchorRunEventLog(fetchPage);

    expect(outcome.anchor).toBe(123);
    expect(outcome.oldestLoaded).toBe(1);
    expect(outcome.retained.map((e) => e.seq)).toEqual(
      Array.from({ length: 123 }, (_, i) => i + 1),
    );
    // A single non-full page: walked equals retained.
    expect(outcome.walked.map((e) => e.seq)).toEqual(
      Array.from({ length: 123 }, (_, i) => i + 1),
    );
    expect(calls.map((c) => c.since)).toEqual([0]);
  });

  test('returns a zero anchor when the run has no events', async () => {
    const { calls, fetchPage } = fakeServer(0);
    const outcome = await anchorRunEventLog(fetchPage);

    expect(outcome).toEqual({ anchor: 0, oldestLoaded: null, retained: [], walked: [] });
    expect(calls).toEqual([{ since: 0, limit: EVENT_PAGE_SIZE }]);
  });

  test('terminates when the server echoes a stale continuation', async () => {
    // Defensive: a next_since_seq <= since must not loop forever.
    let calls = 0;
    const outcome = await anchorRunEventLog(async () => {
      calls += 1;
      return { events: [makeEnv(1)], lastSeq: 1, nextSinceSeq: 0 };
    });
    expect(calls).toBe(1);
    expect(outcome.anchor).toBe(1);
    expect(outcome.oldestLoaded).toBe(1);
    expect(outcome.walked.map((e) => e.seq)).toEqual([1]);
  });
});

describe('runEventLogReducer', () => {
  test('anchored sets hasEarlier only when history is known to exist', () => {
    let state = runEventLogReducer(initialRunEventLogState, {
      type: 'anchored',
      anchor: 1000,
      oldestLoaded: 501,
    });
    expect(state).toMatchObject({ anchor: 1000, oldestLoaded: 501, hasEarlier: true });

    state = runEventLogReducer(state, { type: 'anchored', anchor: 500, oldestLoaded: 1 });
    expect(state).toMatchObject({ anchor: 500, oldestLoaded: 1, hasEarlier: false });

    state = runEventLogReducer(initialRunEventLogState, {
      type: 'anchored',
      anchor: 0,
      oldestLoaded: null,
    });
    expect(state).toMatchObject({ anchor: 0, oldestLoaded: null, hasEarlier: false });
  });

  test('anchorFailed degrades to watch replay without pagination', () => {
    let state = runEventLogReducer(initialRunEventLogState, {
      type: 'anchored',
      anchor: 1000,
      oldestLoaded: 501,
    });
    state = runEventLogReducer(state, { type: 'anchorFailed' });
    expect(state).toMatchObject({ anchor: 0, hasEarlier: false });
  });

  test('earlierPageLoaded tracks the oldest seq and ends pagination at seq 1', () => {
    let state = runEventLogReducer(initialRunEventLogState, {
      type: 'anchored',
      anchor: 1000,
      oldestLoaded: 1001,
    });
    state = runEventLogReducer(state, { type: 'loadEarlierStart' });
    expect(state.loadingEarlier).toBe(true);

    state = runEventLogReducer(state, {
      type: 'earlierPageLoaded',
      events: Array.from({ length: 500 }, (_, i) => makeEnv(i + 501)),
    });
    expect(state).toMatchObject({ oldestLoaded: 501, hasEarlier: true, loadingEarlier: false });

    state = runEventLogReducer(state, { type: 'earlierPageLoaded', events: [makeEnv(1), makeEnv(2)] });
    expect(state).toMatchObject({ oldestLoaded: 1, hasEarlier: false, loadingEarlier: false });
  });

  test('earlierPageLoaded with an empty page ends pagination', () => {
    let state = runEventLogReducer(initialRunEventLogState, {
      type: 'anchored',
      anchor: 1000,
      oldestLoaded: 700,
    });
    state = runEventLogReducer(state, { type: 'earlierPageLoaded', events: [] });
    expect(state).toMatchObject({ hasEarlier: false, loadingEarlier: false, oldestLoaded: 700 });
  });

  test('earlierFailed clears loadingEarlier and keeps the control retryable', () => {
    let state = runEventLogReducer(
      { ...initialRunEventLogState, anchor: 1000, oldestLoaded: 501, hasEarlier: true },
      { type: 'loadEarlierStart' },
    );
    state = runEventLogReducer(state, { type: 'earlierFailed' });
    expect(state).toMatchObject({ hasEarlier: true, loadingEarlier: false });
  });

  test('loadEarlierStart is a no-op without earlier pages or while loading', () => {
    let state = runEventLogReducer(initialRunEventLogState, { type: 'loadEarlierStart' });
    expect(state.loadingEarlier).toBe(false);

    state = runEventLogReducer({ ...initialRunEventLogState, hasEarlier: true }, { type: 'loadEarlierStart' });
    expect(state.loadingEarlier).toBe(true);
    state = runEventLogReducer(state, { type: 'loadEarlierStart' });
    expect(state.loadingEarlier).toBe(true);
  });
});

describe('loadEarlierCursor', () => {
  test('seeks one page below the oldest loaded seq', () => {
    expect(loadEarlierCursor({ ...initialRunEventLogState, oldestLoaded: 1001 })).toBe(500);
    expect(loadEarlierCursor({ ...initialRunEventLogState, oldestLoaded: 501 })).toBe(0);
    expect(loadEarlierCursor({ ...initialRunEventLogState, oldestLoaded: 201 })).toBe(0);
    expect(loadEarlierCursor(initialRunEventLogState)).toBe(0);
  });
});

