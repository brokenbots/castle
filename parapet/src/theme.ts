export type ParapetTheme = 'dark' | 'light';

const STORAGE_KEY = 'parapet-theme';

/** Reads the persisted theme; falls back to the OS preference. */
export function readStoredTheme(): ParapetTheme {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored === 'light' || stored === 'dark') return stored;
  } catch {
    // Storage can throw in privacy modes; the OS preference stays the fallback.
  }
  return window.matchMedia?.('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

/**
 * Applies the theme to the document root and persists it. Idempotent; safe
 * to call on every boot and every toggle.
 */
export function applyTheme(theme: ParapetTheme): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // Non-fatal: the attribute still drives the in-session theme.
  }
}

/** Boot hook: resolves the effective theme and applies it once. */
export function initTheme(): ParapetTheme {
  const theme = readStoredTheme();
  document.documentElement.dataset.theme = theme;
  return theme;
}