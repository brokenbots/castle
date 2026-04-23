const TOKEN_KEY = 'parapetToken';

const fallback = {
  value: '',
};

function storageAvailable(): Storage | null {
  const ls = window.localStorage as Storage | undefined;
  if (!ls) return null;
  if (typeof ls.getItem !== 'function' || typeof ls.setItem !== 'function' || typeof ls.removeItem !== 'function') {
    return null;
  }
  return ls;
}

export function getAuthToken(): string {
  const ls = storageAvailable();
  return ls ? ls.getItem(TOKEN_KEY) ?? '' : fallback.value;
}

export function setAuthToken(token: string): void {
  const ls = storageAvailable();
  if (ls) {
    ls.setItem(TOKEN_KEY, token);
    return;
  }
  fallback.value = token;
}

export function clearAuthToken(): void {
  const ls = storageAvailable();
  if (ls) {
    ls.removeItem(TOKEN_KEY);
    return;
  }
  fallback.value = '';
}
