import { useLayoutEffect, useRef } from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';
import type { EventEnvelope } from '../../../api/castleApi';
import { BranchDecisionEntry } from './BranchDecisionEntry';
import { renderPayloadText } from './payloadText';

interface EventLogProps {
  events: EventEnvelope[];
  /** Older pages exist below the loaded window and can be fetched. */
  hasEarlier: boolean;
  loadingEarlier: boolean;
  onLoadEarlier: () => void;
}

const LINE_HEIGHT_PX = 16; // tailwind text-xs line height
const ROW_BASE_PX = 9; // py-1 padding (8px) + 1px border

/**
 * Row height estimate. Payloads vary wildly (one-line heartbeats vs
 * multi-line stepLog chunks vs long single-line JSON), so scale the estimate
 * with hard newlines plus a wrap heuristic for unbroken strings (break-all
 * visually wraps those at the container width).
 */
export function estimateEventHeight(event: EventEnvelope): number {
  const text = renderPayloadText(event);
  const hardLines = text === '' ? 1 : text.split('\n').length;
  const softLines = Math.ceil(text.length / 120);
  const lines = Math.max(1, hardLines, softLines);
  return ROW_BASE_PX + lines * LINE_HEIGHT_PX;
}

export function EventLog({ events, hasEarlier, loadingEarlier, onLoadEarlier }: EventLogProps) {
  const scrollRef = useRef<HTMLDivElement>(null);

  const virtualizer = useVirtualizer({
    count: events.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: (index) => estimateEventHeight(events[index]),
    overscan: 12,
  });

  // Keep the reader's position stable when an older page is prepended: shift
  // the scroll offset by the estimated height of the newly added head rows.
  const firstSeqRef = useRef<number | null>(events[0]?.seq ?? null);
  useLayoutEffect(() => {
    const firstSeq = events[0]?.seq ?? null;
    const prev = firstSeqRef.current;
    firstSeqRef.current = firstSeq;
    const el = scrollRef.current;
    if (!el || firstSeq === null || prev === null || firstSeq >= prev) return;
    let added = 0;
    for (const e of events) {
      if (e.seq >= prev) break;
      added += estimateEventHeight(e);
    }
    el.scrollTop += added;
  }, [events]);

  return (
    <div>
      {hasEarlier && (
        <div className="mb-2">
          <button
            type="button"
            onClick={onLoadEarlier}
            disabled={loadingEarlier}
            className="text-xs font-semibold text-sky-400 hover:text-sky-300 disabled:text-slate-500 disabled:cursor-not-allowed"
          >
            {loadingEarlier ? 'Loading earlier events…' : 'Load earlier events'}
          </button>
        </div>
      )}
      <div
        ref={scrollRef}
        data-testid="event-log-scroll"
        className="font-mono text-xs bg-slate-900 rounded p-3 h-[60vh] overflow-auto"
      >
        <div style={{ height: virtualizer.getTotalSize(), position: 'relative', width: '100%' }}>
          {virtualizer.getVirtualItems().map((row) => {
            const event = events[row.index];
            return (
              <div
                key={event.seq}
                data-testid="event-log-row"
                style={{
                  position: 'absolute',
                  top: 0,
                  left: 0,
                  width: '100%',
                  height: row.size,
                  transform: `translateY(${row.start}px)`,
                }}
                className="border-b border-slate-800/60 py-1 flex gap-3"
              >
                {event.type === 'branchEvaluated' ? (
                  <BranchDecisionRow event={event} />
                ) : (
                  <>
                    <span className="text-slate-500 w-12 shrink-0">#{event.seq}</span>
                    <span className="text-sky-400 w-40 shrink-0">{event.type}</span>
                    <span className="whitespace-pre-wrap break-all min-w-0">
                      {renderPayloadText(event)}
                    </span>
                  </>
                )}
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}

function BranchDecisionRow({ event }: { event: EventEnvelope }) {
  return (
    <div className="flex gap-3 min-w-0">
      <span className="text-slate-500 w-12 shrink-0">#{event.seq}</span>
      <span className="min-w-0">
        <BranchDecisionEntry event={event} />
      </span>
    </div>
  );
}