import { useState, type ReactNode } from 'react';
import { FollowButton } from './FollowButton';

interface DetailPanelProps {
  title: ReactNode;
  /** Stable test hook for the section; `${id}-toggle` / `${id}-body` follow. */
  id: string;
  /** Legacy testid for the collapse toggle, when tests pin a specific name. */
  idToggle?: string;
  /** Right-aligned control row (placement toggles, follow button…). */
  toolbar?: ReactNode;
  /** Start collapsed; state is per-mount (same as the current sections). */
  defaultCollapsed?: boolean;
  /** Hide the collapse affordance entirely (always-open panels). */
  collapsible?: boolean;
  /**
   * Controlled collapse (optional): when `collapsed` is provided the panel
   * becomes fully controlled — `onToggle` receives the requested flip and
   * owns the state. Uncontrolled otherwise.
   */
  collapsed?: boolean;
  onToggle?: () => void;
  /**
   * Copy shown in place of the body while collapsed. Rendered as
   * `<p data-testid="{id}-collapsed">` so callers keep their existing
   * collapsed-state hooks.
   */
  collapsedNote?: string;
  /**
   * Body testid override; when null the body div carries no data-testid
   * (the `${id}-body` default stays reserved for callers whose tests pin
   * the container itself).
   */
  bodyTestId?: string | null;
  /**
   * Body id override — legacy DOM contract (`aria-controls` targets from
   * existing tests markup). Defaults to `${id}-body`.
   */
  bodyIdOverride?: string;
  /**
   * Collapsed-note testid override; defaults to `${id}-collapsed`. Legacy
   * panels keep their original collapsed-state testids through this.
   */
  collapsedTestId?: string;
  /** Extra classes for the section root (layout containers pass flex sizing). */
  className?: string;
  /**
   * Body classes; replaces the default `p-3` padding so layout-critical
   * panels (the graph) can stretch their bodies edge to edge.
   */
  bodyClassName?: string;
  /**
   * Follow state + toggle for live-linked panels (the drawer's source and
   * inspection views): renders a compact pinned toggled indicator in the
   * header. Omit for plain panels.
   */
  followed?: boolean;
  /** Marks a specific panel header as the follow affordance in tests. */
  followTestId?: string;
  onFollowToggle?: () => void;
  children: ReactNode;
}

// Unified collapsible panel for the run detail page: one header row (title
// left, toolbar + collapse affordance right) above the panel body. Replaces
// the per-section ad-hoc headers so every panel on the page shares one
// visual grammar — uppercase meta title, hairline divider, compact chevron.
export function DetailPanel({
  title,
  id,
  idToggle,
  toolbar,
  defaultCollapsed = false,
  collapsible = true,
  collapsed,
  onToggle,
  collapsedNote,
  bodyTestId,
  bodyIdOverride,
  collapsedTestId,
  className,
  bodyClassName,
  followed,
  followTestId,
  onFollowToggle,
  children,
}: DetailPanelProps) {
  const [uncontrolledCollapsed, setUncontrolledCollapsed] = useState(defaultCollapsed);
  const isControlled = collapsed !== undefined;
  const bodyCollapsed = isControlled ? collapsed : uncontrolledCollapsed;
  const bodyId = bodyIdOverride ?? `${id}-body`;
  const bodyTestid = bodyTestId === undefined ? bodyId : bodyTestId;
  return (
    <section
      data-testid={id}
      id={id}
      className={`relative rounded-lg border border-line bg-canvas ${className ?? ''}`}
    >
      <div className="flex items-center justify-between gap-2 border-b border-line px-3 py-1.5">
        <h3 className="text-meta font-semibold uppercase tracking-wide text-ink-muted">{title}</h3>
        <div className="flex items-center gap-2">
          {onFollowToggle && (
            <FollowButton
              label="Follow"
              pressed={followed ?? false}
              testId={followTestId ?? 'panel-follow-toggle'}
              title={followed ? 'Stop following the replayed step' : 'Follow the replayed step in this panel'}
              onToggle={onFollowToggle}
            />
          )}
          {toolbar}
          {collapsible && (
            <button
              type="button"
              data-testid={idToggle ?? `${id}-collapse`}
              aria-expanded={!bodyCollapsed}
              aria-controls={bodyId}
              title={bodyCollapsed ? 'Expand panel' : 'Collapse panel'}
              onClick={() => {
                if (isControlled) onToggle?.();
                else setUncontrolledCollapsed((c) => !c);
              }}
              className="rounded-md border border-line-strong p-1 text-ink-muted hover:bg-surface-raised hover:text-ink"
            >
              <svg
                aria-hidden
                viewBox="0 0 16 16"
                className={`h-3.5 w-3.5 transition-transform ${bodyCollapsed ? '' : 'rotate-180'}`}
                fill="none"
                stroke="currentColor"
                strokeWidth="1.5"
              >
                <path d="M4 6l4 4 4-4" />
              </svg>
            </button>
          )}
        </div>
      </div>
      {bodyCollapsed ? (
        collapsedNote !== undefined && (
          <p
            className="px-4 py-2 text-sm text-ink-muted"
            data-testid={collapsedTestId ?? `${id}-collapsed`}
          >
            {collapsedNote}
          </p>
        )
      ) : (
        <div id={bodyId} data-testid={bodyTestid === null ? undefined : bodyTestid} className={bodyClassName ?? 'p-3'}>
          {children}
        </div>
      )}
    </section>
  );
}