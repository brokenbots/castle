import { FormEvent, useState } from 'react';
import { Code, ConnectError } from '@connectrpc/connect';
import { server } from '../api/client';
import { clearAuthToken, getAuthToken, setAuthToken } from '../authToken';

interface LoginPageProps {
  onAuthenticated: (token: string) => void;
}

function describeError(err: unknown): string {
  if (err instanceof ConnectError) {
    if (err.code === Code.Unauthenticated) {
      return 'Castle rejected that token. Check the token and try again.';
    }
    const codeName = Code[err.code] ?? String(err.code);
    return `Castle is unreachable (${err.rawMessage || codeName}).`;
  }
  return 'Castle is unreachable. Check your connection and try again.';
}

// Branded login gate. Validates the agent token against the Castle API
// before letting the user in, with an explicit loading state while the
// token is being checked and an error state when Castle rejects it.
export function LoginPage({ onAuthenticated }: LoginPageProps) {
  const [value, setValue] = useState('');
  const [validating, setValidating] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    const token = value.trim();
    if (!token || validating) return;
    setValidating(true);
    setError(null);
    // The transport interceptor authenticates every request from stored
    // state, so the candidate token is persisted for the probe and restored
    // if Castle rejects it. While the login page is mounted nothing else is
    // issuing requests, so the probe is the only consumer.
    const previous = getAuthToken();
    setAuthToken(token);
    try {
      await server.listAgents({ limit: 1 });
      onAuthenticated(token);
    } catch (err) {
      if (previous) setAuthToken(previous);
      else clearAuthToken();
      setValidating(false);
      setError(describeError(err));
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
        <form onSubmit={onSubmit} aria-busy={validating}>
          <label htmlFor="agent-token" className="mb-1 block text-body text-ink-muted">
            Agent token
          </label>
          <input
            id="agent-token"
            data-testid="login-token"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            disabled={validating}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? 'login-error' : undefined}
            autoComplete="off"
            placeholder="Paste an agent token"
            className="w-full rounded-md border border-line-strong bg-canvas px-3 py-2 font-mono text-body text-ink placeholder:text-ink-faint disabled:opacity-50"
          />
          {error && (
            <p id="login-error" role="alert" data-testid="login-error" className="mt-2 text-body text-danger">
              {error}
            </p>
          )}
          <button
            type="submit"
            data-testid="login-submit"
            disabled={validating || value.trim() === ''}
            className="mt-4 flex w-full items-center justify-center gap-2 rounded-md bg-accent px-4 py-2 text-body font-medium text-white hover:bg-accent-strong disabled:cursor-not-allowed disabled:opacity-60"
          >
            {validating && (
              <span
                aria-hidden
                className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-white/40 border-t-white"
              />
            )}
            {validating ? 'Validating…' : 'Sign in'}
          </button>
        </form>
      </div>
    </div>
  );
}