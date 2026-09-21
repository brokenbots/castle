import type { ReactNode } from 'react';

interface PageHeaderProps {
  title: ReactNode;
  // Secondary line under the title (e.g. run id, timestamps).
  meta?: ReactNode;
  // Action row aligned with the title (buttons, controls, filters).
  actions?: ReactNode;
  // Metadata row rendered full-width below title and actions.
  children?: ReactNode;
}

// Shared page header: title, optional metadata line, action row, and an
// optional full-width metadata strip below. Every page renders its header
// through this component for a consistent layout.
export function PageHeader({ title, meta, actions, children }: PageHeaderProps) {
  return (
    <header data-testid="page-header" className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
      <div className="min-w-0">
        <h2 className="text-title font-semibold text-ink">{title}</h2>
        {meta ? <div className="mt-1 font-mono text-body text-ink-muted">{meta}</div> : null}
      </div>
      {actions ? <div className="flex flex-wrap items-center gap-3">{actions}</div> : null}
      {children ? <div className="w-full">{children}</div> : null}
    </header>
  );
}