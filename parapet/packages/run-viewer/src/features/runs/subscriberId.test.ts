import { beforeEach, describe, expect, test, vi } from 'vitest';
import { subscriberIdForSession } from './subscriberId';

describe('subscriberIdForSession', () => {
  beforeEach(() => {
    sessionStorage.clear();
  });

  test('returns same id for repeated calls in one session', () => {
    const randomUUID = vi
      .spyOn(crypto, 'randomUUID')
      .mockReturnValue('11111111-1111-4111-8111-111111111111');

    const first = subscriberIdForSession();
    const second = subscriberIdForSession();

    expect(first).toBe('11111111-1111-4111-8111-111111111111');
    expect(second).toBe('11111111-1111-4111-8111-111111111111');
    expect(randomUUID).toHaveBeenCalledTimes(1);

    randomUUID.mockRestore();
  });

  test('returns different ids in a fresh session', () => {
    const randomUUID = vi
      .spyOn(crypto, 'randomUUID')
      .mockReturnValueOnce('22222222-2222-4222-8222-222222222222')
      .mockReturnValueOnce('33333333-3333-4333-8333-333333333333');

    const first = subscriberIdForSession();
    sessionStorage.clear();
    const second = subscriberIdForSession();

    expect(first).toBe('22222222-2222-4222-8222-222222222222');
    expect(second).toBe('33333333-3333-4333-8333-333333333333');

    randomUUID.mockRestore();
  });
});
