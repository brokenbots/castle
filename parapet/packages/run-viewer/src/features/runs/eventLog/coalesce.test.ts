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

// criteria.v1.AdapterEvent wire shape: {step, adapter, kind, data}.
function adapterEvent(
  seq: number,
  kind: string,
  data: Record<string, unknown>,
  adapter = 'claude-code',
): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq,
    type: 'adapterEvent',
    ts: new Date(0).toISOString(),
    correlationId: '',
    payload: { step: 'develop', adapter, kind, data },
  };
}

describe('stepLogGroupKey', () => {
  test('keys on the step node; per-chunk correlation ids do not affect grouping', () => {
    // Wire-conformant shape: one step's chunks stream under the same node
    // while every envelope carries a unique transport correlation id.
    expect(stepLogGroupKey(stepLog(1, { step: 'build', correlationId: 'corr-1' }))).toBe('build');
    expect(stepLogGroupKey(stepLog(2, { step: 'build', correlationId: 'corr-2' }))).toBe('build');
  });

  test('falls back to the correlation id when the payload has no step node', () => {
    expect(stepLogGroupKey(stepLog(1, { correlationId: 'corr-1' }))).toBe('corr-1');
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
    const items = coalesceStepLogs([stepLog(1, { step: 'build', correlationId: 'corr-1' })]);
    expect(items).toEqual([{ kind: 'event', event: expect.objectContaining({ seq: 1 }) }]);
  });

  test('groups 500 chunks of one step with distinct correlation ids into a single block', () => {
    // The realistic streaming shape: every chunk of the step carries its
    // own transport correlation id (ids are unique within a run), so only
    // the step node can group them.
    const events = Array.from({ length: 500 }, (_, i) =>
      stepLog(i + 1, { step: 'build', correlationId: `corr-${i + 1}` }),
    );
    const items = coalesceStepLogs(events);

    expect(items).toHaveLength(1);
    const block = items[0];
    expect(block.kind).toBe('stepLogBlock');
    if (block.kind !== 'stepLogBlock') return;
    expect(block.key).toBe('build');
    expect(block.events).toHaveLength(500);
    expect(block.startSeq).toBe(1);
    expect(block.endSeq).toBe(500);
    expect(block.tailText).toBe('chunk 500');
    expect(block.fullText.split('\n')).toHaveLength(500);
  });

  test('groups consecutive chunks of one step into one block spanning their correlation ids', () => {
    const items = coalesceStepLogs([
      stepLog(1, { step: 'build', correlationId: 'corr-1', chunk: 'first' }),
      stepLog(2, { step: 'build', correlationId: 'corr-2', chunk: 'second' }),
      stepLog(3, { step: 'build', correlationId: 'corr-3', chunk: 'third' }),
    ]);

    expect(items).toHaveLength(1);
    const block = items[0];
    expect(block.kind).toBe('stepLogBlock');
    if (block.kind !== 'stepLogBlock') return;
    expect(block.key).toBe('build');
    expect(block.events.map((e) => e.seq)).toEqual([1, 2, 3]);
    expect(block.startSeq).toBe(1);
    expect(block.endSeq).toBe(3);
    expect(block.fullText).toBe('first\nsecond\nthird');
    expect(block.tailText).toBe('third');
  });

  test('groups consecutive chunks by step node when no correlation id is set', () => {
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

  test('does not merge chunks of different steps', () => {
    const items = coalesceStepLogs([
      stepLog(1, { step: 'build', correlationId: 'corr-1' }),
      stepLog(2, { step: 'build', correlationId: 'corr-2' }),
      stepLog(3, { step: 'test', correlationId: 'corr-3' }),
      stepLog(4, { step: 'test', correlationId: 'corr-4' }),
    ]);

    expect(items).toHaveLength(2);
    expect(items[0]).toMatchObject({ kind: 'stepLogBlock', key: 'build', startSeq: 1, endSeq: 2 });
    expect(items[1]).toMatchObject({ kind: 'stepLogBlock', key: 'test', startSeq: 3, endSeq: 4 });
  });

  test('starts a new block when the correlation id changes for step-less chunks', () => {
    // Fallback identity: chunks without a step node can only group by
    // correlation id, so an id change breaks the run.
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
      stepLog(1, { step: 'build', correlationId: 'corr-1', chunk: 'a' }),
      stepLog(2, { step: 'build', correlationId: 'corr-2', chunk: 'b' }),
      other(3),
      stepLog(4, { step: 'build', correlationId: 'corr-4', chunk: 'c' }),
      stepLog(5, { step: 'build', correlationId: 'corr-5', chunk: 'd' }),
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
      stepLog(1, { step: 'build', correlationId: 'corr-1' }),
      stepLog(2, { step: 'build', correlationId: 'corr-2' }),
      stepLog(3, { step: 'build', correlationId: 'corr-3' }),
      stepLog(4, { step: 'build', correlationId: 'corr-4' }),
      stepLog(5, { step: 'build', correlationId: 'corr-5' }),
    ];
    const items = coalesceStepLogs(merged);

    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ kind: 'stepLogBlock', startSeq: 1, endSeq: 5 });
  });

  test('is empty for adapter events that are not delta streams', () => {
    expect(stepLogGroupKey(adapterEvent(1, 'agent.message', { text: 'hello' }))).toBe('');
    expect(stepLogGroupKey(adapterEvent(1, 'adapter.lifecycle.provision_wanted', {}))).toBe('');
  });

  test('keys delta streams on adapter + kind regardless of transport identity', () => {
    // Agent-first deltas carry no step node and a fresh correlation id per
    // fragment; the adapter+kind pair is the stream identity.
    expect(stepLogGroupKey(adapterEvent(1, 'agent.message', { delta: 'a' }))).toBe('adapter:claude-code:agent.message');
  });
});

