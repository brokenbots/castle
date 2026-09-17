import { useEffect, useRef, useState } from 'react';
import { getAuthToken } from '../authToken';

// Masks an agent token for display: only the trailing four characters are
// ever surfaced, never enough material to reconstruct the secret.
export function maskToken(token: string): string {
  const tail = token.length >= 4 ? token.slice(-4) : '';
  return `••••${tail}`;
}

interface TokenMenuProps {
  onLogout: () => void;
}

// Session menu anchored in the top bar: identifies the active agent token
// (masked) and is the only place "Log out" is reachable from.
export function TokenMenu({ onLogout }: TokenMenuProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

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
        Token <span className="font-mono">{masked}</span>
      </button>
      {open && (
        <div
          role="menu"
          aria-label="Token menu"
          data-testid="token-menu-panel"
          className="absolute right-0 top-full z-20 mt-2 w-56 rounded-md border border-line bg-surface shadow-xl"
        >
          <div className="border-b border-line px-3 py-2" role="none">
            <p className="text-meta uppercase tracking-wide text-ink-faint">Agent token</p>
            <p className="mt-1 font-mono text-meta text-ink-muted">{masked}</p>
          </div>
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