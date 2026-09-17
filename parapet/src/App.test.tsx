import { render, screen, act, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { delay, http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, test, vi } from 'vitest';
import { App } from './App';
import { store } from './store';
import { castleApi } from './api/castleApi';
import { selectAuthExpired, sessionRecovered, sessionExpired } from './features/auth/sessionSlice';
import { clearAuthToken, getAuthToken, setAuthToken } from './authToken';
import { RunListPage } from './features/runs/RunListPage';
import { RunDetailPage } from './features/runs/RunDetailPage';
import { AgentListPage } from './features/agents/AgentListPage';
import { AgentDetailPage } from './features/agents/AgentDetailPage';
import { server } from './test/mocks/server';
import { serverPath } from './test/mocks/handlers';
// Vite's ?raw import inlines the shipped index.html so the document title
// can be asserted against the real markup.
import indexHtml from '../index.html?raw';

// The run detail page starts a live watch stream; jsdom+msw have no real
// stream transport, so stub it the same way RunDetailPage tests do.
vi.mock('./features/runs/watchRun', () => ({
  startWatch: vi.fn().mockResolvedValue(undefined),
}));

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
            <Route path="runs/:id" element={<RunDetailPage />} />
            <Route path="agents" element={<AgentListPage />} />
            <Route path="agents/:criteriaId" element={<AgentDetailPage />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </Provider>,
  );
}

// Password login is the default mode; the agent-token probe tests must
// switch to the secondary agent-token mode first.
async function signInWithToken(token: string) {
  await userEvent.click(screen.getByTestId('login-mode-toggle'));
  await userEvent.type(screen.getByTestId('login-token'), token);
  await userEvent.click(screen.getByTestId('login-submit'));
}

// The module-level store is shared across tests: clear the RTK Query cache
// and stored token before every test so each assertion observes a real
// request lifecycle regardless of test order, and so every test runs the
// same when executed in isolation.
beforeEach(() => {
  clearAuthToken();
  // Module-level session state is shared across tests; clear any expired
  // flag a previous test left behind so the gate starts neutral.
  store.dispatch(sessionRecovered());
  store.dispatch(castleApi.util.resetApiState());
});

