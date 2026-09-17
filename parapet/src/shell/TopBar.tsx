import { Link } from 'react-router-dom';
import { ConnectionStatus } from './ConnectionStatus';
import { TokenMenu } from './TokenMenu';

interface TopBarProps {
  onLogout: () => void;
}

// Product top bar: product name, global search placeholder, connection
// indicator, and the token (session) menu. Global search is a placeholder
// slot — no filtering is wired yet.
export function TopBar({ onLogout }: TopBarProps) {
  return (
    <header
      data-testid="top-bar"
      className="flex h-14 shrink-0 items-center gap-4 border-b border-line bg-surface px-4"
    >
      <Link
        to="/runs"
        data-testid="product-name"
        className="flex items-center gap-2 text-body font-semibold tracking-wide text-ink"
      >
        <span
          aria-hidden
          className="inline-block h-3 w-3 rotate-45 rounded-sm border-2 border-accent-strong"
        />
        Parapet
      </Link>
      <form role="search" onSubmit={(e) => e.preventDefault()}>
        <input
          type="search"
          data-testid="global-search"
          aria-label="Global search"
          placeholder="Search runs, agents…"
          title="Search (coming soon)"
          className="w-64 rounded-md border border-line bg-canvas px-3 py-1.5 text-body text-ink placeholder:text-ink-faint"
        />
      </form>
      <div className="ml-auto flex items-center gap-4">
        <ConnectionStatus />
        <TokenMenu onLogout={onLogout} />
      </div>
    </header>
  );
}