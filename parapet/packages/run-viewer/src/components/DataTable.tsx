import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';

// Shared list-table machinery for the run list and the agents list. Both
// tables live in different feature code paths (runs in the run-viewer
// feature, agents in host pages), so the mechanics they share — sticky
// header chrome, full-row click-through, per-table column selection — are
// factored here and exported for both.

/** Column definition consumed by the selector and the hooks below. */
export interface TableColumn {
  id: string;
  label: string;
}

const columnsStorageKey = (tableId: string): string => `parapet-columns:${tableId}`;

function readStoredVisible(tableId: string, columns: TableColumn[]): string[] | null {
  try {
    const raw = localStorage.getItem(columnsStorageKey(tableId));
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return null;
    const known = new Set(columns.map((c) => c.id));
    const ids = parsed.filter((v): v is string => typeof v === 'string' && known.has(v));
    // A stored selection that resolves to nothing (or only unknown ids) or
    // that predates a column rename falls back to "all visible".
    return ids.length > 0 ? ids : null;
  } catch {
    return null;
  }
}

/**
 * Column visibility for one table: which columns are shown, persisted under
 * a stable table id. Hidden columns may never empty the table — the last
 * visible column cannot be dismissed.
 */
export function useColumnVisibility(tableId: string, columns: TableColumn[]) {
  const idsKey = columns.map((c) => c.id).join('|');
  const [visible, setVisible] = useState<string[]>(() => {
    const stored = readStoredVisible(tableId, columns);
    return stored ?? columns.map((c) => c.id);
  });

  // Reconcile when the column set changes (e.g. a new column ships): keep
  // known visible ids, reveal new ones.
  useEffect(() => {
    const known = idsKey.split('|');
    setVisible((current) => {
      const kept = current.filter((id) => known.includes(id));
      const missing = known.filter((id) => !kept.includes(id));
      const next = [...kept, ...missing];
      return next.length === current.length && next.every((id, i) => current[i] === id)
        ? current
        : next;
    });
  }, [idsKey]);

  const toggle = useCallback(
    (id: string) => {
      setVisible((current) => {
        // The table never renders zero columns.
        if (current.length === 1 && current[0] === id) return current;
        const next = current.includes(id)
          ? current.filter((c) => c !== id)
          : [...columns.map((c) => c.id)].filter((c) => c === id || current.includes(c));
        try {
          localStorage.setItem(columnsStorageKey(tableId), JSON.stringify(next));
        } catch {
          // Storage can throw in privacy modes; the in-session state wins.
        }
        return next;
      });
    },
    [columns, tableId],
  );

  return { visible, toggle };
}

interface ColumnVisibilitySelectProps {
  columns: TableColumn[];
  visible: string[];
  onToggle: (id: string) => void;
  /** Stable test hooks: `${testId}-button`, `${testId}-col-${id}`. */
  testId: string;
}

/**
 * Column picker: a compact "Columns" dropdown listing one checked item per
 * column. Rendering is a native details/summary pair inside a positioned
 * wrapper, so no outside-click plumbing is needed.
 */
export function ColumnVisibilitySelect({ columns, visible, onToggle, testId }: ColumnVisibilitySelectProps) {
  return (
    <details className="relative" data-testid={testId}>
      <summary
        data-testid={`${testId}-button`}
        title="Choose which columns to show"
        className="flex cursor-pointer select-none items-center gap-2 rounded-md border border-line bg-surface px-2 py-1 text-body text-ink-muted hover:bg-surface-raised hover:text-ink"
      >
        Columns
        <svg aria-hidden viewBox="0 0 16 16" className="h-3 w-3" fill="none" stroke="currentColor" strokeWidth="1.5">
          <path d="M4 6l4 4 4-4" />
        </svg>
      </summary>
      <div
        data-testid={`${testId}-panel`}
        role="group"
        aria-label="Visible columns"
        className="absolute right-0 top-full z-20 mt-1 min-w-40 rounded-md border border-line bg-surface p-2 shadow-xl"
      >
        {columns.map((c) => (
          <label
            key={c.id}
            className="flex cursor-pointer select-none items-center gap-2 rounded px-2 py-1 text-body text-ink hover:bg-surface-raised"
          >
            <input
              type="checkbox"
              data-testid={`${testId}-col-${c.id}`}
              aria-label={`Show column ${c.label}`}
              title={`Show column ${c.label}`}
              checked={visible.includes(c.id)}
              disabled={visible.length === 1 && visible[0] === c.id}
              onChange={() => onToggle(c.id)}
              className="accent-(--parapet-accent)"
            />
            <span aria-hidden>{c.label}</span>
            {/* The label text ends up in the accessible name of the wrapped
                checkbox; a suffix keeps it from exactly equaling the column
                name, so getByLabelText('Status') (etc.) doesn't collide with
                other labeled controls that share a column's name. */}
            <span className="sr-only">{' column'}</span>
          </label>
        ))}
      </div>
    </details>
  );
}

/**
 * Full-row click-through handler: navigates to the row's detail route. A
 * click that starts on (or inside) an interactive element — the row's own
 * links and controls — takes the element's own action instead, so the
 * status chip can still route to its filtered list view.
 */
export function makeRowClick(
  navigate: (to: string) => void,
  to: string,
): (event: React.MouseEvent<HTMLTableRowElement>) => void {
  return (event) => {
    if (event.target instanceof Element && event.target.closest('a, button, input, select, label, summary')) {
      return;
    }
    navigate(to);
  };
}

/** Sticky-header table chrome both list pages share. */
export function StickyTableHead({ children }: { children: ReactNode }) {
  return (
    <thead className="sticky top-[-1px] z-10 bg-canvas/95 text-ink-muted backdrop-blur-sm">
      {children}
    </thead>
  );
}

/** Convenience for row-link pages: navigate via react-router. */
export function useRowNavigation() {
  return useNavigate();
}