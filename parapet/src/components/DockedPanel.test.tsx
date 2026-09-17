import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, test, vi } from 'vitest';
import {
  DockedPanel,
  DOCKED_PANEL_MAX_VIEWPORT_RATIO,
  DOCKED_PANEL_MIN_WIDTH,
} from './DockedPanel';

const VIEWPORT = 1024;

describe('DockedPanel', () => {
  test('renders a right-docked panel with title, body and close control', () => {
    const onClose = vi.fn();
    render(
      <DockedPanel title="Run Scope" onClose={onClose} testId="scope-dock">
        <p>panel content</p>
      </DockedPanel>,
    );

    const panel = screen.getByTestId('scope-dock');
    expect(panel).toBeInTheDocument();
    // Lives in the page flow: no fixed positioning on a docked panel.
    expect(panel.className).not.toContain('fixed');
    expect(screen.getByRole('heading', { name: 'Run Scope' })).toBeInTheDocument();
    expect(screen.getByTestId('scope-dock-body')).toHaveTextContent('panel content');
    expect(screen.getByTestId('scope-dock-body')).toHaveClass('overflow-y-auto');

    fireEvent.click(screen.getByTestId('scope-dock-close'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  test('resizes with the pointer and clamps to the viewport ratio', () => {
    render(
      <DockedPanel title="Run Scope" testId="scope-dock">
        <p>content</p>
      </DockedPanel>,
    );

    const handle = screen.getByTestId('scope-dock-resize');
    const panel = screen.getByTestId('scope-dock');
    expect(panel.style.width).toBe('320px');

    // Pointer drag leftwards widens the right-docked panel.
    fireEvent.mouseDown(handle, { clientX: 800 });
    fireEvent.mouseMove(window, { clientX: 600 });
    expect(panel.style.width).toBe('520px');

    // Dragging far past the viewport limit clamps at 60% of the window.
    fireEvent.mouseMove(window, { clientX: 0 });
    expect(panel.style.width).toBe(
      `${Math.round(VIEWPORT * DOCKED_PANEL_MAX_VIEWPORT_RATIO)}px`,
    );

    // After mouseup the drag ends: further moves are ignored.
    fireEvent.mouseUp(window);
    fireEvent.mouseMove(window, { clientX: 1000 });
    expect(panel.style.width).toBe(
      `${Math.round(VIEWPORT * DOCKED_PANEL_MAX_VIEWPORT_RATIO)}px`,
    );
    expect(handle.getAttribute('aria-valuenow')).toBe(
      String(Math.round(VIEWPORT * DOCKED_PANEL_MAX_VIEWPORT_RATIO)),
    );
  });

  test('resizes with the keyboard and clamps at the minimum width', () => {
    render(
      <DockedPanel title="Run Scope" testId="scope-dock">
        <p>content</p>
      </DockedPanel>,
    );

    const handle = screen.getByTestId('scope-dock-resize');
    handle.focus();

    // ArrowLeft widens the right-docked panel by the step.
    fireEvent.keyDown(handle, { key: 'ArrowLeft' });
    expect(handle.getAttribute('aria-valuenow')).toBe('344');

    // ArrowRight narrows it, clamping at the configured minimum.
    for (let i = 0; i < 5; i += 1) {
      fireEvent.keyDown(handle, { key: 'ArrowRight' });
    }
    expect(handle.getAttribute('aria-valuenow')).toBe(String(DOCKED_PANEL_MIN_WIDTH));
    expect(screen.getByTestId('scope-dock').style.width).toBe(
      `${DOCKED_PANEL_MIN_WIDTH}px`,
    );
  });
});