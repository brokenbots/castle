import { useCallback, useEffect, useLayoutEffect, useMemo, useReducer, useRef, useState } from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';
import type { EventEnvelope } from '../../../api/castleApi';
import { BranchDecisionEntry } from './BranchDecisionEntry';
import { renderPayloadText } from './payloadText';
import {
  coalesceStepLogs,
  itemEndSeq,
  itemStartSeq,
  type EventLogItem,
  type StepLogBlock,
} from './coalesce';
import { initialTailState, tailReducer } from './tail';

interface EventLogProps {
  events: EventEnvelope[];
  /** Whether the run is currently executing; drives the live-tail affordances. */
  running: boolean;
  /** Older pages exist below the loaded window and can be fetched. */
  hasEarlier: boolean;
  loadingEarlier: boolean;
  onLoadEarlier: () => void;
}

const LINE_HEIGHT_PX = 16; // tailwind text-xs line height
const ROW_BASE_PX = 9; // py-1 padding (8px) + 1px border
const SOFT_WRAP_CHARS_PER_LINE = 120;
/** Distance from the bottom within which the view counts as pinned. */
const BOTTOM_THRESHOLD_PX = 48;

function textLines(text: string): number {
  const hardLines = text === '' ? 1 : text.split('\n').length;
  const softLines = Math.ceil(text.length / SOFT_WRAP_CHARS_PER_LINE);
  return Math.max(1, hardLines, softLines);
}

/**
 * Row height estimate. Payloads vary wildly (one-line heartbeats vs
 * multi-line stepLog chunks vs long single-line JSON), so scale the estimate
 * with hard newlines plus a wrap heuristic for unbroken strings (break-all
 * visually wraps those at the container width).
 */
export function estimateEventHeight(event: EventEnvelope): number {
  return ROW_BASE_PX + textLines(renderPayloadText(event)) * LINE_HEIGHT_PX;
}

/**
 * Height estimate of a log item (plain event row or stepLog block). A block
 * adds one header line; its payload text is the tail when collapsed and the
 * full output when expanded.
 */
export function estimateItemHeight(item: EventLogItem, expanded: boolean): number {
  if (item.kind === 'event') return estimateEventHeight(item.event);
  return ROW_BASE_PX + (1 + textLines(expanded ? item.fullText : item.tailText)) * LINE_HEIGHT_PX;
}

/**
 * Scroll offset to add after older events are prepended, so the reader's
 * position stays anchored at the previously-first row. Items that now end
 * entirely above that row are new content; the item containing it is a
 * boundary: for a block, the content above the old first row is the block
 * header (only when the row was not already a block header — an absorbed
 * block keeps its header) plus the chunks that moved in front of it.
 */
function estimatePrependShift(
  prevFirstSeq: number,
  prevFirstWasBlock: boolean,
  items: EventLogItem[],
  expandedKeys: ReadonlySet<string>,
): number {
  let shift = 0;
  for (const item of items) {
    if (itemEndSeq(item) < prevFirstSeq) {
      const expanded = item.kind === 'stepLogBlock' && expandedKeys.has(item.key);
      shift += estimateItemHeight(item, expanded);
      continue;
    }
    if (item.kind === 'event') break;
    const boundaryIndex = item.events.findIndex((e) => e.seq === prevFirstSeq);
    if (boundaryIndex > 0) {
      if (!prevFirstWasBlock) shift += LINE_HEIGHT_PX;
      for (let i = 0; i < boundaryIndex; i++) shift += estimateEventHeight(item.events[i]);
    }
    break;
  }
  return shift;
}

