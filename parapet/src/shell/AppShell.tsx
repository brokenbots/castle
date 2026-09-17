import { useState } from 'react';
import { Outlet } from 'react-router-dom';
import { TopBar } from './TopBar';
import { SideNav } from './SideNav';

// Application shell: top bar (product, search, connection, token menu) above
// a left nav rail and the routed page outlet.
export function AppShell({ onLogout }: { onLogout: () => void }) {
  const [collapsed, setCollapsed] = useState(false);
  return (
    <div className="flex h-full flex-col bg-canvas text-ink">
      <TopBar onLogout={onLogout} />
      <div className="flex min-h-0 flex-1">
        <SideNav collapsed={collapsed} onToggle={() => setCollapsed((c) => !c)} />
        <main data-testid="shell-outlet" className="min-w-0 flex-1 overflow-y-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}