describe('adapter delta streams', () => {
  test('folds a delta run into one block with the joined message text', () => {
    const items = coalesceStepLogs([
      adapterEvent(1, 'agent.message', { delta: 'Hel' }),
      adapterEvent(2, 'agent.message', { delta: 'lo, ' }),
      adapterEvent(3, 'agent.message', { delta: 'world' }),
    ]);

    expect(items).toHaveLength(1);
    const block = items[0];
    expect(block.kind).toBe('stepLogBlock');
    if (block.kind !== 'stepLogBlock') return;
    expect(block.key).toBe('adapter:claude-code:agent.message');
    expect(block.step).toBe('claude-code · agent.message');
    // Delta fragments are byte-stream continuations: no separators.
    expect(block.fullText).toBe('Hello, world');
    expect(block.tailText).toBe('world');
  });

  test('breaks the stream at a different adapter or kind, and at other events', () => {
    // Streams need 2+ fragments to coalesce (a lone fragment stays a plain
    // row — same rule as stepLog chunks).
    const items = coalesceStepLogs([
      adapterEvent(1, 'agent.message', { delta: 'a' }),
      adapterEvent(2, 'agent.message', { delta: 'a2' }),
      other(3),
      adapterEvent(4, 'agent.message', { delta: 'b' }, 'shell'),
      adapterEvent(5, 'agent.message', { delta: 'b2' }, 'shell'),
      adapterEvent(6, 'tool.invocation', { delta: 'c' }),
      adapterEvent(7, 'tool.invocation', { delta: 'c2' }),
    ]);

    expect(items.map((item) => item.kind)).toEqual(['stepLogBlock', 'event', 'stepLogBlock', 'stepLogBlock']);
    const [head, , shellBlock, toolBlock] = items as [
      { kind: 'stepLogBlock'; fullText: string },
      unknown,
      { kind: 'stepLogBlock'; fullText: string; step: string },
      { kind: 'stepLogBlock'; fullText: string; step: string },
    ];
    // Delta fragments are byte-stream continuations: no separators.
    expect(head.fullText).toBe('aa2');
    expect(shellBlock.fullText).toBe('bb2');
    expect(shellBlock.step).toBe('shell · agent.message');
    expect(toolBlock.fullText).toBe('cc2');
    expect(toolBlock.step).toBe('claude-code · tool.invocation');
  });

  test('a lone delta fragment stays a plain event row', () => {
    const items = coalesceStepLogs([adapterEvent(1, 'agent.message', { delta: 'partial…' })]);
    expect(items.map((item) => item.kind)).toEqual(['event']);
  });
});
