import { useEffect, useRef, useState } from 'react';
import { useCallerIdentity } from '@castle/run-viewer';
import { getAuthToken } from '../authToken';
import { applyTheme, readStoredTheme, type ParapetTheme } from '../theme';

// Masks an agent token for display: only the trailing four characters are
// ever surfaced, never enough material to reconstruct the secret.
export function maskToken(token: string): string {
  const tail = token.length >= 4 ? token.slice(-4) : '';
  return `••••${tail}`;
}

interface TokenMenuProps {
  onLogout: () => void;
}

// Renders the scope line for the caller's identity: membership-scoped
// identities name the projects their view covers, unscoped ones cover the
// whole project space. Never implies anything about other identities.
function scopeText(membershipScoped: boolean, membership: string[]): string {
  if (!membershipScoped) return 'all projects';
  return membership.length > 0 ? `scoped to ${membership.join(', ')}` : 'no projects in scope';
}

// Session menu anchored in the top bar. The badge leads with the caller's
// own operator identity (KB-333) — display name and role from the unified
// operator attributes, plus a scope summary in the panel — instead of the
// credential class the session travels as. The masked token remains a
// secondary session detail, and the token-only presentation returns
// whenever the identity read answers nothing so the menu keeps working
// against hosts that serve no identity.
export function TokenMenu({ onLogout }: TokenMenuProps) {
  const [open, setOpen] = useState(false);
  const [theme, setTheme] = useState<ParapetTheme>(() => readStoredTheme());
  const rootRef = useRef<HTMLDivElement>(null);
  const { data: identity } = useCallerIdentity();

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('mousedown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open]);

  const masked = maskToken(getAuthToken());
  const displayName = identity?.displayName || identity?.name || '';

  const toggleTheme = () => {
    const next: ParapetTheme = theme === 'dark' ? 'light' : 'dark';
    applyTheme(next);
    setTheme(next);
  };

  return (
    <div ref={rootRef} className="relative" data-testid="token-menu">
      <button
        type="button"
        data-testid="token-menu-button"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="flex items-center gap-2 rounded-md px-2 py-1.5 text-body text-ink-muted hover:bg-surface-raised hover:text-ink"
      >
        <span aria-hidden className="inline-block h-2 w-2 rounded-full bg-success" />
        {identity && displayName ? (
          <>
            <span data-testid="token-menu-identity">{displayName}</span>
            <span className="rounded bg-surface-raised px-1.5 py-0.5 text-meta font-medium text-ink-muted">
              {identity.role}
            </span>
          </>
        ) : (
          <>
            Token <span className="font-mono">{masked}</span>
          </>
        )}
      </button>
      {open && (
        <div
          role="menu"
          aria-label="Token menu"
          data-testid="token-menu-panel"
          className="absolute right-0 top-full z-20 mt-2 w-64 rounded-md border border-line bg-surface shadow-xl"
        >
          {identity ? (
            <div className="border-b border-line px-3 py-2" role="none">
              <p className="text-meta uppercase tracking-wide text-ink-faint">Signed in as</p>
              <p className="mt-1 text-body font-medium text-ink" data-testid="token-identity-name">
                {displayName || identity.operatorId}
              </p>
              <p className="mt-0.5 text-meta text-ink-muted">
                role <span data-testid="token-identity-role">{identity.role}</span>
                {' · '}
                <span data-testid="token-identity-scope">
                  {scopeText(identity.membershipScoped, identity.membership)}
                </span>
              </p>
            </div>
          ) : (
            <div className="border-b border-line px-3 py-2" role="none">
              <p className="text-meta uppercase tracking-wide text-ink-faint">Agent token</p>
              <p className="mt-1 font-mono text-meta text-ink-muted" data-testid="token-menu-token">
                {masked}
              </p>
            </div>
          )}
          {identity && (
            <div className="border-b border-line px-3 py-2" role="none">
              <p className="text-meta uppercase tracking-wide text-ink-faint">Session token</p>
              <p className="mt-1 font-mono text-meta text-ink-muted" data-testid="token-menu-token-identity">
                {masked}
              </p>
            </div>
          )}
          <button
            type="button"
            role="menuitem"
            data-testid="theme-toggle"
            onClick={toggleTheme}
            className="flex w-full items-center justify-between px-3 py-2 text-left text-body text-ink hover:bg-surface-raised"
          >
            Theme
            <span className="text-meta text-ink-muted" data-testid="theme-current">
              {theme === 'dark' ? 'Dark' : 'Light'}
            </span>
          </button>
          <button
            type="button"
            role="menuitem"
            data-testid="logout"
            onClick={() => {
              setOpen(false);
              onLogout();
            }}
            className="block w-full px-3 py-2 text-left text-body text-ink hover:bg-surface-raised"
          >
            Log out
          </button>
        </div>
      )}
    </div>
  );
}