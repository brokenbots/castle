import { useEffect, useState, type ReactNode } from 'react';

export const DOCKED_PANEL_MIN_WIDTH = 240;
// The dock never takes more than this fraction of the viewport.
export const DOCKED_PANEL_MAX_VIEWPORT_RATIO = 0.6;
const RESIZE_STEP_PX = 24;

interface DockedPanelProps {
  title: ReactNode;
  onClose?: () => void;
  defaultWidth?: number;
  testId?: string;
  children: ReactNode;
}

function clampWidth(width: number): number {
  const max = Math.round(window.innerWidth * DOCKED_PANEL_MAX_VIEWPORT_RATIO);
  return Math.min(Math.max(width, DOCKED_PANEL_MIN_WIDTH), max);
}

interface DragState {
  startX: number;
  startWidth: number;
}

// Docked right-side panel. Lives inside the page layout (no fixed
// positioning): a header row with title and close affordance, a body that
// scrolls independently, and a draggable left edge for resizing. The resize
// handle is also focusable and responds to ArrowLeft/ArrowRight for
// keyboard-driven resizing.
export function DockedPanel({ title, onClose, defaultWidth = 320, testId = 'docked-panel', children }: DockedPanelProps) {
  const [width, setWidth] = useState(() => clampWidth(defaultWidth));
  const [drag, setDrag] = useState<DragState | null>(null);

  useEffect(() => {
    if (!drag) return;
    const onMove = (e: MouseEvent) => {
      // The panel is right-anchored: moving the pointer left widens it.
      setWidth(clampWidth(drag.startWidth + (drag.startX - e.clientX)));
    };
    const onUp = () => setDrag(null);
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
    return () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
    };
  }, [drag]);

  const startDrag = (e: React.MouseEvent) => {
    e.preventDefault();
    setDrag({ startX: e.clientX, startWidth: width });
  };

  const resizeBy = (delta: number) => setWidth((w) => clampWidth(w + delta));

  const onKeyDown = (e: React.KeyboardEvent) => {
    // Right-docked panel: ArrowLeft pulls the edge left (wider), ArrowRight
    // pushes it right (narrower).
    if (e.key === 'ArrowLeft') {
      e.preventDefault();
      resizeBy(RESIZE_STEP_PX);
    } else if (e.key === 'ArrowRight') {
      e.preventDefault();
      resizeBy(-RESIZE_STEP_PX);
    }
  };

  const max = Math.round(window.innerWidth * DOCKED_PANEL_MAX_VIEWPORT_RATIO);

  return (
    <aside
      data-testid={testId}
      style={{ width: `${width}px` }}
      className="relative flex h-full min-h-0 shrink-0 flex-col border-l border-line bg-surface"
      aria-label={typeof title === 'string' ? title : undefined}
    >
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize panel"
        aria-valuenow={width}
        aria-valuemin={DOCKED_PANEL_MIN_WIDTH}
        aria-valuemax={max}
        tabIndex={0}
        data-testid={`${testId}-resize`}
        onMouseDown={startDrag}
        onKeyDown={onKeyDown}
        className="absolute inset-y-0 -left-1 w-2 cursor-col-resize hover:bg-accent-soft focus-visible:bg-accent-soft focus:outline-none"
      />
      <div className="flex shrink-0 items-center justify-between border-b border-line px-4 py-3">
        <h3 className="text-body font-semibold text-ink">{title}</h3>
        {onClose && (
          <button
            type="button"
            data-testid={`${testId}-close`}
            aria-label="Close panel"
            onClick={onClose}
            className="text-ink-muted hover:text-ink"
          >
            ×
          </button>
        )}
      </div>
      <div data-testid={`${testId}-body`} className="min-h-0 flex-1 space-y-4 overflow-y-auto p-4">
        {children}
      </div>
    </aside>
  );
}