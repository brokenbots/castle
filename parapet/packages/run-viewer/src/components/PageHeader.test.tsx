import { render, screen } from '@testing-library/react';
import { describe, expect, test } from 'vitest';
import { PageHeader } from './PageHeader';

describe('PageHeader', () => {
  test('renders title, meta, actions and metadata slots', () => {
    render(
      <PageHeader
        title="hello"
        meta="run-1"
        actions={<button type="button">Pause</button>}
      >
        <div>ticket: CRI-190</div>
      </PageHeader>,
    );

    const header = screen.getByTestId('page-header');
    expect(header).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 2, name: 'hello' })).toBeInTheDocument();
    expect(screen.getByText('run-1')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument();
    expect(screen.getByText('ticket: CRI-190')).toBeInTheDocument();
  });

  test('renders title only when optional slots are omitted', () => {
    render(<PageHeader title="Agents" />);

    expect(screen.getByRole('heading', { name: 'Agents' })).toBeInTheDocument();
    expect(screen.getByTestId('page-header').children).toHaveLength(1);
  });
});