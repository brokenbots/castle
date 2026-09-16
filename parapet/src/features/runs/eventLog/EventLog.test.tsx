import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, test, vi, beforeAll } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import { coalesceStepLogs } from './coalesce';
import { EventLog, estimateEventHeight, estimateItemHeight } from './EventLog';

// jsdom has no ResizeObserver; react-virtual needs one. Scoped to this file
// (vitest isolates globals per test file, so setup.ts's AbortController
// stubs are unaffected).
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal('ResizeObserver', ResizeObserverStub);

// jsdom has no layout: offsetWidth/offsetHeight measure 0 and react-virtual
// (which reads offsetHeight via getRect) renders nothing. Give elements a
// plausible size so the virtualizer computes a real window. measureElement
// reads getBoundingClientRect, so model the browser box contract faithfully:
// a committed inline height pins the border box (CSS: height wins over
// content); otherwise the box fits the content — payload text wraps at the
// container width (~104 12px-monospace chars at this 800px box; 16px per
// line + 9px row padding). A stub contradicting an element's committed
// height would let row-measurement tests pass on the row-pinning behaviour
// they exist to catch.
const PAYLOAD_CHARS_PER_LINE = 104;
const ROW_LINE_PX = 16;
const ROW_BASE_PX = 9;

beforeAll(() => {
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(800);
  // Tail logic reads the scroll metrics to detect "at bottom"; model the
  // scroller as a 600px viewport over its committed inner height.
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockImplementation(function (this: HTMLElement) {
    return this.getAttribute('data-testid') === 'event-log-scroll' ? 600 : 0;
  });
  vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockImplementation(function (this: HTMLElement) {
    if (this.getAttribute('data-testid') !== 'event-log-scroll') return 0;
    const inner = this.firstElementChild;
    return inner ? inner.getBoundingClientRect().height : 0;
  });
  vi.spyOn(
    HTMLElement.prototype,
    'getBoundingClientRect',
  ).mockImplementation(function (this: HTMLElement) {
    const pinned = Number.parseFloat(this.style.height);
    const textLength = this.textContent?.length ?? 0;
    const wrappedLines = Math.max(
      1,
      Math.ceil(textLength / PAYLOAD_CHARS_PER_LINE),
    );
    const height = Number.isNaN(pinned)
      ? ROW_BASE_PX + wrappedLines * ROW_LINE_PX
      : pinned;
    return {
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      right: 800,
      bottom: height,
      width: 800,
      height,
      toJSON: () => ({}),
    };
  });
});

function env(seq: number, chunk = `chunk ${seq}`): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq,
    type: 'stepLog',
    ts: new Date(0).toISOString(),
    correlationId: '',
    payload: { chunk },
  };
}

function renderLog(events: EventEnvelope[], props?: Partial<Parameters<typeof EventLog>[0]>) {
  const onLoadEarlier = vi.fn();
  const result = render(
    <EventLog
      events={events}
      running={false}
      hasEarlier={false}
      loadingEarlier={false}
      onLoadEarlier={onLoadEarlier}
      {...props}
    />,
  );
  return { onLoadEarlier, rerender: result.rerender };
}

function logProps(events: EventEnvelope[], running: boolean, hasEarlier = false) {
  return (
    <EventLog
      events={events}
      running={running}
      hasEarlier={hasEarlier}
      loadingEarlier={false}
      onLoadEarlier={() => {}}
    />
  );
}

// stepLog chunks WITH a correlation id: these are the ones that coalesce.
// env() keeps correlationId '' (no identity), so existing fixtures stay plain rows.
function keyedEnv(seq: number, correlationId = 'corr-1'): EventEnvelope {
  return { ...env(seq), correlationId };
}

function otherEnv(seq: number): EventEnvelope {
  return {
    schemaVersion: 1,
    runId: 'run-1',
    seq,
    type: 'runStatus',
    ts: new Date(0).toISOString(),
    correlationId: '',
    payload: { detail: `event ${seq}` },
  };
}

