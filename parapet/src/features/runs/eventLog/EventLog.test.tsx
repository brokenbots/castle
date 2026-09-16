import { act, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, test, vi, beforeAll } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import { EventLog, estimateEventHeight } from './EventLog';

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
      hasEarlier={false}
      loadingEarlier={false}
      onLoadEarlier={onLoadEarlier}
      {...props}
    />,
  );
  return { onLoadEarlier, rerender: result.rerender };
}

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
  const thousand = Array.from({ length: 1000 }, (_, i) => env(i + 1));

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