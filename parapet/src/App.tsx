import { useState } from 'react';
import { clearAuthToken, getAuthToken, setAuthToken } from './authToken';
import { AppShell } from './shell/AppShell';
import { LoginPage } from './shell/LoginPage';

export function App() {
  const [token, setToken] = useState(() => getAuthToken());

  if (!token) {
    return (
      <LoginPage
        onAuthenticated={(next) => {
          setAuthToken(next);
          setToken(next);
        }}
      />
    );
  }

  return (
    <AppShell
      onLogout={() => {
        clearAuthToken();
        setToken('');
      }}
    />
  );
}