export function EventLog({ events, running, hasEarlier, loadingEarlier, onLoadEarlier }: EventLogProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const items = useMemo(() => coalesceStepLogs(events), [events]);
  const [expandedKeys, setExpandedKeys] = useState<ReadonlySet<string>>(() => new Set());
  const [tail, dispatch] = useReducer(tailReducer, undefined, initialTailState);

  const isBlockExpanded = useCallback(
    (item: EventLogItem) => item.kind === 'stepLogBlock' && expandedKeys.has(item.key),
    [expandedKeys],
  );

  const scrollToBottom = useCallback(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, []);

  const handleScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_THRESHOLD_PX;
    dispatch({ type: atBottom ? 'scrolledAtBottom' : 'scrolledUp' });
  }, []);

  // Listen natively so the handler also fires for dispatched scroll events
  // (React delegates scroll specially, and jsdom never fires it implicitly).
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    el.addEventListener('scroll', handleScroll);
    return () => el.removeEventListener('scroll', handleScroll);
  }, [handleScroll]);

  const virtualizer = useVirtualizer({
    count: items.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: (index) => estimateItemHeight(items[index], isBlockExpanded(items[index])),
    overscan: 12,
    // Measure the real row box so long single-line payloads get their true
    // height (the length-based estimate underestimates them); fall back to
    // the estimate when the box is not readable yet (e.g. jsdom).
    measureElement: (el) => {
      const measured = el?.getBoundingClientRect().height ?? 0;
      if (measured > 0) return measured;
      const index = Number(el?.getAttribute('data-index') ?? Number.NaN);
      const item = Number.isFinite(index) ? items[index] : undefined;
      return item ? estimateItemHeight(item, isBlockExpanded(item)) : ROW_BASE_PX + LINE_HEIGHT_PX;
    },
  });

  // Keep the reader's position stable when an older page is prepended: shift
  // the scroll offset by the estimated height of the newly added head items.
  const firstItemRef = useRef<EventLogItem | null>(null);
  useLayoutEffect(() => {
    const prevFirst = firstItemRef.current;
    const first = items[0] ?? null;
    firstItemRef.current = first;
    const el = scrollRef.current;
    if (!el || prevFirst === null || first === null) return;
    const prevFirstSeq = itemStartSeq(prevFirst);
    const firstSeq = itemStartSeq(first);
    if (firstSeq >= prevFirstSeq) return;
    el.scrollTop += estimatePrependShift(prevFirstSeq, prevFirst.kind === 'stepLogBlock', items, expandedKeys);
  }, [items, expandedKeys]);

  // Live tail: while pinned to the bottom, follow every new item.
  useLayoutEffect(() => {
    if (!tail.pinned) return;
    scrollToBottom();
  }, [items, tail.pinned, scrollToBottom]);

  // Count arrivals that land behind the view (following but not pinned), so
  // the "Jump to latest" affordance can report them. The first observation
  // is the initial page load, not an arrival.
  const latestSeq = events.length > 0 ? events[events.length - 1].seq : null;
  const seenSeqRef = useRef<number | null>(null);
  useLayoutEffect(() => {
    const prev = seenSeqRef.current;
    seenSeqRef.current = latestSeq;
    if (prev === null || latestSeq === null || latestSeq <= prev) return;
    const arrived = events.reduce((count, e) => (e.seq > prev ? count + 1 : count), 0);
    if (arrived > 0) dispatch({ type: 'eventArrived' });
  }, [latestSeq, events]);

  // When a run starts (including a page load of an already-running run with
  // history), jump to the bottom once. Opting into auto-follow re-pins the
  // same way.
  const runningRef = useRef(false);
  useLayoutEffect(() => {
    const was = runningRef.current;
    runningRef.current = running;
    if (!running || was) return;
    dispatch({ type: 'jumpRequested' });
    scrollToBottom();
  }, [running, scrollToBottom]);

  const handleAutoFollowChange = (checked: boolean) => {
    dispatch({ type: 'autoFollowChanged', enabled: checked });
    if (checked) scrollToBottom();
  };

  const jumpToLatest = () => {
    dispatch({ type: 'jumpRequested' });
    scrollToBottom();
  };

  const toggleBlock = (key: string) => {
    setExpandedKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const showJump = tail.autoFollow && !tail.pinned && (tail.unseen > 0 || (running && items.length > 0));

  return (
    <div>
      <div className="mb-2 flex items-center justify-between gap-2">
        {hasEarlier ? (
          <button
            type="button"
            onClick={onLoadEarlier}
            disabled={loadingEarlier}
            className="text-xs font-semibold text-sky-400 hover:text-sky-300 disabled:text-slate-500 disabled:cursor-not-allowed"
          >
            {loadingEarlier ? 'Loading earlier events…' : 'Load earlier events'}
          </button>
        ) : (
          <span />
        )}
        <label className="flex items-center gap-2 text-xs text-slate-400 select-none">
          <input
            type="checkbox"
            data-testid="auto-follow-toggle"
            checked={tail.autoFollow}
            onChange={(e) => handleAutoFollowChange(e.target.checked)}
            className="accent-sky-500"
          />
          Auto-follow
        </label>
      </div>
      <div className="relative">
        <div
          ref={scrollRef}
          data-testid="event-log-scroll"
          className="font-mono text-xs bg-slate-900 rounded p-3 h-[60vh] overflow-auto"
        >
          <div style={{ height: virtualizer.getTotalSize(), position: 'relative', width: '100%' }}>
            {virtualizer.getVirtualItems().map((row) => {
              const item = items[row.index];
              return (
                <div
                  key={itemStartSeq(item)}
                  data-testid="event-log-row"
                  data-seq={itemStartSeq(item)}
                  data-index={row.index}
                  ref={virtualizer.measureElement}
                  style={{
                    position: 'absolute',
                    top: 0,
                    left: 0,
                    width: '100%',
                    // Deliberately no height: measureElement reads this
                    // element's border box, so a height pinned to the
                    // estimate would make it observe the estimate instead of
                    // the real content height.
                    overflow: 'hidden',
                    transform: `translateY(${row.start}px)`,
                  }}
                  className="border-b border-slate-800/60 py-1 flex gap-3"
                >
                  {item.kind === 'stepLogBlock' ? (
                    <StepLogBlockRow
                      block={item}
                      expanded={expandedKeys.has(item.key)}
                      onToggle={() => toggleBlock(item.key)}
                    />
                  ) : item.event.type === 'branchEvaluated' ? (
                    <BranchDecisionRow event={item.event} />
                  ) : (
                    <>
                      <span className="text-slate-500 w-12 shrink-0">#{item.event.seq}</span>
                      <span className="text-sky-400 w-40 shrink-0">{item.event.type}</span>
                      <span className="whitespace-pre-wrap break-all min-w-0">
                        {renderPayloadText(item.event)}
                      </span>
                    </>
                  )}
                </div>
              );
            })}
          </div>
        </div>
        {showJump && (
          <button
            type="button"
            data-testid="jump-to-latest"
            onClick={jumpToLatest}
            className="absolute bottom-3 right-3 rounded bg-sky-600 px-3 py-1.5 text-xs font-semibold text-white shadow hover:bg-sky-500"
          >
            {tail.unseen > 0 ? `Jump to latest (${tail.unseen} unseen)` : 'Jump to latest'}
          </button>
        )}
      </div>
    </div>
  );
}

function StepLogBlockRow({
  block,
  expanded,
  onToggle,
}: {
  block: StepLogBlock;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <div className="w-full min-w-0">
      <button
        type="button"
        data-testid="step-log-toggle"
        aria-expanded={expanded}
        onClick={onToggle}
        className="flex gap-3 text-left font-semibold text-sky-400 hover:text-sky-300"
      >
        <span className="text-slate-500 w-12 shrink-0">#{block.startSeq}</span>
        <span className="shrink-0">
          {block.step || 'stepLog'} ×{block.events.length} {expanded ? '▾' : '▸'}
        </span>
      </button>
      <div className="whitespace-pre-wrap break-all min-w-0">{expanded ? block.fullText : block.tailText}</div>
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