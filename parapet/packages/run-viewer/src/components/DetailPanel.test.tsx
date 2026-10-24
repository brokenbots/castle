import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, test } from 'vitest';
import { DetailPanel } from './DetailPanel';

describe('DetailPanel', () => {
  test('renders title, toolbar and body with stable testids', () => {
    render(
      <DetailPanel id="my-panel" title="Events" toolbar={<button>Place</button>}>
        <p>body content</p>
      </DetailPanel>,
    );
    expect(screen.getByTestId('my-panel')).toBeInTheDocument();
    expect(screen.getByTestId('my-panel-body')).toHaveTextContent('body content');
    expect(screen.getByRole('button', { name: 'Place' })).toBeInTheDocument();
    // Collapsible by default.
    expect(screen.getByTestId('my-panel-collapse')).toHaveAttribute('aria-expanded', 'true');
  });

  test('collapsing hides the body and flips aria-expanded', async () => {
    const user = userEvent.setup();
    render(
      <DetailPanel id="my-panel" title="Events">
        <p>body content</p>
      </DetailPanel>,
    );
    await user.click(screen.getByTestId('my-panel-collapse'));
    expect(screen.queryByTestId('my-panel-body')).not.toBeInTheDocument();
    expect(screen.getByTestId('my-panel-collapse')).toHaveAttribute('aria-expanded', 'false');
    await user.click(screen.getByTestId('my-panel-collapse'));
    expect(screen.getByTestId('my-panel-body')).toBeInTheDocument();
  });

  test('starts collapsed when defaultCollapsed and supports a non-collapsible mode', async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <DetailPanel id="my-panel" title="Events" defaultCollapsed>
        <p>body content</p>
      </DetailPanel>,
    );
    expect(screen.queryByTestId('my-panel-body')).not.toBeInTheDocument();
    expect(screen.getByTestId('my-panel-collapse')).toHaveAttribute('aria-expanded', 'false');

    rerender(
      <div>
        <DetailPanel id="other-panel" title="Fixed" collapsible={false}>
          <p>always visible</p>
        </DetailPanel>
      </div>,
    );
    expect(screen.getByTestId('other-panel-body')).toBeInTheDocument();
    expect(screen.queryByTestId('other-panel-collapse')).not.toBeInTheDocument();
    expect(user).toBeTruthy();
  });

  test('honors a legacy toggle testid via idToggle', () => {
    render(
      <DetailPanel id="scope-controls" idToggle="scope-controls-collapse" title="Scope">
        <p>x</p>
      </DetailPanel>,
    );
    expect(screen.getByTestId('scope-controls-collapse')).toBeInTheDocument();
    expect(screen.queryByTestId('scope-controls-toggle')).not.toBeInTheDocument();
  });
});