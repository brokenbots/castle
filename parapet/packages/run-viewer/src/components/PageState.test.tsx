import { describe, expect, test, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { PageState } from './PageState';
import { selectAuthExpired } from '../features/auth/sessionSlice';
import { createRunViewerStore } from '../store';
import { sessionRecovered } from '../features/auth/sessionSlice';

// One store instance per test file; RTK Query caches per store.
const store = createRunViewerStore();


// PageState dispatches on the re-auth path, so every render goes through the
// real store provider.
const renderUi = (ui: React.ReactElement) => render(<Provider store={store}>{ui}</Provider>);

describe('PageState', () => {
  test('loading renders a polite status with default copy', () => {
    renderUi(<PageState loading />);
    const el = screen.getByTestId('page-state-loading');
    expect(el).toHaveAttribute('data-mode', 'loading');
    expect(el).toHaveAttribute('role', 'status');
    expect(el).toHaveTextContent('Loading…');
  });

  test('loading renders custom copy when given', () => {
    renderUi(<PageState loading title="Loading agent…" />);
    expect(screen.getByTestId('page-state-loading')).toHaveTextContent('Loading agent…');
  });

  test('empty renders guidance headline and detail', () => {
    renderUi(<PageState empty title="No runs yet." detail="Runs appear here once Castle starts executing them." />);
    const el = screen.getByTestId('page-state-empty');
    expect(el).toHaveAttribute('data-mode', 'empty');
    expect(el).toHaveTextContent('No runs yet.');
    expect(el).toHaveTextContent('Runs appear here once Castle starts executing them.');
  });

  test.each([
    ['server', 'Something went wrong'],
    ['not_found', 'Not found'],
    ['forbidden', 'Access denied'],
    ['unknown', 'Something went wrong'],
  ] as const)('error mode (%s) renders an alert with retry affordance', (kind, headline) => {
    const onRetry = vi.fn();
    renderUi(<PageState error kind={kind} onRetry={onRetry} />);
    const el = screen.getByTestId('page-state-error');
    expect(el).toHaveAttribute('data-mode', 'error');
    expect(el).toHaveAttribute('data-kind', kind);
    expect(el).toHaveAttribute('role', 'alert');
    expect(el).toHaveTextContent(headline);
    expect(screen.getByTestId('page-state-retry')).toHaveTextContent('Try again');
  });

  test('retry button invokes the caller-provided refetch handler', async () => {
    const user = userEvent.setup();
    const onRetry = vi.fn();
    renderUi(<PageState error kind="server" onRetry={onRetry} />);
    await user.click(screen.getByTestId('page-state-retry'));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  test('unauthenticated errors render a re-auth prompt instead of a retry', async () => {
    const user = userEvent.setup();
    store.dispatch(sessionRecovered());
    renderUi(<PageState error kind="unauthenticated" />);
    const el = screen.getByTestId('page-state-error');
    expect(el).toHaveTextContent('Session expired');
    expect(screen.queryByTestId('page-state-retry')).toBeNull();
    await user.click(screen.getByTestId('page-state-reauth'));
    expect(selectAuthExpired(store.getState())).toBe(true);
    store.dispatch(sessionRecovered());
  });

  test('custom title/detail override the kind defaults', () => {
    renderUi(<PageState error kind="server" title="Failed to load runs." detail="Castle did not answer." onRetry={() => {}} />);
    const el = screen.getByTestId('page-state-error');
    expect(el).toHaveTextContent('Failed to load runs.');
    expect(el).toHaveTextContent('Castle did not answer.');
  });

  test('renders caller action node in empty and error modes', () => {
    renderUi(<PageState empty action={<button type="button">Do a thing</button>} />);
    expect(screen.getByRole('button', { name: 'Do a thing' })).toBeTruthy();
    renderUi(<PageState error kind="not_found" action={<a href="#back">Back</a>} />);
    expect(screen.getByRole('link', { name: 'Back' })).toBeTruthy();
  });
});