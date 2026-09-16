import { describe, expect, it } from 'vitest';
import {
  durationBetweenMs,
  formatAbsoluteTime,
  formatDuration,
  formatRelativeTime,
} from './time';

describe('formatDuration', () => {
  it.each([
    [0, '0s'],
    [-5_000, '0s'],
    [Number.NaN, '0s'],
    [42_000, '42s'],
    [59_999, '59s'],
    [61_000, '1m 01s'],
    [303_000, '5m 03s'],
    [3_600_000, '1h 00m'],
    [25 * 3_600_000, '1d 01h'],
  ])('renders %i ms as %s', (ms, want) => {
    expect(formatDuration(ms)).toBe(want);
  });
});

describe('formatRelativeTime', () => {
  const now = Date.parse('2025-01-01T12:00:00Z');

  it('shows "just now" for very recent timestamps', () => {
    expect(formatRelativeTime('2025-01-01T11:59:44Z', now)).toBe('just now');
    expect(formatRelativeTime('2025-01-01T12:00:00Z', now)).toBe('just now');
  });

  it('renders the whole sub-minute window as "just now", never "0 minutes ago"', () => {
    // 45-59s previously skipped the "just now" branch and floored to
    // "0 minutes ago".
    expect(formatRelativeTime('2025-01-01T11:59:15Z', now)).toBe('just now');
    expect(formatRelativeTime('2025-01-01T11:59:01Z', now)).toBe('just now');
  });

  it('renders singular and plural minutes', () => {
    expect(formatRelativeTime('2025-01-01T11:55:00Z', now)).toBe('5 minutes ago');
    expect(formatRelativeTime('2025-01-01T11:59:00Z', now)).toBe('1 minute ago');
  });

  it('renders singular and plural hours', () => {
    expect(formatRelativeTime('2025-01-01T09:00:00Z', now)).toBe('3 hours ago');
    expect(formatRelativeTime('2025-01-01T11:00:00Z', now)).toBe('1 hour ago');
  });

  it('renders singular and plural days', () => {
    expect(formatRelativeTime('2024-12-28T12:00:00Z', now)).toBe('4 days ago');
    expect(formatRelativeTime('2024-12-31T12:00:00Z', now)).toBe('1 day ago');
  });

  it('clamps clock skew to zero instead of going negative', () => {
    expect(formatRelativeTime('2025-01-01T12:00:10Z', now)).toBe('just now');
  });

  it('returns an empty string for invalid timestamps', () => {
    expect(formatRelativeTime('not-a-date', now)).toBe('');
  });
});

describe('formatAbsoluteTime', () => {
  it('renders the timestamp with the local locale', () => {
    const rendered = formatAbsoluteTime('2026-02-05T08:30:00Z');
    expect(rendered).not.toBe('');
    expect(rendered).toMatch(/2026/);
  });

  it('returns an empty string for invalid timestamps', () => {
    expect(formatAbsoluteTime('oops')).toBe('');
  });
});

describe('durationBetweenMs', () => {
  it('returns the static duration between startedAt and endedAt', () => {
    expect(durationBetweenMs('2025-01-01T12:00:00Z', '2025-01-01T12:02:30Z')).toBe(150_000);
  });

  it('uses now for a live elapsed time when endedAt is missing', () => {
    const now = Date.parse('2025-01-01T12:00:10Z');
    expect(durationBetweenMs('2025-01-01T12:00:00Z', undefined, now)).toBe(10_000);
  });

  it('clamps negative durations to zero', () => {
    expect(durationBetweenMs('2025-01-01T12:00:10Z', '2025-01-01T12:00:00Z')).toBe(0);
  });

  it('returns undefined when startedAt is missing or invalid', () => {
    expect(durationBetweenMs(undefined, '2025-01-01T12:00:00Z')).toBeUndefined();
    expect(durationBetweenMs('nope', '2025-01-01T12:00:00Z')).toBeUndefined();
  });
});