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
// additionally reads getBoundingClientRect (always zeros in jsdom), so give
// measured rows a real height too.
beforeAll(() => {
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(800);
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
    x: 0,
    y: 0,
    top: 0,
    left: 0,
    right: 800,
    bottom: 16,
    width: 800,
    height: 16,
    toJSON: () => ({}),
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
    // 16px measured rows: ceil(600/16) = 38 visible + 12 overscan.
    expect(rows.length).toBeLessThan(60);

    // The window opens at the top of the loaded list.
    expect(screen.getByText('chunk 1')).toBeInTheDocument();
    expect(screen.queryByText('chunk 1000')).not.toBeInTheDocument();
  });

  test('measures rendered rows from their real boxes', () => {
    renderLog(thousand);

    // measureElement reads the real box (stubbed to 16px per row here);
    // without it rows would sit at the 25px estimate offsets, so a long
    // single-line payload would keep its underestimated position and could
    // paint over the next row.
    const rows = screen.getAllByTestId('event-log-row');
    expect(rows.length).toBeGreaterThan(1);
    expect(rows[1].style.transform).toBe('translateY(16px)');
    expect(rows[1].style.height).toBe('16px');
    expect(rows[1].style.overflow).toBe('hidden');
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