describe('App login gate', () => {
  test('renders the branded login page with the password form when no token is stored', () => {
    renderApp();

    expect(screen.getByTestId('login-brand')).toHaveTextContent('Parapet');
    // Password mode is the primary login; the agent-token field is one
    // toggle away.
    expect(screen.getByTestId('login-username')).toBeInTheDocument();
    expect(screen.getByTestId('login-password')).toBeInTheDocument();
    expect(screen.queryByTestId('login-token')).not.toBeInTheDocument();
    expect(screen.getByTestId('login-mode-toggle')).toBeInTheDocument();
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
    await signInWithToken('bad-token');

    const error = await screen.findByTestId('login-error');
    expect(error).toHaveTextContent('Castle rejected that token.');
    expect(error).toHaveAttribute('role', 'alert');
    expect(screen.getByTestId('login-token')).toHaveAttribute('aria-invalid', 'true');
    // Still gated: the shell never renders.
    expect(screen.queryByTestId('top-bar')).not.toBeInTheDocument();
    // A rejected token is never persisted.
    expect(getAuthToken()).toBe('');
  });

  test('shows a loading state while the token is validated', async () => {
    server.use(
      http.post(serverPath('ListAgents'), async () => {
        await delay(300);
        return HttpResponse.json({ agents: [], next_page_token: '' });
      }),
    );

    renderApp();
    await signInWithToken('valid-token');

    const submit = screen.getByTestId('login-submit');
    expect(submit).toHaveTextContent('Validating…');
    expect(submit).toBeDisabled();
    expect(screen.getByTestId('login-token')).toBeDisabled();
    expect(screen.queryByTestId('login-error')).not.toBeInTheDocument();
    // Let the probe complete within this test: an in-flight response that
    // resolves after the test ends would fire this gate's onAuthenticated
    // during the next test and pollute its storage assertions.
    expect(await screen.findByTestId('top-bar')).toBeInTheDocument();
  });

  test('validates the token and renders the shell on success', async () => {
    renderApp();
    await signInWithToken('valid-token-123456');

    expect(await screen.findByTestId('top-bar')).toBeInTheDocument();
    // The accepted token was persisted for the API transport.
    expect(getAuthToken()).toBe('valid-token-123456');
    expect(screen.queryByTestId('login-brand')).not.toBeInTheDocument();
  });

  // CRI-195: the human console path — username + password is the primary
  // login mode and lands on the same shell with the issued session token.
  test('signs in with username and password into the console shell', async () => {
    renderApp();
    const user = userEvent.setup();

    // Password mode is the default; no mode switch needed.
    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));

    expect(await screen.findByTestId('top-bar')).toBeInTheDocument();
    // The session token issued by castle's Login is what gets persisted —
    // identical post-login behavior to the agent-token path.
    expect(getAuthToken()).toBe('console-session-token-123456');
    expect(screen.queryByTestId('login-brand')).not.toBeInTheDocument();
  });

  test('a rejected password keeps the user at the gate without persisting anything', async () => {
    server.use(
      http.post(serverPath('Login'), () =>
        HttpResponse.json({ code: 'unauthenticated', message: 'invalid credentials' }, { status: 401 }),
      ),
    );
    renderApp();
    const user = userEvent.setup();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'wrong');
    await user.click(screen.getByTestId('login-submit'));

    const error = await screen.findByTestId('login-error');
    expect(error).toHaveTextContent('Incorrect username or password.');
    expect(error).toHaveAttribute('role', 'alert');
    // Still gated: the shell never renders and nothing is persisted.
    expect(screen.queryByTestId('top-bar')).not.toBeInTheDocument();
    expect(getAuthToken()).toBe('');
  });
});

