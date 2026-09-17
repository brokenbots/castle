import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, test, vi } from 'vitest';
import { LoginPage } from './LoginPage';
import { getAuthToken, setAuthToken, clearAuthToken } from '../authToken';
import { server } from '../test/mocks/server';
import { serverPath } from '../test/mocks/handlers';

function renderLogin(props?: { notice?: string }) {
  const onAuthenticated = vi.fn();
  render(<LoginPage onAuthenticated={onAuthenticated} notice={props?.notice} />);
  return { onAuthenticated };
}

// The shared storage key must not leak between tests that assert on it.
beforeEach(() => {
  clearAuthToken();
});

describe('LoginPage password mode (default, CRI-195)', () => {
  test('renders the username + password form as the primary path', () => {
    renderLogin();

    expect(screen.getByTestId('login-username')).toBeInTheDocument();
    expect(screen.getByTestId('login-password')).toBeInTheDocument();
    expect(screen.queryByTestId('login-token')).not.toBeInTheDocument();
    expect(screen.getByTestId('login-submit')).toHaveTextContent('Sign in');
    // The password input masks input.
    expect(screen.getByTestId('login-password')).toHaveAttribute('type', 'password');
  });

  test('submit stays disabled until both username and password are entered', async () => {
    const user = userEvent.setup();
    renderLogin();

    const submit = screen.getByTestId('login-submit');
    expect(submit).toBeDisabled();

    await user.type(screen.getByTestId('login-username'), 'operator');
    expect(submit).toBeDisabled();
    await user.type(screen.getByTestId('login-password'), 'op-password');
    expect(submit).toBeEnabled();
  });

  test('successful credentials hand the issued session token to onAuthenticated', async () => {
    const user = userEvent.setup();
    const { onAuthenticated } = renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));

    // The session token returned by castle's Login is what gets handed up —
    // the same callback (and thus the same storage key) the agent-token path
    // uses, so both modes boot the app identically.
    expect(onAuthenticated).toHaveBeenCalledWith('console-session-token-123456');
    expect(onAuthenticated).toHaveBeenCalledTimes(1);
    // The gate component itself never writes storage; persisting is App's
    // job via onAuthenticated.
    expect(getAuthToken()).toBe('');
  });

  test('wrong credentials show an unauthenticated error and never persist anything', async () => {
    const user = userEvent.setup();
    const { onAuthenticated } = renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'wrong-password');
    await user.click(screen.getByTestId('login-submit'));

    const error = await screen.findByTestId('login-error');
    expect(error).toHaveTextContent('Incorrect username or password.');
    expect(error).toHaveAttribute('role', 'alert');
    expect(screen.getByTestId('login-password')).toHaveAttribute('aria-invalid', 'true');
    expect(onAuthenticated).not.toHaveBeenCalled();
    expect(getAuthToken()).toBe('');
  });

  test('a disabled console login surfaces an explicit, actionable error', async () => {
    server.use(
      http.post(serverPath('Login'), () =>
        HttpResponse.json(
          { code: 'unimplemented', message: 'console login is disabled' },
          { status: 501 },
        ),
      ),
    );
    const user = userEvent.setup();
    const { onAuthenticated } = renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));

    const error = await screen.findByTestId('login-error');
    expect(error).toHaveTextContent('Console login is disabled on this Castle.');
    expect(error).toHaveTextContent('CASTLE_CONSOLE_USER');
    expect(onAuthenticated).not.toHaveBeenCalled();
  });

  test('a failed precondition response is treated as disabled login too', async () => {
    server.use(
      http.post(serverPath('Login'), () =>
        HttpResponse.json(
          { code: 'failed_precondition', message: 'console login not configured' },
          { status: 400 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));

    expect(await screen.findByTestId('login-error')).toHaveTextContent('Console login is disabled');
  });

  test('shows a signing-in state while credentials are validated', async () => {
    server.use(
      http.post(serverPath('Login'), async () => {
        await delay(1000);
        return HttpResponse.json({ session_token: 'tok', username: 'operator' });
      }),
    );
    const user = userEvent.setup();
    renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));

    const submit = screen.getByTestId('login-submit');
    expect(submit).toHaveTextContent('Signing in…');
    expect(submit).toBeDisabled();
    expect(screen.getByTestId('login-username')).toBeDisabled();
    expect(screen.getByTestId('login-password')).toBeDisabled();
    expect(screen.queryByTestId('login-error')).not.toBeInTheDocument();
  });
});

