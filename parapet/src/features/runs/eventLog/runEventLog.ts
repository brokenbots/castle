import type { EventEnvelope } from '../../../api/castleApi';

/**
 * Page size for ListRunEvents; matches castle's sqlite.ListEventsDefaultLimit
 * (the server rejects anything above ListEventsMaxLimit = 2048).
 */
export const EVENT_PAGE_SIZE = 500;

export interface EventPage {
  events: EventEnvelope[];
  lastSeq: number;
  /**
   * Continuation cursor to pass as since_seq for the next (newer) page, or
   * null when the page was not full and no newer events exist.
   */
  nextSinceSeq: number | null;
}

export type FetchEventPage = (sinceSeq: number, limit: number) => Promise<EventPage>;

export interface RunEventLogState {
  /** since_seq to start WatchRun from; 0 until the newest page is anchored. */
  anchor: number;
  /** Smallest seq currently held in the log; null while nothing is loaded. */
  oldestLoaded: number | null;
  /** Older pages exist below oldestLoaded and can be fetched on demand. */
  hasEarlier: boolean;
  loadingEarlier: boolean;
}

export type RunEventLogAction =
  | { type: 'anchored'; anchor: number; oldestLoaded: number | null }
  | { type: 'anchorFailed' }
  | { type: 'loadEarlierStart' }
  | { type: 'earlierPageLoaded'; events: EventEnvelope[] }
  | { type: 'earlierFailed' };

export const initialRunEventLogState: RunEventLogState = {
  anchor: 0,
  oldestLoaded: null,
  hasEarlier: false,
  loadingEarlier: false,
};

export function runEventLogReducer(
  state: RunEventLogState,
  action: RunEventLogAction,
): RunEventLogState {
  switch (action.type) {
    case 'anchored':
      return {
        ...state,
        anchor: action.anchor,
        oldestLoaded: action.oldestLoaded,
        hasEarlier: action.oldestLoaded !== null && action.oldestLoaded > 1,
      };
    case 'anchorFailed':
      // Degrade to the pre-pagination behavior: the watch replays from the
      // beginning and there is nothing older to page through.
      return { ...state, anchor: 0, hasEarlier: false };
    case 'loadEarlierStart':
      if (!state.hasEarlier || state.loadingEarlier) return state;
      return { ...state, loadingEarlier: true };
    case 'earlierPageLoaded': {
      if (action.events.length === 0) {
        return { ...state, hasEarlier: false, loadingEarlier: false };
      }
      const oldestLoaded = Math.min(state.oldestLoaded ?? Infinity, action.events[0].seq);
      return { ...state, oldestLoaded, hasEarlier: oldestLoaded > 1, loadingEarlier: false };
    }
    case 'earlierFailed':
      return { ...state, loadingEarlier: false };
  }
}

/** since_seq that fetches the page of events immediately below oldestLoaded. */
export function loadEarlierCursor(state: RunEventLogState): number {
  return state.oldestLoaded === null
    ? 0
    : Math.max(0, state.oldestLoaded - 1 - EVENT_PAGE_SIZE);
}

export interface AnchorOutcome {
  /** since_seq for the watch (0 when the run has no events). */
  anchor: number;
  oldestLoaded: number | null;
  /** Events of the retained newest page; the caller dispatches them. */
  retained: EventEnvelope[];
}

/**
 * Anchors the event log at the newest events. ListRunEvents pages
 * forward-only (since_seq is an exclusive lower bound and next_since_seq is
 * only set on full pages), so reaching the newest page requires walking from
 * 0. Intermediate pages are discarded: only the final page is retained, and
 * the caller dispatches just those events into the store. Runs whose event
 * count is an exact page multiple end the walk with an empty probe, in which
 * case the preceding full page is the tail.
 */
export async function anchorRunEventLog(fetchPage: FetchEventPage): Promise<AnchorOutcome> {
  let since = 0;
  let staged: { events: EventEnvelope[]; lastSeq: number } | null = null;
  for (;;) {
    const page = await fetchPage(since, EVENT_PAGE_SIZE);
    if (page.events.length === 0) {
      // No newer events: a staged full page is the tail, else the run is empty.
      if (staged) {
        return { anchor: staged.lastSeq, oldestLoaded: staged.events[0].seq, retained: staged.events };
      }
      return { anchor: 0, oldestLoaded: null, retained: [] };
    }
    if (page.nextSinceSeq !== null && page.nextSinceSeq > since) {
      staged = { events: page.events, lastSeq: page.lastSeq };
      since = page.nextSinceSeq;
      continue;
    }
    return { anchor: page.lastSeq, oldestLoaded: page.events[0].seq, retained: page.events };
  }
}