// Plain 25px rows at this stub: the standard large fixture for both the
// virtualization and live-tail suites.
const thousand = Array.from({ length: 1000 }, (_, i) => env(i + 1));

describe('estimateEventHeight', () => {
  test('single-line payload estimates one line plus row padding', () => {
    expect(estimateEventHeight(env(1, 'ok'))).toBe(9 + 16);
  });

  test('hard newlines scale the estimate', () => {
    expect(estimateEventHeight(env(1, 'a\nb\nc'))).toBe(9 + 3 * 16);
  });

  test('long unbroken payloads estimate wrapped lines', () => {
    expect(estimateEventHeight(env(1, 'x'.repeat(240)))).toBe(9 + 2 * 16);
  });
});

describe('EventLog', () => {
  test('renders only a window of rows for large logs', () => {
    renderLog(thousand);

    const rows = screen.getAllByTestId('event-log-row');
    expect(rows.length).toBeGreaterThan(0);
    // 25px rows: ceil(600/25) = 24 visible + 12 overscan.
    expect(rows.length).toBeLessThan(50);

    // The window opens at the top of the loaded list.
    expect(screen.getByText('chunk 1')).toBeInTheDocument();
    expect(screen.queryByText('chunk 1000')).not.toBeInTheDocument();
  });

  test('measures rendered rows from their real boxes', () => {
    // A 240-char single-line payload: the estimator pins 2 lines (41px),
    // but at this 800px-wide box it actually wraps to 3 lines (57px). With
    // the stub modelling the browser box contract, measureElement can only
    // see 57px if the row's height is not pinned to the estimate — so this
    // test fails while the row style pins height.
    renderLog([env(1, 'x'.repeat(240)), env(2), env(3)]);

    const rows = screen.getAllByTestId('event-log-row');
    expect(rows.length).toBe(3);
    // The row box is not pinned to the estimate, so measurement can work.
    expect(rows[0].style.height).toBe('');
    expect(rows[1].style.height).toBe('');
    // Row 1 sits at row 0's measured box (3 wrapped lines = 57px), not at
    // the 41px estimate; content must not be clipped into the estimate.
    expect(rows[1].style.transform).toBe('translateY(57px)');
    expect(rows[0].style.overflow).toBe('hidden');
  });

  test('renders the newest rows after scrolling to the bottom', () => {
    renderLog(thousand);

    const scroller = screen.getByTestId('event-log-scroll');
    act(() => {
      scroller.scrollTop = 1e9;
      scroller.dispatchEvent(new Event('scroll'));
    });

    expect(screen.getByText('chunk 1000')).toBeInTheDocument();
    expect(screen.queryByText('chunk 1')).not.toBeInTheDocument();
  });

  test('preserves the reader position when older events are prepended', () => {
    const tail = thousand.slice(500);
    const { rerender } = renderLog(tail);
    expect(screen.getByTestId('event-log-scroll').scrollTop).toBe(0);

    rerender(
      <EventLog
        events={thousand}
        running={false}
        hasEarlier={false}
        loadingEarlier={false}
        onLoadEarlier={() => {}}
      />,
    );

    // The scroll offset shifted by the estimated height of the 500 new
    // head rows (25px each: 9px padding + one 16px line).
    expect(screen.getByTestId('event-log-scroll').scrollTop).toBe(500 * 25);
  });

  test('offers the load-earlier control only when older pages remain', async () => {
    const { onLoadEarlier } = renderLog(thousand.slice(500), { hasEarlier: true });

    await userEvent.click(screen.getByRole('button', { name: 'Load earlier events' }));
    expect(onLoadEarlier).toHaveBeenCalledTimes(1);
  });

  test('disables the control while an earlier page is loading', () => {
    renderLog(thousand, { hasEarlier: true, loadingEarlier: true });

    const button = screen.getByRole('button', { name: 'Loading earlier events…' });
    expect(button).toBeDisabled();
  });

  test('hides the control when the log is complete', () => {
    renderLog(thousand.slice(0, 10), { hasEarlier: false });

    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });
});