describe('LoginPage agent-token mode (secondary option)', () => {
  test('mode toggle switches between the two forms and back', async () => {
    const user = userEvent.setup();
    renderLogin();

    await user.click(screen.getByTestId('login-mode-toggle'));
    expect(screen.getByTestId('login-token')).toBeInTheDocument();
    expect(screen.queryByTestId('login-username')).not.toBeInTheDocument();
    expect(screen.getByTestId('login-mode-toggle')).toHaveTextContent('Use username and password instead');

    await user.click(screen.getByTestId('login-mode-toggle'));
    expect(screen.getByTestId('login-username')).toBeInTheDocument();
    expect(screen.queryByTestId('login-token')).not.toBeInTheDocument();
  });

  test('switching modes clears the error', async () => {
    server.use(
      http.post(serverPath('Login'), () =>
        HttpResponse.json({ code: 'unauthenticated', message: 'nope' }, { status: 401 }),
      ),
    );
    const user = userEvent.setup();
    renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'bad');
    await user.click(screen.getByTestId('login-submit'));
    expect(await screen.findByTestId('login-error')).toBeInTheDocument();

    await user.click(screen.getByTestId('login-mode-toggle'));
    expect(screen.queryByTestId('login-error')).not.toBeInTheDocument();
  });

  test('agent token login validates via the ListAgents probe and boots identically', async () => {
    const user = userEvent.setup();
    const { onAuthenticated } = renderLogin();

    await user.click(screen.getByTestId('login-mode-toggle'));
    await user.type(screen.getByTestId('login-token'), 'valid-token-123456');
    await user.click(screen.getByTestId('login-submit'));

    expect(onAuthenticated).toHaveBeenCalledWith('valid-token-123456');
    expect(onAuthenticated).toHaveBeenCalledTimes(1);
  });

  test('a rejected agent token keeps the token-mode error text', async () => {
    server.use(
      http.post(serverPath('ListAgents'), () =>
        HttpResponse.json({ code: 'unauthenticated', message: 'invalid token' }, { status: 401 }),
      ),
    );
    const user = userEvent.setup();
    const { onAuthenticated } = renderLogin();

    await user.click(screen.getByTestId('login-mode-toggle'));
    await user.type(screen.getByTestId('login-token'), 'bad-token');
    await user.click(screen.getByTestId('login-submit'));

    const error = await screen.findByTestId('login-error');
    expect(error).toHaveTextContent('Castle rejected that token.');
    expect(onAuthenticated).not.toHaveBeenCalled();
  });

  test('both modes produce the same stored-token session through onAuthenticated', async () => {
    const user = userEvent.setup();
    const collect = (token: string) => {
      setAuthToken(token);
    };
    const { unmount } = render(<LoginPage onAuthenticated={collect} />);

    // Password mode stores the issued session token.
    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));
    await vi.waitFor(() => expect(getAuthToken()).toBe('console-session-token-123456'));

    // A fresh gate (as App would render after logout), switched to the
    // agent-token mode, stores the validated token under the same key.
    unmount();
    clearAuthToken();
    render(<LoginPage onAuthenticated={collect} />);
    await user.click(screen.getByTestId('login-mode-toggle'));
    await user.type(screen.getByTestId('login-token'), 'agent-token-xyz');
    await user.click(screen.getByTestId('login-submit'));
    await vi.waitFor(() => expect(getAuthToken()).toBe('agent-token-xyz'));
  });
});

describe('LoginPage shared surface', () => {
  test('shows the expiry notice above the form', () => {
    renderLogin({ notice: 'Your session expired. Sign in again to continue.' });
    const notice = screen.getByTestId('login-notice');
    expect(notice).toHaveAttribute('role', 'status');
    expect(notice).toHaveTextContent('Your session expired.');
  });

  test('non-auth connect failures surface an unreachable message', async () => {
    server.use(
      http.post(serverPath('Login'), () =>
        HttpResponse.json({ code: 'unavailable', message: 'connection refused' }, { status: 503 }),
      ),
    );
    const user = userEvent.setup();
    renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));

    expect(await screen.findByTestId('login-error')).toHaveTextContent('Castle is unreachable');
  });

  test('unexpected error shapes still surface as errors, never as success', async () => {
    server.use(
      http.post(serverPath('Login'), () => HttpResponse.error()),
    );
    const user = userEvent.setup();
    const { onAuthenticated } = renderLogin();

    await user.type(screen.getByTestId('login-username'), 'operator');
    await user.type(screen.getByTestId('login-password'), 'op-password');
    await user.click(screen.getByTestId('login-submit'));

    expect(await screen.findByTestId('login-error')).toBeInTheDocument();
    expect(onAuthenticated).not.toHaveBeenCalled();
  });
});