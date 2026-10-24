import { useCallback, useEffect, useLayoutEffect, useMemo, useReducer, useRef, useState, type ReactNode } from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';
import type { EventEnvelope } from '../../../api/castleApi';
import { FollowButton } from '../../../components/FollowButton';
import { formatAbsoluteTime } from '../time';
import {
  eventSummaryParts,
  expandedValueText,
  fieldLabel,
  toneForKey,
  type EventSummaryTone,
} from './payloadText';
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
  /**
   * `strip` renders the log as a fixed ~10-row tail strip pinned to the
   * bottom of the workspace pane (the page never scrolls; the strip's
   * virtualized scroller does). `full` is the tall standalone variant.
   */
  variant?: 'full' | 'strip';
}

const LINE_HEIGHT_PX = 16; // tailwind text-xs line height
const ROW_BASE_PX = 9; // py-1 padding (8px) + 1px border
const SOFT_WRAP_CHARS_PER_LINE = 120;
/** Distance from the bottom within which the view counts as pinned. */
const BOTTOM_THRESHOLD_PX = 48;

// Expansion key of a plain event row: the envelope's seq is unique within
// a run's log, so it is the row identity (same value the rows use as key).
function eventKey(event: EventEnvelope): string {
  return `event-${event.seq}`;
}

// Wall-clock time an event landed at, local HH:MM:SS; unreadable stamps
// degrade to a dash instead of `Invalid Date`.
function formatEventTime(ts: string | undefined): string {
  if (!ts) return '—';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '—';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

const TONE_CLASSES: Record<EventSummaryTone, string> = {
  mono: 'font-mono text-ink',
  success: 'text-success',
  danger: 'text-danger',
  warning: 'text-warning',
  muted: 'text-ink-muted',
  plain: 'text-ink',
};

function textLines(text: string): number {
  const hardLines = text === '' ? 1 : text.split('\n').length;
  const softLines = Math.ceil(text.length / SOFT_WRAP_CHARS_PER_LINE);
  return Math.max(1, hardLines, softLines);
}

/**
 * Row height estimate. Every collapsed row is a uniform single-line strip:
 * fixed-width seq/time/type cells plus a clipped (nowrap) one-line summary,
 * so collapsed rows are a constant height regardless of payload size. Only
 * expanded bodies are free-form; the virtualizer measures their real boxes.
 */
export function estimateEventHeight(_event: EventEnvelope): number {
  return ROW_BASE_PX + LINE_HEIGHT_PX;
}

/**
 * Height estimate of a log item (plain event row or stepLog block). A
 * collapsed item is the uniform single-line strip; an expanded block's body
 * is the full joined output, so its estimate scales with its line count
 * (measurement corrects the remainder).
 */
export function estimateItemHeight(item: EventLogItem, expanded: boolean): number {
  if (item.kind === 'event') return estimateEventHeight(item.event);
  if (!expanded) return estimateEventHeight(item.events[0]);
  return ROW_BASE_PX + textLines(item.fullText) * LINE_HEIGHT_PX;
}

/**
 * Scroll offset to add after older events are prepended, so the reader's
 * position stays anchored at the previously-first row. All collapsed rows
 * are the uniform strip, so the shift is simply the summed strip heights of
 * the items that moved entirely in front of it.
 */
function estimatePrependShift(prevFirstSeq: number, items: EventLogItem[]): number {
  let shift = 0;
  for (const item of items) {
    if (itemEndSeq(item) < prevFirstSeq) {
      shift += estimateItemHeight(item, false);
      continue;
    }
    if (item.kind === 'event') break;
    const boundaryIndex = item.events.findIndex((e) => e.seq === prevFirstSeq);
    if (boundaryIndex > 0) {
      for (let i = 0; i < boundaryIndex; i++) shift += estimateEventHeight(item.events[i]);
    }
    break;
  }
  return shift;
}

export function EventLog({ events, running, hasEarlier, loadingEarlier, onLoadEarlier, variant = 'full' }: EventLogProps) {
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
    el.scrollTop += estimatePrependShift(prevFirstSeq, items);
  }, [items]);

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
    if (arrived > 0) dispatch({ type: 'eventArrived', count: arrived });
  }, [latestSeq, events]);

  // When a run starts (including a page load of an already-running run with
  // history), jump to the bottom once — but only while auto-follow is
  // enabled, and only once the log has content: with no events yet the
  // terminal-event guard (run data arriving before the event log) cannot
  // have evaluated, so jumping then would pin a finished run whose status
  // still says running. The transition stays unhandled until the log
  // settles, so a run that starts with an empty log still jumps on its
  // first event.
  const runningRef = useRef(false);
  useLayoutEffect(() => {
    const was = runningRef.current;
    if (!running || was) {
      runningRef.current = running;
      return;
    }
    if (!tail.autoFollow || events.length === 0) return;
    runningRef.current = running;
    dispatch({ type: 'jumpRequested' });
    scrollToBottom();
  }, [running, tail.autoFollow, events.length, scrollToBottom]);

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
            className="text-meta font-semibold text-accent-strong hover:underline disabled:text-ink-faint disabled:cursor-not-allowed"
          >
            {loadingEarlier ? 'Loading earlier events…' : 'Load earlier events'}
          </button>
        ) : (
          <span />
        )}
        {/* The standard follow widget (same button everywhere): auto-follow
            pins the log tail to the newest events. */}
        <FollowButton
          label="Auto-follow"
          pressed={tail.autoFollow}
          testId="auto-follow-toggle"
          title={tail.autoFollow ? 'Auto-follow is on — the tail pins to the newest events' : 'Turn on auto-follow to pin the tail to the newest events'}
          onToggle={() => handleAutoFollowChange(!tail.autoFollow)}
        />
      </div>
      <div className="relative">
        <div
          ref={scrollRef}
          data-testid="event-log-scroll"
          className={`font-mono text-xs bg-surface rounded p-3 overflow-auto ${variant === 'strip' ? 'h-[274px] shrink-0' : 'h-[60vh]'}`}
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
                  className="border-b border-line py-1"
                >
                  {item.kind === 'stepLogBlock' ? (
                    <StepLogBlockRow
                      block={item}
                      expanded={expandedKeys.has(item.key)}
                      onToggle={() => toggleBlock(item.key)}
                    />
                  ) : (
                    <EventRow
                      event={item.event}
                      expanded={expandedKeys.has(eventKey(item.event))}
                      onToggle={() => toggleBlock(eventKey(item.event))}
                    />
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
            className="absolute bottom-3 right-3 rounded bg-accent px-3 py-1.5 text-meta font-semibold text-white shadow hover:bg-accent-strong"
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
    <EventRowShell
      seq={block.startSeq}
      ts={block.events[0]?.ts ?? ''}
      label={`${block.step || 'stepLog'} ×${block.events.length}`}
      labelTone="accent"
      expanded={expanded}
      onToggle={onToggle}
    >
      {expanded ? (
        <span className="whitespace-pre-wrap break-all">{block.fullText}</span>
      ) : (
        <span className="text-ink-muted whitespace-nowrap">{block.tailText.split('\n')[0] || '(empty output)'}</span>
      )}
    </EventRowShell>
  );
}

