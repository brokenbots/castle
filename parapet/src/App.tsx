import { useEffect, useState } from 'react';
import { useSelector } from 'react-redux';
import { clearAuthToken, getAuthToken, setAuthToken } from './authToken';
import { selectAuthExpired, sessionRecovered } from './features/auth/sessionSlice';
import { store } from './store';
import { AppShell } from './shell/AppShell';
import { LoginPage } from './shell/LoginPage';

export function App() {
  const [token, setToken] = useState(() => getAuthToken());
  const authExpired = useSelector(selectAuthExpired);

  // Mid-session auth expiry: when Castle starts rejecting the token (any
  // 401 on query or watch stream), return the user to the login gate with
  // an explanatory notice instead of leaving stuck failure states behind.
  // The expired flag stays set until the user signs in again (or logs out)
  // so the notice survives the transition to the login gate.
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
        store.dispatch(sessionRecovered());
      }}
    />
  );
}