describe('estimateItemHeight', () => {
  const [block] = coalesceStepLogs([keyedEnv(1), keyedEnv(2)]);
  if (!block || block.kind !== 'stepLogBlock') throw new Error('expected a coalesced block');

  test('collapsed blocks estimate a header line plus the tail', () => {
    expect(estimateItemHeight(block, false)).toBe(9 + 2 * 16);
  });

  test('expanded blocks estimate from the full output', () => {
    expect(estimateItemHeight(block, true)).toBe(9 + 3 * 16);
  });
});

describe('step log coalescing', () => {
  test('renders 500 consecutive chunks of one step as a single collapsed block', () => {
    const events = Array.from({ length: 500 }, (_, i) => keyedEnv(i + 1));
    renderLog(events);

    const rows = screen.getAllByTestId('event-log-row');
    expect(rows).toHaveLength(1);
    const toggle = screen.getByTestId('step-log-toggle');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    // The header counts the coalesced chunks…
    expect(toggle).toHaveTextContent('×500');
    // …and the body shows only the tail of the output, not the whole log.
    expect(screen.getByText('chunk 500')).toBeInTheDocument();
    expect(screen.queryByText('chunk 499')).not.toBeInTheDocument();
    expect(screen.queryByText('chunk 1')).not.toBeInTheDocument();
  });

  test('expands the block to the full output and collapses back', async () => {
    const events = Array.from({ length: 500 }, (_, i) => keyedEnv(i + 1));
    renderLog(events);

    const toggle = screen.getByTestId('step-log-toggle');
    await userEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    // The full output (joined chunks) is now rendered in the row.
    const rows = screen.getAllByTestId('event-log-row');
    expect(rows[0].textContent).toContain('chunk 1');
    expect(rows[0].textContent).toContain('chunk 250');
    expect(rows[0].textContent).toContain('chunk 500');

    await userEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(rows[0].textContent).not.toContain('chunk 1\n');
  });

  test('keeps non-log events interleaved around coalesced blocks', () => {
    // chunk 1 is a lone run (plain row); chunks 3+4 form a block; the
    // runStatus events interleave chronologically in between.
    const events = [keyedEnv(1, 'corr-a'), otherEnv(2), keyedEnv(3, 'corr-a'), keyedEnv(4, 'corr-a'), otherEnv(5)];
    renderLog(events);

    const rows = screen.getAllByTestId('event-log-row');
    expect(rows.map((row) => row.getAttribute('data-seq'))).toEqual(['1', '2', '3', '5']);
    // The lone chunk stays a plain event row.
    expect(rows[0].querySelector('[data-testid="step-log-toggle"]')).toBeNull();
    expect(rows[0].textContent).toContain('chunk 1');
    // The consecutive run coalesces into one block showing the tail chunk.
    expect(rows[2].querySelector('[data-testid="step-log-toggle"]')).not.toBeNull();
    expect(rows[2].textContent).toContain('chunk 4');
    expect(rows[3].textContent).toContain('event 5');
  });

  test('coalesces chunks that arrive across page loads and keeps the reader anchored', () => {
    // The newest page contains only the step's last chunk: it renders as a
    // lone (plain) event row.
    const tail = [keyedEnv(1000)];
    const { rerender } = renderLog(tail);
    expect(screen.getAllByTestId('event-log-row')).toHaveLength(1);
    const scroller = screen.getByTestId('event-log-scroll');
    expect(scroller.scrollTop).toBe(0);

    // Loading the older page merges the chunks into a single block spanning
    // the page boundary.
    const older = Array.from({ length: 999 }, (_, i) => keyedEnv(i + 1));
    rerender(logProps([...older, ...tail], false));

    const rows = screen.getAllByTestId('event-log-row');
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain('×1000');
    expect(screen.queryByText('chunk 1')).not.toBeInTheDocument();
    expect(screen.getByText('chunk 1000')).toBeInTheDocument();
    // The reader stays anchored at the old first chunk: the block header
    // line plus the 999 chunk rows that moved in front of it.
    expect(scroller.scrollTop).toBe(16 + 999 * 25);
  });
});

