import type { EventEnvelope } from '../../../api/castleApi';
import { renderPayloadText } from './payloadText';

/**
 * A single non-coalesced event, rendered as one row like before.
 */
export interface EventItem {
  kind: 'event';
  event: EventEnvelope;
}

/**
 * A run of consecutive stepLog chunks belonging to the same step execution,
 * rendered as one collapsible row. `fullText` is every chunk joined with
 * newlines (shown when expanded); `tailText` is the newest chunk (shown
 * collapsed, so the block previews the tail of the output).
 */
export interface StepLogBlock {
  kind: 'stepLogBlock';
  /** Grouping key: correlation id, or the stepLog's step node. */
  key: string;
  /** Node name of the step, when the payloads carry one. */
  step: string;
  events: EventEnvelope[];
  startSeq: number;
  endSeq: number;
  fullText: string;
  tailText: string;
}

export type EventLogItem = EventItem | StepLogBlock;

/**
 * Identity of the step execution a stepLog chunk belongs to. The envelope
 * correlation id wins when present; otherwise the chunk's step node names
 * the execution. An empty key means the chunk carries no identity at all,
 * so it is never coalesced — merging unidentifiable chunks could fuse
 * output of unrelated steps.
 */
export function stepLogGroupKey(e: EventEnvelope): string {
  if (e.type !== 'stepLog') return '';
  const step = (e.payload as { step?: string } | undefined)?.step ?? '';
  return e.correlationId || step;
}

/**
 * Groups consecutive stepLog events with the same step execution identity
 * into single block items. A run shorter than two chunks stays a plain
 * event row (coalescing exists to stop flooding, and one chunk renders
 * fully without a toggle). Runs break at any non-stepLog event, at a stepLog
 * with a different identity, and at a stepLog without identity.
 *
 * Grouping is position-based on the seq-ordered event list, so chunks that
 * span event-log page loads still form one block once both pages are loaded.
 */
export function coalesceStepLogs(events: EventEnvelope[]): EventLogItem[] {
  const items: EventLogItem[] = [];
  let pending: EventEnvelope[] = [];

  const flushPending = () => {
    if (pending.length === 0) return;
    if (pending.length === 1) {
      items.push({ kind: 'event', event: pending[0] });
    } else {
      const first = pending[0];
      const last = pending[pending.length - 1];
      items.push({
        kind: 'stepLogBlock',
        key: stepLogGroupKey(first),
        step: blockStep(first),
        events: pending,
        startSeq: first.seq,
        endSeq: last.seq,
        fullText: pending.map(renderPayloadText).join('\n'),
        tailText: renderPayloadText(last),
      });
    }
    pending = [];
  };

  for (const e of events) {
    const key = stepLogGroupKey(e);
    if (key && pending.length > 0 && stepLogGroupKey(pending[pending.length - 1]) === key) {
      pending.push(e);
      continue;
    }
    flushPending();
    if (key) pending = [e];
    else items.push({ kind: 'event', event: e });
  }
  flushPending();
  return items;
}

export function itemStartSeq(item: EventLogItem): number {
  return item.kind === 'event' ? item.event.seq : item.startSeq;
}

export function itemEndSeq(item: EventLogItem): number {
  return item.kind === 'event' ? item.event.seq : item.endSeq;
}

function blockStep(e: EventEnvelope): string {
  return (e.payload as { step?: string } | undefined)?.step ?? '';
}