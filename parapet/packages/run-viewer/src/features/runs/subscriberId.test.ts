import { beforeEach, describe, expect, test, vi } from 'vitest';
import {
  SUBSCRIBER_ID_KEY,
  formatUuidV4,
  generateSubscriberId,
  subscriberIdForSession,
} from './subscriberId';

// CRI-284 regression: crypto.randomUUID only exists in secure contexts, and
// the castle ingress is plain HTTP on an internal IP, so a real browser at
// /runs/:id used to throw "crypto.randomUUID is not a function" and take the
// whole page down. The subscriber id is now generated from
// crypto.getRandomValues (available in insecure contexts too), so the
// insecure origin is simulated by making randomUUID look absent.
function hideRandomUUID() {
  Object.defineProperty(crypto, 'randomUUID', { value: undefined, configurable: true });
}

function restoreRandomUUID() {
  delete (crypto as { randomUUID?: unknown }).randomUUID;
}

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

describe('generateSubscriberId', () => {
  test('returns well-formed UUID v4 values', () => {
    const first = generateSubscriberId();
    const second = generateSubscriberId();

    expect(first).toMatch(UUID_V4);
    expect(second).toMatch(UUID_V4);
    expect(first).not.toBe(second);
  });

  test('does not call crypto.randomUUID (removed dependency)', () => {
    const randomUUID = vi.spyOn(crypto, 'randomUUID');

    generateSubscriberId();

    expect(randomUUID).not.toHaveBeenCalled();
  });

  test('still produces a UUID v4 when crypto.randomUUID is absent', () => {
    hideRandomUUID();
    try {
      expect(generateSubscriberId()).toMatch(UUID_V4);
    } finally {
      restoreRandomUUID();
    }
  });

  test('never throws even without Web Crypto at all', () => {
    const originalCrypto = globalThis.crypto;
    Object.defineProperty(globalThis, 'crypto', { value: undefined, configurable: true });
    try {
      expect(generateSubscriberId()).toMatch(UUID_V4);
    } finally {
      Object.defineProperty(globalThis, 'crypto', { value: originalCrypto, configurable: true });
    }
  });
});

describe('formatUuidV4', () => {
  test('stamps version 4 and the RFC 4122 variant over the raw bytes', () => {
    const bytes = Uint8Array.from({ length: 16 }, (_, i) => (i * 17 + 3) & 0xff);

    const id = formatUuidV4(bytes);

    expect(id).toMatch(UUID_V4);
    // 0x69 & 0x0f | 0x40 = 0x49 (version nibble), 0x8b & 0x3f | 0x80 = 0x8b.
    expect(id).toBe('03142536-4758-497a-8b9c-adbecfe0f102');
  });
});

describe('subscriberIdForSession', () => {
  beforeEach(() => {
    sessionStorage.clear();
  });

  test('returns same id for repeated calls in one session', () => {
    const first = subscriberIdForSession();
    const second = subscriberIdForSession();

    expect(first).toMatch(UUID_V4);
    expect(second).toBe(first);
  });

  test('persists the id in sessionStorage', () => {
    const id = subscriberIdForSession();

    expect(sessionStorage.getItem(SUBSCRIBER_ID_KEY)).toBe(id);
  });

  test('returns different ids in a fresh session', () => {
    const first = subscriberIdForSession();
    sessionStorage.clear();
    const second = subscriberIdForSession();

    expect(second).toMatch(UUID_V4);
    expect(first).not.toBe(second);
  });

  test('works when crypto.randomUUID is absent (insecure origin)', () => {
    hideRandomUUID();
    try {
      const id = subscriberIdForSession();

      expect(id).toMatch(UUID_V4);
      expect(sessionStorage.getItem(SUBSCRIBER_ID_KEY)).toBe(id);
    } finally {
      restoreRandomUUID();
    }
  });
});
