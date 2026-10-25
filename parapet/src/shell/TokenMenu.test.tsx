import { cleanup, render, screen, within } from '@testing-library/react';
import { Provider } from 'react-redux';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, test } from 'vitest';
import userEvent from '@testing-library/user-event';
import { TokenMenu } from './TokenMenu';
import { setAuthToken, clearAuthToken } from '../authToken';
import { createRunViewerStore } from '@castle/run-viewer';
import { server } from '@castle/run-viewer/src/test/mocks/server';
import { identityPath } from '@castle/run-viewer/src/test/mocks/handlers';

// KB-333: the session menu renders the operator's own attributes — display
// name, role, scope — instead of leading with the credential class, and
// falls back to the token-only presentation when no identity is available.

function renderTokenMenu() {
  setAuthToken('test-token-12345678');
  const store = createRunViewerStore();
  render(
    <Provider store={store}>
      <TokenMenu onLogout={() => {}} />
    </Provider>,
  );
}

afterEach(() => {
  cleanup();
  clearAuthToken();
});

describe('TokenMenu identity badge (KB-333)', () => {
  test('a console admin renders display name, role chip and unscoped badge', async () => {
    renderTokenMenu();

    const badge = await screen.findByTestId('token-menu-identity');
    expect(badge).toHaveTextContent('operator');
    const button = badge.closest('button')!;
    expect(within(button).getByText('admin')).toBeInTheDocument();
  });

  test('the panel carries the scope summary and the masked token stays secondary', async () => {
    const user = userEvent.setup();
    renderTokenMenu();

    await screen.findByTestId('token-menu-identity');
    await user.click(screen.getByTestId('token-menu-button'));

    const panel = screen.getByTestId('token-menu-panel');
    expect(within(panel).getByTestId('token-identity-name')).toHaveTextContent('operator');
    expect(within(panel).getByTestId('token-identity-role')).toHaveTextContent('admin');
    expect(within(panel).getByTestId('token-identity-scope')).toHaveTextContent('all projects');
    // The masked token is still visible as a secondary session detail, and
    // only its trailing four characters are ever surfaced.
    const tokenLine = within(panel).getByTestId('token-menu-token-identity');
    expect(tokenLine).toHaveTextContent(/••••\d{4}$/);
    expect(tokenLine).not.toHaveTextContent('test-token-12345678');
    // Theme and logout stay reachable.
    expect(within(panel).getByTestId('theme-toggle')).toBeInTheDocument();
    expect(within(panel).getByTestId('logout')).toBeInTheDocument();
  });

  test('a membership-scoped agent identity names its project scope', async () => {
    server.use(
      http.post(identityPath('GetCallerIdentity'), () =>
        HttpResponse.json({
          operator_id: 'agent-1',
          name: 'agent-1',
          display_name: 'agent-1',
          role: 'agent',
          attributes: { role: 'agent' },
          membership_scoped: true,
          membership: ['squad'],
        }),
      ),
    );

    renderTokenMenu();

    await screen.findByTestId('token-menu-identity');
    await userEvent.setup().click(screen.getByTestId('token-menu-button'));

    const panel = screen.getByTestId('token-menu-panel');
    expect(within(panel).getByTestId('token-identity-role')).toHaveTextContent('agent');
    expect(within(panel).getByTestId('token-identity-scope')).toHaveTextContent('scoped to squad');
  });

  test('a scoped agent with no membership rows reads as out-of-scope', async () => {
    server.use(
      http.post(identityPath('GetCallerIdentity'), () =>
        HttpResponse.json({
          operator_id: 'agent-2',
          name: 'agent-2',
          display_name: 'agent-2',
          role: 'agent',
          attributes: { role: 'agent' },
          membership_scoped: true,
          membership: [],
        }),
      ),
    );

    renderTokenMenu();

    await screen.findByTestId('token-menu-identity');
    await userEvent.setup().click(screen.getByTestId('token-menu-button'));

    expect(within(screen.getByTestId('token-menu-panel')).getByTestId('token-identity-scope')).toHaveTextContent(
      'no projects in scope',
    );
  });

  test('falls back to the token-only presentation when identity is unavailable', async () => {
    setAuthToken('abc123456789');
    server.use(
      http.post(identityPath('GetCallerIdentity'), () =>
        new HttpResponse(JSON.stringify({ code: 'unavailable', message: 'offline' }), {
          status: 503,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );
    const store = createRunViewerStore();
    render(
      <Provider store={store}>
        <TokenMenu onLogout={() => {}} />
      </Provider>,
    );

    await screen.findByTestId('token-menu-button');
    // Let the identity read settle (the 503 above) so the fallback
    // assertions cover the post-resolution state and not a pending race.
    await new Promise((resolve) => setTimeout(resolve, 500));
    // No identity: the badge keeps the credential-class presentation —
    // masked token only — and nothing identity-flavored renders.
    expect(screen.queryByTestId('token-menu-identity')).not.toBeInTheDocument();
    expect(screen.getByTestId('token-menu-button')).toHaveTextContent('••••6789');

    await userEvent.setup().click(screen.getByTestId('token-menu-button'));
    const panel = screen.getByTestId('token-menu-panel');
    expect(within(panel).getByTestId('token-menu-token')).toHaveTextContent('••••6789');
    expect(within(panel).queryByTestId('token-identity-name')).not.toBeInTheDocument();
  });
});