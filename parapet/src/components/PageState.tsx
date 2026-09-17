import type { ReactNode } from 'react';
import { useDispatch } from 'react-redux';
import type { PageErrorKind } from '../api/errors';
import { sessionExpired } from '../features/auth/sessionSlice';
import type { AppDispatch } from '../store';

// Shared page-state presentation for the data pages: one component covers
// the loading, empty and error conditions so every page renders them the
// same way. Error states carry a retry affordance (wired by the caller to
// RTK Query refetch) and — when the error is an auth rejection — a
// re-authentication prompt instead of a dead end.

interface PageStateProps {
  loading?: boolean;
  empty?: boolean;
  error?: boolean;
  /** Headline copy: the loading label, or the empty/error headline. */
  title?: string;
  /** Supporting copy rendered under the headline. */
  detail?: string;
  /** Error classification; with `error` it drives the re-auth prompt. */
  kind?: PageErrorKind;
  /** Retry affordance; wired to RTK Query refetch by the caller. */
  onRetry?: () => void;
  /** Extra affordance rendered below the copy (e.g. a back link). */
  action?: ReactNode;
  /** Stable test hook for the container. */
  testId?: string;
}

const ERROR_DEFAULTS: Record<PageErrorKind, { title: string; detail: string }> = {
  unauthenticated: {
    title: 'Session expired',
    detail: 'Your token was rejected by Castle. Sign in again to continue.',
  },
  not_found: {
    title: 'Not found',
    detail: "This record doesn't exist or was removed.",
  },
  forbidden: {
    title: 'Access denied',
    detail: 'Your token is not allowed to read this.',
  },
  server: {
    title: 'Something went wrong',
    detail: 'Castle is unreachable or failed to answer. Try again.',
  },
  unknown: {
    title: 'Something went wrong',
    detail: 'An unexpected error occurred. Try again.',
  },
};

export function PageState({
  empty = false,
  error = false,
  title,
  detail,
  kind,
  onRetry,
  action,
  testId,
}: PageStateProps) {
  const dispatch = useDispatch<AppDispatch>();
  // Precedence: error > empty > everything else. `loading` (and the degenerate
  // no-prop call) falls through to the loading presentation.
  const mode = error ? 'error' : empty ? 'empty' : 'loading';
  const testIdFor = testId ?? `page-state-${mode}`;

  if (mode === 'loading') {
    return (
      <div data-testid={testIdFor} data-mode="loading" role="status" aria-live="polite" className="py-8 text-body text-ink-muted">
        {title ?? 'Loading…'}
      </div>
    );
  }

  if (mode === 'empty') {
    return (
      <div data-testid={testIdFor} data-mode="empty" role="status" className="rounded-lg border border-line bg-surface px-6 py-10 text-center">
        <p className="text-title font-semibold text-ink">{title ?? 'Nothing here yet'}</p>
        {detail && <p className="mx-auto mt-1 max-w-md text-body text-ink-muted">{detail}</p>}
        {action && <div className="mt-4 flex justify-center">{action}</div>}
      </div>
    );
  }

  const errorKind: PageErrorKind = kind ?? 'unknown';
  const isAuth = errorKind === 'unauthenticated';
  const defaults = ERROR_DEFAULTS[errorKind];

  return (
    <div data-testid={testIdFor} data-mode="error" data-kind={errorKind} role="alert" className="rounded-lg border border-line bg-surface px-6 py-10 text-center">
      <p className="text-title font-semibold text-ink">{title ?? defaults.title}</p>
      {(detail ?? defaults.detail) && (
        <p className="mx-auto mt-1 max-w-md text-body text-ink-muted">{detail ?? defaults.detail}</p>
      )}
      <div className="mt-4 flex flex-wrap items-center justify-center gap-3">
        {isAuth && (
          <button
            type="button"
            data-testid="page-state-reauth"
            className="rounded-md bg-accent px-3 py-1.5 text-body font-medium text-white hover:bg-accent-strong"
            onClick={() => dispatch(sessionExpired())}
          >
            Sign in again
          </button>
        )}
        {onRetry && !isAuth && (
          <button
            type="button"
            data-testid="page-state-retry"
            className="rounded-md border border-line-strong px-3 py-1.5 text-body text-ink hover:bg-surface-raised"
            onClick={onRetry}
          >
            Try again
          </button>
        )}
        {action && <div className="flex items-center">{action}</div>}
      </div>
    </div>
  );
}