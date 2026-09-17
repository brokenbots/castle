import { render } from '@testing-library/react';
import { afterEach, describe, expect, test } from 'vitest';
import { useDocumentTitle } from './useDocumentTitle';

const BASE_TITLE = 'Parapet — Castle';

function Probe({ title }: { title?: string }) {
  useDocumentTitle(title);
  return null;
}

afterEach(() => {
  document.title = BASE_TITLE;
});

describe('useDocumentTitle', () => {
  test('sets the tab title from the argument while mounted', () => {
    render(<Probe title="hello" />);

    expect(document.title).toBe('hello — Parapet — Castle');
  });

  test('falls back to the base title when the argument is undefined', () => {
    render(<Probe />);

    expect(document.title).toBe(BASE_TITLE);
  });

  test('follows argument changes while mounted', () => {
    const { rerender } = render(<Probe title="hello" />);

    rerender(<Probe title="local" />);

    expect(document.title).toBe('local — Parapet — Castle');
  });

  test('restores the base title on unmount so navigations never leak a stale title', () => {
    const { unmount } = render(<Probe title="hello" />);

    expect(document.title).toBe('hello — Parapet — Castle');
    unmount();

    expect(document.title).toBe(BASE_TITLE);
  });
});