/**
 * Uniform collapsed/expandable row for every event type. Collapsed: the
 * payload's top-level key-value pairs. Expanded: the same fields as a
 * labeled list (the branch-decision rendering, generalized), then the full
 * record as pretty JSON — still inside the single row so the virtualizer
 * keeps measuring it like any other item.
 */
function EventRow({
  event,
  expanded,
  onToggle,
}: {
  event: EventEnvelope;
  expanded: boolean;
  onToggle: () => void;
}) {
  const parts = eventSummaryParts(event);
  return (
    <EventRowShell
      seq={event.seq}
      ts={event.ts}
      label={event.type}
      expanded={expanded}
      onToggle={onToggle}
    >
      {expanded ? (
        <ExpandedFields event={event} />
      ) : (
        <span className="min-w-0 flex items-baseline gap-3 overflow-hidden whitespace-nowrap">
          {parts.map((part, i) => (
            <span key={i} className="flex items-baseline gap-1">
              <span className="text-meta uppercase tracking-wide text-ink-faint">{part.label}</span>
              <span className={`text-xs ${TONE_CLASSES[part.tone ?? 'plain']}`}>{part.text}</span>
            </span>
          ))}
        </span>
      )}
    </EventRowShell>
  );
}

/**
 * Expanded body: the branch-decision field list generalized to any payload
 * — one labeled line per top-level key (long values pretty-print), then
 * the complete record as pretty JSON for reference.
 */
function ExpandedFields({ event }: { event: EventEnvelope }) {
  const p = event.payload as Record<string, unknown> | undefined;
  const entries =
    p && typeof p === 'object' && !Array.isArray(p) ? Object.entries(p) : [];
  return (
    <div className="w-full min-w-0">
      {entries.length > 0 && (
        <div className="font-mono text-xs space-y-1 py-1">
          {entries.map(([key, value]) => (
            <div key={key} className="flex gap-2 items-baseline min-w-0">
              <span className="text-ink-faint shrink-0">{fieldLabel(key)}:</span>
              <span
                className={`whitespace-pre-wrap break-all min-w-0 ${TONE_CLASSES[toneForKey(key, value)]}`}
              >
                {expandedValueText(value)}
              </span>
            </div>
          ))}
        </div>
      )}
      <pre className="whitespace-pre-wrap break-all text-ink-faint font-mono text-xs mt-1 border-t border-line pt-1">
        {JSON.stringify(p ?? null, null, 2)}
      </pre>
    </div>
  );
}

function EventRowShell({
  seq,
  ts,
  label,
  labelTone = 'plain',
  expanded = false,
  onToggle,
  children,
}: {
  seq: number;
  ts: string | undefined;
  label: string;
  labelTone?: 'plain' | 'accent';
  expanded?: boolean;
  onToggle?: () => void;
  children: ReactNode;
}) {
  const summaryLine = (
    <div className="flex w-full min-w-0 items-baseline gap-3 overflow-hidden">
      <span className="text-ink-faint w-12 shrink-0">#{seq}</span>
      <span className="text-ink-faint w-16 shrink-0 font-mono" title={formatAbsoluteTime(ts ?? '')} data-testid="event-time">
        {formatEventTime(ts)}
      </span>
      <span
        className={`w-36 shrink-0 truncate ${labelTone === 'accent' ? 'text-accent-strong' : 'text-ink'}`}
      >
        {label}
      </span>
      <span className="min-w-0 flex-1 text-xs text-ink">{children}</span>
      {onToggle && (
        <span aria-hidden className="text-ink-faint shrink-0">{expanded ? '▾' : '▸'}</span>
      )}
    </div>
  );
  if (!onToggle)
    return (
      <div role="group" className="w-full">
        {summaryLine}
      </div>
    );
  return (
    <button
      type="button"
      data-testid={labelTone === 'accent' ? 'step-log-toggle' : 'event-log-toggle'}
      aria-expanded={expanded}
      onClick={onToggle}
      className="w-full text-left"
    >
      {summaryLine}
    </button>
  );
}