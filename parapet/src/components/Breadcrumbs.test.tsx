import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, test } from 'vitest';
import { Breadcrumbs } from './Breadcrumbs';

describe('Breadcrumbs', () => {
  test('renders ancestor links and the current page crumb', () => {
    render(
      <MemoryRouter>
        <Breadcrumbs items={[{ label: 'Runs', to: '/runs' }, { label: 'hello' }]} />
      </MemoryRouter>,
    );

    const nav = screen.getByTestId('breadcrumbs');
    expect(nav).toHaveAttribute('aria-label', 'Breadcrumb');
    const runsLink = screen.getByRole('link', { name: 'Runs' });
    expect(runsLink).toHaveAttribute('href', '/runs');
    // The terminal crumb is the current page: text, not a link, marked for
    // assistive tech.
    const current = screen.getByText('hello');
    expect(current).not.toHaveAttribute('href');
    expect(current).toHaveAttribute('aria-current', 'page');
  });

  test('renders multiple ancestor links in order', () => {
    render(
      <MemoryRouter>
        <Breadcrumbs
          items={[{ label: 'Home', to: '/' }, { label: 'Runs', to: '/runs' }, { label: 'hello' }]}
        />
      </MemoryRouter>,
    );

    expect(screen.getByRole('link', { name: 'Home' })).toHaveAttribute('href', '/');
    expect(screen.getByRole('link', { name: 'Runs' })).toHaveAttribute('href', '/runs');
    expect(screen.getByText('hello')).toHaveAttribute('aria-current', 'page');
  });

  test('a single crumb renders as the current page without links', () => {
    render(
      <MemoryRouter>
        <Breadcrumbs items={[{ label: 'Agents' }]} />
      </MemoryRouter>,
    );

    expect(screen.getByTestId('breadcrumbs')).toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.getByText('Agents')).toHaveAttribute('aria-current', 'page');
  });
});