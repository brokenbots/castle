import { NavLink } from 'react-router-dom';

interface SideNavProps {
  collapsed: boolean;
  onToggle: () => void;
}

interface NavSection {
  label: string;
  items: { to: string; label: string; icon: JSX.Element }[];
}

function RunsIcon() {
  return (
    <svg aria-hidden viewBox="0 0 16 16" className="h-4 w-4 shrink-0" fill="currentColor">
      <path d="M4 2.5v11l9-5.5-9-5.5z" />
    </svg>
  );
}

function AgentsIcon() {
  return (
    <svg aria-hidden viewBox="0 0 16 16" className="h-4 w-4 shrink-0" fill="none" stroke="currentColor" strokeWidth="1.5">
      <rect x="3" y="3" width="10" height="10" rx="2" />
      <path d="M3 7.5h10M7.5 3v10" />
    </svg>
  );
}

function SettingsIcon() {
  return (
    <svg aria-hidden viewBox="0 0 16 16" className="h-4 w-4 shrink-0" fill="none" stroke="currentColor" strokeWidth="1.5">
      <circle cx="8" cy="8" r="2.5" />
      <path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M12.6 3.4l-1.4 1.4M4.8 11.2l-1.4 1.4" />
    </svg>
  );
}

// Grouped left navigation. Sections (Runs, Agents, Settings console) carry
// their links; the rail can collapse to an icon-only strip. When collapsed,
// labels stay available to assistive tech via sr-only text and title
// tooltips. The settings console is its own area: the run-viewer feature
// (runs, agents) stays viewer-shaped, and project administration lives here.
const SECTIONS: NavSection[] = [
  { label: 'Runs', items: [{ to: '/runs', label: 'All runs', icon: <RunsIcon /> }] },
  { label: 'Agents', items: [{ to: '/agents', label: 'All agents', icon: <AgentsIcon /> }] },
  {
    label: 'Settings console',
    items: [{ to: '/settings/projects', label: 'Projects', icon: <SettingsIcon /> }],
  },
];

export function SideNav({ collapsed, onToggle }: SideNavProps) {
  const linkClass = ({ isActive }: { isActive: boolean }) =>
    `flex items-center gap-2 rounded-md text-body ${
      collapsed ? 'justify-center p-2' : 'px-3 py-2'
    } ${isActive ? 'bg-surface-raised text-ink' : 'text-ink-muted hover:bg-surface-raised hover:text-ink'}`;

  return (
    <nav
      id="side-nav"
      data-testid="side-nav"
      data-collapsed={collapsed}
      aria-label="Primary"
      className={`flex shrink-0 flex-col gap-1 overflow-hidden border-r border-line bg-surface py-3 ${
        collapsed ? 'w-14' : 'w-56'
      }`}
    >
      {SECTIONS.map((section) => (
        <div key={section.label} className="px-2 py-1">
          <p
            className={`px-1 pb-1 text-meta font-semibold uppercase tracking-wide text-ink-faint ${
              collapsed ? 'sr-only' : ''
            }`}
          >
            {section.label}
          </p>
          <ul className="flex flex-col gap-1" aria-label={section.label}>
            {section.items.map((item) => (
              <li key={item.to}>
                <NavLink to={item.to} className={linkClass} title={collapsed ? item.label : undefined}>
                  {item.icon}
                  <span className={collapsed ? 'sr-only' : ''}>{item.label}</span>
                </NavLink>
              </li>
            ))}
          </ul>
        </div>
      ))}
      <button
        type="button"
        data-testid="nav-toggle"
        aria-controls="side-nav"
        aria-expanded={!collapsed}
        aria-label={collapsed ? 'Expand navigation' : 'Collapse navigation'}
        onClick={onToggle}
        className="mt-auto mx-2 flex items-center justify-center gap-2 rounded-md p-2 text-body text-ink-muted hover:bg-surface-raised hover:text-ink"
      >
        <svg aria-hidden viewBox="0 0 16 16" className={`h-4 w-4 shrink-0 ${collapsed ? '' : 'rotate-180'}`} fill="none" stroke="currentColor" strokeWidth="1.5">
          <path d="M10 3 5 8l5 5" />
        </svg>
        <span className={collapsed ? 'sr-only' : ''}>Collapse</span>
      </button>
    </nav>
  );
}