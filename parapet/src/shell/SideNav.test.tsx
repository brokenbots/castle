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
    // carrying its route as a router link (never a plain anchor — every
    // destination is inside parapet's router).
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
});