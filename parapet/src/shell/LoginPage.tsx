import { FormEvent, useState } from 'react';
import { Code, ConnectError } from '@connectrpc/connect';
import { server } from '../api/client';

interface LoginPageProps {
  onAuthenticated: (token: string) => void;
  /** Shown above the form — e.g. a session-expired notice after a 401. */
  notice?: string;
}

type LoginMode = 'password' | 'agent-token';

// CRI-195: the console's user is a human, so username + password is the
// primary path (castle's Login RPC with CASTLE_CONSOLE_USER/PASSWORD); the
// agent token stays available as the secondary option for machine-adjacent
// operators. Both modes end the same way: onAuthenticated stores the session
// token under the same key and the app boots identically.
function describeError(err: unknown, mode: LoginMode): string {
  if (err instanceof ConnectError) {
    if (mode === 'agent-token' && err.code === Code.Unauthenticated) {
      return 'Castle rejected that token. Check the token and try again.';
    }
    if (mode === 'password') {
      if (err.code === Code.Unauthenticated) {
        return 'Incorrect username or password.';
      }
      if (err.code === Code.Unimplemented || err.code === Code.FailedPrecondition) {
        return 'Console login is disabled on this Castle. Ask your operator to set CASTLE_CONSOLE_USER and CASTLE_CONSOLE_PASSWORD, or sign in with an agent token instead.';
      }
    }
    const codeName = Code[err.code] ?? String(err.code);
    return `Castle is unreachable (${err.rawMessage || codeName}).`;
  }
  return 'Castle is unreachable. Check your connection and try again.';
}

// Branded login gate. The password form validates credentials against
// castle's Login RPC before the issued session token is persisted; the agent
// token form validates the candidate token against the Castle API the same
// way. An unvalidated credential is never written to storage.
export function LoginPage({ onAuthenticated, notice }: LoginPageProps) {
  const [mode, setMode] = useState<LoginMode>('password');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [token, setToken] = useState('');
  const [validating, setValidating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const credentialsReady = mode === 'password' ? username.trim() !== '' && password !== '' : token.trim() !== '';

  const switchMode = (next: LoginMode) => {
    setMode(next);
    setError(null);
  };

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    if (!credentialsReady || validating) return;
    setValidating(true);
    setError(null);
    try {
      if (mode === 'password') {
        const resp = await server.login({ username: username.trim(), password });
        onAuthenticated(resp.sessionToken);
        return;
      }
      // The probe carries the candidate token in an explicit header, so the
      // token is persisted (via onAuthenticated) only after Castle accepts
      // it — an unvalidated token is never written to storage.
      await server.listAgents({ limit: 1 }, { headers: { Authorization: `Bearer ${token.trim()}` } });
      onAuthenticated(token.trim());
    } catch (err) {
      setValidating(false);
      setError(describeError(err, mode));
    }
  };

  return (
    <div className="flex h-full items-center justify-center bg-canvas p-6 text-ink">
      <div className="w-full max-w-md rounded-lg border border-line bg-surface p-8 shadow-xl">
        <div data-testid="login-brand" className="mb-6 text-center">
          <span
            aria-hidden
            className="mx-auto mb-3 block h-6 w-6 rotate-45 rounded-sm border-2 border-accent-strong"
          />
          <h1 className="text-display font-semibold">Parapet</h1>
          <p className="mt-1 text-body text-ink-muted">Castle control plane</p>
        </div>
        {notice && (
          <p data-testid="login-notice" role="status" className="mb-4 rounded-md border border-line bg-surface-raised px-3 py-2 text-body text-ink-muted">
            {notice}
          </p>
        )}
        <form onSubmit={onSubmit} aria-busy={validating}>
          {mode === 'password' ? (
            <>
              <label htmlFor="login-username" className="mb-1 block text-body text-ink-muted">
                Username
              </label>
              <input
                id="login-username"
                data-testid="login-username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                disabled={validating}
                autoComplete="username"
                placeholder="Console username"
                className="w-full rounded-md border border-line-strong bg-canvas px-3 py-2 text-body text-ink placeholder:text-ink-faint disabled:opacity-50"
              />
              <label htmlFor="login-password" className="mb-1 mt-3 block text-body text-ink-muted">
                Password
              </label>
              <input
                id="login-password"
                data-testid="login-password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={validating}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? 'login-error' : undefined}
                autoComplete="current-password"
                placeholder="Console password"
                className="w-full rounded-md border border-line-strong bg-canvas px-3 py-2 text-body text-ink placeholder:text-ink-faint disabled:opacity-50"
              />
            </>
          ) : (
            <>
              <label htmlFor="agent-token" className="mb-1 block text-body text-ink-muted">
                Agent token
              </label>
              <input
                id="agent-token"
                data-testid="login-token"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                disabled={validating}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? 'login-error' : undefined}
                autoComplete="off"
                placeholder="Paste an agent token"
                className="w-full rounded-md border border-line-strong bg-canvas px-3 py-2 font-mono text-body text-ink placeholder:text-ink-faint disabled:opacity-50"
              />
            </>
          )}
          {error && (
            <p id="login-error" role="alert" data-testid="login-error" className="mt-2 text-body text-danger">
              {error}
            </p>
          )}
          <button
            type="submit"
            data-testid="login-submit"
            disabled={validating || !credentialsReady}
            className="mt-4 flex w-full items-center justify-center gap-2 rounded-md bg-accent px-4 py-2 text-body font-medium text-white hover:bg-accent-strong disabled:cursor-not-allowed disabled:opacity-60"
          >
            {validating && (
              <span
                aria-hidden
                className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-white/40 border-t-white"
              />
            )}
            {validating ? (mode === 'password' ? 'Signing in…' : 'Validating…') : 'Sign in'}
          </button>
        </form>
        <button
          type="button"
          data-testid="login-mode-toggle"
          onClick={() => switchMode(mode === 'password' ? 'agent-token' : 'password')}
          className="mt-4 w-full text-center text-body text-ink-muted underline-offset-2 hover:text-ink hover:underline"
        >
          {mode === 'password' ? 'Use an agent token instead' : 'Use username and password instead'}
        </button>
      </div>
    </div>
  );
}