describe('App shell', () => {
  beforeEach(() => {
    setAuthToken('test-token-12345678');
  });

  test('renders the top bar with product name, search, connection status and token menu', async () => {
    renderApp();

    const topBar = screen.getByTestId('top-bar');
    expect(within(topBar).getByTestId('product-name')).toHaveTextContent('Parapet');
    expect(
      within(topBar).getByPlaceholderText(/search runs, agents/i),
    ).toBeInTheDocument();
    expect(within(topBar).getByTestId('token-menu-button')).toBeInTheDocument();
    // With the cache reset per test this observes the real connecting →
    // online transition once the connection probe answers.
    const status = within(topBar).getByTestId('connection-status');
    expect(within(status).getByText('connecting')).toBeInTheDocument();
    expect(await within(status).findByText('online')).toBeInTheDocument();
    expect(status).toHaveAccessibleName('Castle connection: online');
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

  test('routes the agent detail page inside the shell outlet', async () => {
    renderApp('/agents/agent-1');

    const outlet = screen.getByTestId('shell-outlet');
    expect(await within(outlet).findByTestId('agent-detail')).toBeInTheDocument();
    expect(within(outlet).getByTestId('agent-criteria-id')).toHaveTextContent('agent-1');
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

  test('does not offer log out in the sidebar nav', () => {
    renderApp();

    const nav = screen.getByTestId('side-nav');
    expect(within(nav).queryByTestId('logout')).not.toBeInTheDocument();
    expect(within(nav).queryByText(/log out/i)).not.toBeInTheDocument();
  });
});

describe('document title', () => {
  test('the tab is branded Parapet — Castle', () => {
    expect(indexHtml).toContain('<title>Parapet — Castle</title>');
  });
});

describe('App auth expiry', () => {
  test('returns to the login page with an expiry notice when the session expires mid-session', async () => {
    renderApp();
    await signInWithToken('valid-token-123456');
    expect(await screen.findByTestId('top-bar')).toBeInTheDocument();

    act(() => store.dispatch(sessionExpired()));

    expect(screen.getByTestId('login-brand')).toBeInTheDocument();
    const notice = screen.getByTestId('login-notice');
    expect(notice).toHaveAttribute('role', 'status');
    expect(notice).toHaveTextContent('Your session expired. Sign in again to continue.');
    // The stale token is dropped so the API transport cannot reuse it.
    expect(getAuthToken()).toBe('');
  });

  test('clears the expiry flag after signing back in', async () => {
    renderApp();
    await signInWithToken('valid-token-123456');
    expect(await screen.findByTestId('top-bar')).toBeInTheDocument();

    act(() => store.dispatch(sessionExpired()));
    await signInWithToken('fresh-token-123456');

    expect(await screen.findByTestId('top-bar')).toBeInTheDocument();
    expect(getAuthToken()).toBe('fresh-token-123456');
    expect(selectAuthExpired(store.getState())).toBe(false);
    expect(screen.queryByTestId('login-notice')).not.toBeInTheDocument();
  });
});

// CRI-194: deep-linking to a run page used to trap users in a login loop —
// Castle rejected InspectRun for UI agent tokens and Parapet classified any
// unauthenticated rejection as session expiry, so the run detail route
// bounced straight back into the login gate with the same failing token.
describe('App run deep links', () => {
  beforeEach(() => {
    setAuthToken('test-token-12345678');
  });

  test('deep-linked run page renders an access state instead of the login gate when the inspection is denied', async () => {
    server.use(
      http.post(serverPath('InspectRun'), () =>
        HttpResponse.json(
          { code: 'permission_denied', message: 'caller does not own this run' },
          { status: 403 },
        ),
      ),
    );
    renderApp('/runs/run-1');

    // The run page itself renders normally...
    const outlet = screen.getByTestId('shell-outlet');
    expect(await within(outlet).findByRole('heading', { name: 'hello' })).toBeInTheDocument();
    // ...the inspection panel shows an explicit access state rather than a
    // generic failure...
    expect(await within(outlet).findByTestId('inspection-access-denied')).toBeInTheDocument();
    expect(within(outlet).queryByText('Inspection unavailable.')).not.toBeInTheDocument();
    // ...and a permission denial never flips the session gate: no login gate,
    // no cleared token, no expiry.
    expect(screen.queryByTestId('login-brand')).not.toBeInTheDocument();
    expect(selectAuthExpired(store.getState())).toBe(false);
    expect(getAuthToken()).toBe('test-token-12345678');
  });

  test('an unauthenticated rejection on a deep-linked route gates the user and a rejected token cannot re-enter the app', async () => {
    server.use(
      http.post(serverPath('GetRun'), () =>
        HttpResponse.json({ code: 'unauthenticated', message: 'invalid token' }, { status: 401 }),
      ),
      http.post(serverPath('InspectRun'), () =>
        HttpResponse.json({ code: 'unauthenticated', message: 'invalid token' }, { status: 401 }),
      ),
    );
    renderApp('/runs/run-1');

    // The unauthenticated rejection flips the session gate on the
    // deep-linked route and drops the stale token.
    expect(await screen.findByTestId('login-brand')).toBeInTheDocument();
    expect(screen.getByTestId('login-notice')).toHaveTextContent('Your session expired.');
    expect(getAuthToken()).toBe('');

    // Signing back in with a token Castle rejects stays at the gate with the
    // error — the token probe validates, so a bad token can never silently
    // re-enter the failing route.
    server.use(
      http.post(serverPath('ListAgents'), () =>
        HttpResponse.json({ code: 'unauthenticated', message: 'invalid token' }, { status: 401 }),
      ),
    );
    await signInWithToken('stale-token-123456');

    expect(await screen.findByTestId('login-error')).toHaveTextContent('Castle rejected that token.');
    expect(screen.queryByTestId('top-bar')).not.toBeInTheDocument();
    expect(getAuthToken()).toBe('');
  });
});
