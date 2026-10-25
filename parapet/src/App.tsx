import { useEffect, useState } from 'react';
import { useSelector } from 'react-redux';
import { clearAuthToken, getAuthToken, setAuthToken } from './authToken';
import { castleApi, selectAuthExpired, sessionRecovered } from '@castle/run-viewer';
import { settingsApi } from './features/settings/settingsApi';
import { store } from './store';
import { AppShell } from './shell/AppShell';
import { LoginPage } from './shell/LoginPage';

// Credential-scoped caches never survive a session transition: the next
// token may resolve to a different operator with different admin rights
// and membership scope, and RTK Query would otherwise keep answering all
// consumers from the previous credential's entries (identity, scoped runs,
// agent and project pages alike). The identity endpoint additionally
// refetches on every mount, so a transition in the same store re-reads it
// even if an entry was left in the error state.
function resetSessionCaches() {
  store.dispatch(castleApi.util.resetApiState());
  store.dispatch(settingsApi.util.resetApiState());
}

export function App() {
  const [token, setToken] = useState(() => getAuthToken());
  const authExpired = useSelector(selectAuthExpired);

  // Mid-session auth expiry: when Castle starts rejecting the token (any
  // 401 on query or watch stream), return the user to the login gate with
  // an explanatory notice instead of leaving stuck failure states behind.
  // The expired flag stays set until the user signs in again (or logs out)
  // so the notice survives the transition to the login gate. The caches
  // reset when the next session signs in via onAuthenticated.
  useEffect(() => {
    if (authExpired && token) {
      clearAuthToken();
      setToken('');
    }
  }, [authExpired, token]);

  if (!token) {
    return (
      <LoginPage
        notice={authExpired ? 'Your session expired. Sign in again to continue.' : undefined}
        onAuthenticated={(next) => {
          // Drop the previous credential's caches before the new session
          // mounts anything that consumes them.
          resetSessionCaches();
          setAuthToken(next);
          setToken(next);
          store.dispatch(sessionRecovered());
        }}
      />
    );
  }

  return (
    <AppShell
      onLogout={() => {
        clearAuthToken();
        setToken('');
        resetSessionCaches();
        store.dispatch(sessionRecovered());
      }}
    />
  );
}
