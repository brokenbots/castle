import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, test, vi } from 'vitest';
import { SideNav } from './SideNav';

function renderSideNav(collapsed: boolean) {
  return render(
    <MemoryRouter>
      <SideNav collapsed={collapsed} onToggle={vi.fn()} />
    </MemoryRouter>,
  );
}

describe('SideNav', () => {
  test('renders the grouped sections with router links', () => {
    renderSideNav(false);

    // The nav owns two sections: Runs and Agents, each a labelled group
    // carrying its route as a router link; the complete-link-set regression
    // test below enforces that every destination stays inside parapet's
    // router.
    const runsGroup = screen.getByRole('list', { name: 'Runs' });
    expect(runsGroup).toBeInTheDocument();
    const runsLink = screen.getByRole('link', { name: /all runs/i });
    expect(runsLink).toHaveAttribute('href', '/runs');
    expect(within(runsGroup).getByRole('link', { name: /all runs/i })).toBe(runsLink);

    const agentsGroup = screen.getByRole('list', { name: 'Agents' });
    expect(agentsGroup).toBeInTheDocument();
    expect(within(agentsGroup).getByRole('link', { name: /all agents/i })).toHaveAttribute(
      'href',
      '/agents',
    );
  });

  test('keeps the section labels accessible while collapsed', () => {
    renderSideNav(true);

    // Collapsed rail: section labels and link text collapse to sr-only so
    // assistive tech still reads them, and titles surface the label on
    // hover over the icon-only strip.
    expect(screen.getByText('Runs')).toHaveClass('sr-only');
    expect(screen.getByText('Agents')).toHaveClass('sr-only');
    expect(screen.getByRole('link', { name: /all runs/i })).toHaveAttribute('title', 'All runs');
    expect(screen.getByRole('link', { name: /all agents/i })).toHaveAttribute('title', 'All agents');
  });

  test('offers no castle-hosted standalone run-viewer entry', () => {
    // Regression (CRI-283): the standalone run viewer's data server lives
    // inside the criteria CLI's loopback, unreachable from a browser at the
    // castle ingress — a castle-hosted /runview/ entry is a dead link by
    // design and must never return. The nav's complete link set is asserted
    // so every destination provably stays inside parapet's router.
    renderSideNav(false);

    const nav = screen.getByTestId('side-nav');
    expect(screen.queryByTestId('runview-link')).not.toBeInTheDocument();
    expect(
      within(nav).queryAllByRole('link', { name: /run viewer|standalone/i }),
    ).toHaveLength(0);
    expect(within(nav).getAllByRole('link').map((link) => link.getAttribute('href'))).toEqual([
      '/runs',
      '/agents',
    ]);
  });
});