describe('live tail', () => {
  test('pins to the bottom when a running run mounts and auto-follow defaults to on', () => {
    renderLog(thousand, { running: true });

    const scroller = screen.getByTestId('event-log-scroll');
    expect(screen.getByTestId('auto-follow-toggle')).toBeChecked();
    expect(scroller.scrollTop).toBe(scroller.scrollHeight);
  });

  test('does not jump to the bottom for a completed run', () => {
    renderLog(thousand);

    const scroller = screen.getByTestId('event-log-scroll');
    expect(scroller.scrollTop).toBe(0);
    expect(screen.queryByTestId('jump-to-latest')).not.toBeInTheDocument();
  });

  test('detaches on scroll-up and reports the unseen count behind the pin', async () => {
    const { rerender } = renderLog(thousand, { running: true });
    const scroller = screen.getByTestId('event-log-scroll');

    // Scrolling up releases the pin; nothing is unseen yet, so the
    // affordance shows without a count…
    act(() => {
      scroller.scrollTop = 0;
      scroller.dispatchEvent(new Event('scroll'));
    });
    expect(screen.getByTestId('jump-to-latest')).toBeInTheDocument();
    expect(screen.getByTestId('jump-to-latest')).toHaveTextContent('Jump to latest');

    // …arrivals accumulate behind the pin and the count grows.
    rerender(logProps([...thousand, env(thousand.length + 1)], true));
    expect(screen.getByTestId('jump-to-latest')).toHaveTextContent('Jump to latest (1 unseen)');
    rerender(logProps([...thousand, env(thousand.length + 1), env(thousand.length + 2)], true));
    expect(screen.getByTestId('jump-to-latest')).toHaveTextContent('Jump to latest (2 unseen)');

    // Activating the affordance re-pins the view to the bottom.
    await userEvent.click(screen.getByTestId('jump-to-latest'));
    expect(scroller.scrollTop).toBe(scroller.scrollHeight);
    expect(screen.queryByTestId('jump-to-latest')).not.toBeInTheDocument();
  });

  test('re-pins when the user scrolls back to the bottom', () => {
    const { rerender } = renderLog(thousand, { running: true });
    const scroller = screen.getByTestId('event-log-scroll');

    act(() => {
      scroller.scrollTop = 0;
      scroller.dispatchEvent(new Event('scroll'));
    });
    rerender(logProps([...thousand, env(thousand.length + 1)], true));
    expect(screen.getByTestId('jump-to-latest')).toHaveTextContent('(1 unseen)');

    // Scrolling back down re-pins and clears the unseen count.
    act(() => {
      scroller.scrollTop = 1e9;
      scroller.dispatchEvent(new Event('scroll'));
    });
    expect(screen.queryByTestId('jump-to-latest')).not.toBeInTheDocument();
  });

  test('stops following new output when auto-follow is opted out', async () => {
    const { rerender } = renderLog(thousand, { running: true });
    const scroller = screen.getByTestId('event-log-scroll');

    await userEvent.click(screen.getByTestId('auto-follow-toggle'));
    // Park the view away from the bottom after opting out.
    act(() => {
      scroller.scrollTop = 0;
      scroller.dispatchEvent(new Event('scroll'));
    });

    rerender(logProps([...thousand, env(thousand.length + 1)], true));
    // No following while opted out, and no jump affordance either (it only
    // exists while auto-follow is enabled).
    expect(scroller.scrollTop).toBe(0);
    expect(screen.queryByTestId('jump-to-latest')).not.toBeInTheDocument();

    // Re-enabling re-pins the view at the bottom.
    await userEvent.click(screen.getByTestId('auto-follow-toggle'));
    expect(scroller.scrollTop).toBe(scroller.scrollHeight);
  });
});