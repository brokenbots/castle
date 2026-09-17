import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { delay, http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, test } from 'vitest';
import { App } from './App';
import { store } from './store';
import { getAuthToken, setAuthToken } from './authToken';
import { RunListPage } from './features/runs/RunListPage';
import { AgentListPage } from './features/agents/AgentListPage';
import { server } from './test/mocks/server';
import { serverPath } from './test/mocks/handlers';

// App mounts as a routed layout: login when no token is stored, otherwise
// the application shell wrapping the routed pages.
function renderApp(initialPath = '/') {
  return render(
    <Provider store={store}>
      <MemoryRouter
        initialEntries={[initialPath]}
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <Routes>
          <Route path="/" element={<App />}>
            <Route path="runs" element={<RunListPage />} />
            <Route path="agents" element={<AgentListPage />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </Provider>,
  );
}

async function signIn(token: string) {
  await userEvent.type(screen.getByTestId('login-token'), token);
  await userEvent.click(screen.getByTestId('login-submit'));
}

describe('App login gate', () => {
  test('renders the branded login page when no token is stored', () => {
    renderApp();

    expect(screen.getByTestId('login-brand')).toHaveTextContent('Parapet');
    const input = screen.getByTestId('login-token');
    expect(input).not.toBeInvalid();
    expect(screen.getByTestId('login-submit')).toHaveTextContent('Sign in');
  });

  test('shows an error state when Castle rejects the token', async () => {
    server.use(
      http.post(serverPath('ListAgents'), () =>
        HttpResponse.json(
          { code: 'unauthenticated', message: 'invalid token' },
          { status: 401 },
        ),
      ),
    );

    renderApp();
    await signIn('bad-token');

    const error = await screen.findByTestId('login-error');
    expect(error).toHaveTextContent('Castle rejected that token.');
    expect(error).toHaveAttribute('role', 'alert');
    expect(screen.getByTestId('login-token')).toHaveAttribute('aria-invalid', 'true');
    // Still gated: the shell never renders.
    expect(screen.queryByTestId('top-bar')).not.toBeInTheDocument();
  });

  test('shows a loading state while the token is validated', async () => {
    server.use(
      http.post(serverPath('ListAgents'), async () => {
        await delay(1000);
        return HttpResponse.json({ agents: [], next_page_token: '' });
      }),
    );

    renderApp();
    await signIn('valid-token');

    const submit = screen.getByTestId('login-submit');
    expect(submit).toHaveTextContent('Validating…');
    expect(submit).toBeDisabled();
    expect(screen.getByTestId('login-token')).toBeDisabled();
    expect(screen.queryByTestId('login-error')).not.toBeInTheDocument();
  });

  test('validates the token and renders the shell on success', async () => {
    renderApp();
    await signIn('valid-token-123456');

    expect(await screen.findByTestId('top-bar')).toBeInTheDocument();
    // The accepted token was persisted for the API transport.
    expect(getAuthToken()).toBe('valid-token-123456');
    expect(screen.queryByTestId('login-brand')).not.toBeInTheDocument();
  });
});

describe('App shell', () => {
  beforeEach(() => {
    setAuthToken('test-token-12345678');
  });

  test('renders the top bar with product name, search, connection status and token menu', () => {
    renderApp();

    const topBar = screen.getByTestId('top-bar');
    expect(within(topBar).getByTestId('product-name')).toHaveTextContent('Parapet');
    expect(
      within(topBar).getByPlaceholderText(/search runs, agents/i),
    ).toBeInTheDocument();
    const status = within(topBar).getByTestId('connection-status');
    expect(status).toHaveAccessibleName(/online/i);
    expect(within(topBar).getByTestId('token-menu-button')).toBeInTheDocument();
  });

  test('renders grouped nav sections and routes pages inside the outlet', async () => {
    renderApp('/runs');

    const nav = screen.getByTestId('side-nav');
    expect(within(nav).getByRole('link', { name: /all runs/i })).toHaveAttribute('href', '/runs');
    expect(within(nav).getByRole('link', { name: /all agents/i })).toHaveAttribute('href', '/agents');
    // Grouped sections: Runs and Agents are labelled groups inside the nav.
    const runsGroup = within(nav).getByRole('list', {
      name: /runs/i,
    });
    expect(runsGroup).toBeInTheDocument();
    expect(within(nav).getByRole('list', { name: /agents/i })).toBeInTheDocument();

    // The routed page renders inside the shell outlet.
    const outlet = screen.getByTestId('shell-outlet');
    expect(await within(outlet).findByText('hello')).toBeInTheDocument();
    expect(within(outlet).getByRole('heading', { name: 'Runs' })).toBeInTheDocument();
  });

  test('routes the agents page inside the shell outlet', async () => {
    renderApp('/agents');

    const outlet = screen.getByTestId('shell-outlet');
    expect(await within(outlet).findByRole('heading', { name: 'Agents' })).toBeInTheDocument();
    expect(await within(outlet).findByText('local')).toBeInTheDocument();
  });

  test('collapses and expands the nav rail', async () => {
    const user = userEvent.setup();
    renderApp();

    const nav = screen.getByTestId('side-nav');
    expect(nav).toHaveAttribute('data-collapsed', 'false');

    await user.click(screen.getByTestId('nav-toggle'));
    expect(nav).toHaveAttribute('data-collapsed', 'true');
    expect(screen.getByTestId('nav-toggle')).toHaveAttribute('aria-expanded', 'false');

    await user.click(screen.getByTestId('nav-toggle'));
    expect(nav).toHaveAttribute('data-collapsed', 'false');
    expect(screen.getByTestId('nav-toggle')).toHaveAttribute('aria-expanded', 'true');
  });

  test('logs out from the token menu back to the login gate', async () => {
    const user = userEvent.setup();
    renderApp();

    await user.click(screen.getByTestId('token-menu-button'));
    await user.click(screen.getByTestId('logout'));

    // The token is cleared and the login gate returns.
    expect(getAuthToken()).toBe('');
    expect(await screen.findByTestId('login-brand')).toBeInTheDocument();
    expect(screen.queryByTestId('top-bar')).not.toBeInTheDocument();
  });
});