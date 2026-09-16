import { describe, expect, test } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import {
  coalesceStepLogs,
  itemEndSeq,
  itemStartSeq,
  stepLogGroupKey,
} from './coalesce';

function stepLog(seq: number, opts: { correlationId?: string; step?: string; chunk?: string } = {}): EventEnvelope {
  const { correlationId = '', step, chunk = `chunk ${seq}` } = opts;
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq,
    type: 'stepLog',
    ts: new Date(0).toISOString(),
    correlationId,
    payload: step === undefined ? { chunk } : { step, chunk },
  };
}

function other(seq: number, type = 'runStatus'): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq,
    type,
    ts: new Date(0).toISOString(),
    correlationId: '',
    payload: { detail: `event ${seq}` },
  };
}

describe('stepLogGroupKey', () => {
  test('prefers the correlation id over the step node', () => {
    expect(stepLogGroupKey(stepLog(1, { correlationId: 'corr-1', step: 'build' }))).toBe('corr-1');
  });

  test('falls back to the step node when no correlation id is set', () => {
    expect(stepLogGroupKey(stepLog(1, { step: 'build' }))).toBe('build');
  });

  test('is empty for chunks without any identity', () => {
    expect(stepLogGroupKey(stepLog(1))).toBe('');
  });

  test('is empty for non-stepLog events', () => {
    expect(stepLogGroupKey(other(1))).toBe('');
  });
});

describe('coalesceStepLogs', () => {
  test('returns an empty list for no events', () => {
    expect(coalesceStepLogs([])).toEqual([]);
  });

  test('keeps a lone chunk as a plain event row', () => {
    const items = coalesceStepLogs([stepLog(1, { correlationId: 'corr-1' })]);
    expect(items).toEqual([{ kind: 'event', event: expect.objectContaining({ seq: 1 }) }]);
  });

  test('groups consecutive chunks with the same correlation id into one block', () => {
    const items = coalesceStepLogs([
      stepLog(1, { correlationId: 'corr-1', chunk: 'first' }),
      stepLog(2, { correlationId: 'corr-1', chunk: 'second' }),
      stepLog(3, { correlationId: 'corr-1', chunk: 'third' }),
    ]);

    expect(items).toHaveLength(1);
    const block = items[0];
    expect(block.kind).toBe('stepLogBlock');
    if (block.kind !== 'stepLogBlock') return;
    expect(block.key).toBe('corr-1');
    expect(block.events.map((e) => e.seq)).toEqual([1, 2, 3]);
    expect(block.startSeq).toBe(1);
    expect(block.endSeq).toBe(3);
    expect(block.fullText).toBe('first\nsecond\nthird');
    expect(block.tailText).toBe('third');
  });

  test('groups consecutive chunks by step node when correlation id is empty', () => {
    const items = coalesceStepLogs([
      stepLog(1, { step: 'build', chunk: 'a' }),
      stepLog(2, { step: 'build', chunk: 'b' }),
      stepLog(3, { step: 'build', chunk: 'c' }),
    ]);

    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      kind: 'stepLogBlock',
      key: 'build',
      step: 'build',
      startSeq: 1,
      endSeq: 3,
      tailText: 'c',
    });
  });

  test('starts a new block when the correlation id changes', () => {
    const items = coalesceStepLogs([
      stepLog(1, { correlationId: 'corr-1' }),
      stepLog(2, { correlationId: 'corr-1' }),
      stepLog(3, { correlationId: 'corr-2' }),
      stepLog(4, { correlationId: 'corr-2' }),
    ]);

    expect(items).toHaveLength(2);
    expect(items[0]).toMatchObject({ kind: 'stepLogBlock', key: 'corr-1', startSeq: 1, endSeq: 2 });
    expect(items[1]).toMatchObject({ kind: 'stepLogBlock', key: 'corr-2', startSeq: 3, endSeq: 4 });
  });

  test('breaks runs at non-stepLog events so they interleave chronologically', () => {
    const items = coalesceStepLogs([
      stepLog(1, { correlationId: 'corr-1', chunk: 'a' }),
      stepLog(2, { correlationId: 'corr-1', chunk: 'b' }),
      other(3),
      stepLog(4, { correlationId: 'corr-1', chunk: 'c' }),
      stepLog(5, { correlationId: 'corr-1', chunk: 'd' }),
    ]);

    expect(items.map((item) => item.kind)).toEqual(['stepLogBlock', 'event', 'stepLogBlock']);
    const [head, , tailBlock] = items;
    if (head.kind !== 'stepLogBlock' || tailBlock.kind !== 'stepLogBlock') return;
    expect(head.fullText).toBe('a\nb');
    expect(tailBlock.fullText).toBe('c\nd');
    expect(itemStartSeq(items[0])).toBe(1);
    expect(itemEndSeq(items[0])).toBe(2);
    expect(itemStartSeq(tailBlock)).toBe(4);
    expect(itemEndSeq(tailBlock)).toBe(5);
  });

  test('never coalesces chunks without any identity', () => {
    const items = coalesceStepLogs([
      stepLog(1, { chunk: 'a' }),
      stepLog(2, { chunk: 'b' }),
      stepLog(3, { chunk: 'c' }),
    ]);

    expect(items.map((item) => item.kind)).toEqual(['event', 'event', 'event']);
  });

  test('coalesces across a page boundary in one call on the merged list', () => {
    // An older page (seq 1..3) is prepended in front of the previously
    // loaded tail (seq 4..5); coalescing runs on the merged seq-ordered
    // list, so the block spans the old page boundary.
    const merged = [
      stepLog(1, { correlationId: 'corr-1' }),
      stepLog(2, { correlationId: 'corr-1' }),
      stepLog(3, { correlationId: 'corr-1' }),
      stepLog(4, { correlationId: 'corr-1' }),
      stepLog(5, { correlationId: 'corr-1' }),
    ];
    const items = coalesceStepLogs(merged);

    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ kind: 'stepLogBlock', startSeq: 1, endSeq: 5 });
  });
});