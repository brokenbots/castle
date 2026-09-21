import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, test, vi } from 'vitest';
import { SideNav } from './SideNav';

describe('SideNav', () => {
  test('links to the standalone run viewer with a plain anchor', () => {
    // The standalone root lives outside parapet's router (a separate static
    // bundle at /runview/), so the entry must be a plain anchor: a NavLink
    // would client-route into a route parapet does not own.
    render(
      <MemoryRouter>
        <SideNav collapsed={false} onToggle={() => {}} />
      </MemoryRouter>,
    );

    const link = screen.getByTestId('runview-link');
    expect(link).toHaveAttribute('href', '/runview/');
    expect(link.tagName).toBe('A');
    expect(link).toHaveTextContent('Run viewer');
  });

  test('keeps the run-viewer link accessible while collapsed', () => {
    render(
      <MemoryRouter>
        <SideNav collapsed onToggle={vi.fn()} />
      </MemoryRouter>,
    );

    const link = screen.getByTestId('runview-link');
    expect(link).toHaveAttribute('href', '/runview/');
    expect(screen.getByText('Run viewer')).toHaveClass('sr-only');
  });
});