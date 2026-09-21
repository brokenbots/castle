import { useEffect } from 'react';

// Base tab title, matching index.html.
const BASE_TITLE = 'Parapet — Castle';

// Sets the browser tab title while the caller is mounted and restores the
// base title when it unmounts, so deep pages (run detail, agent detail) can
// brand the tab with their subject without leaking stale titles across
// navigations. An undefined title resets to the base.
export function useDocumentTitle(title?: string) {
  useEffect(() => {
    document.title = title ? `${title} — Parapet — Castle` : BASE_TITLE;
    return () => {
      document.title = BASE_TITLE;
    };
  }, [title]);
}