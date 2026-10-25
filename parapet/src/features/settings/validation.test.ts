import { describe, expect, test } from 'vitest';
import {
  parseVocabulary,
  validateMemberID,
  validateProjectID,
  validateRefirePolicy,
  validateRefireRoute,
  validateRefireVocabulary,
  validateSettingKey,
  validateSettingsDoc,
  validateSettingValue,
} from './validation';

describe('validateProjectID', () => {
  test.each([
    ['alpha', null],
    ['team-1.web', null],
    ['a'.repeat(128), null],
    ['Upper', 'lowercase slug'],
    ['-lead', 'lowercase slug'],
    ['_lead', 'lowercase slug'],
    ['', 'lowercase slug'],
    ['a'.repeat(129), 'lowercase slug'],
    ['has space', 'lowercase slug'],
  ])('project id %j → %j', (id, expected) => {
    const err = validateProjectID(id);
    if (expected === null) expect(err).toBeNull();
    else expect(err).toContain(expected);
  });
});

describe('validateSettingValue', () => {
  test('empty value means entry absent and is always valid', () => {
    expect(validateSettingValue('any', '')).toBeNull();
  });

  test('requires valid JSON when non-empty', () => {
    expect(validateSettingValue('any', '{')).toContain('valid JSON');
    expect(validateSettingValue('any', '{"retries": 2}')).toBeNull();
    expect(validateSettingValue('any', '"plain string"')).toBeNull();
  });

  test('the interpreted allow_anonymous_reads key must be a JSON boolean', () => {
    expect(validateSettingValue('allow_anonymous_reads', 'true')).toBeNull();
    expect(validateSettingValue('allow_anonymous_reads', 'false')).toBeNull();
    expect(validateSettingValue('allow_anonymous_reads', '"true"')).toContain(
      'must be the JSON boolean',
    );
    expect(validateSettingValue('allow_anonymous_reads', '1')).toContain(
      'must be the JSON boolean',
    );
  });

  test('enforces the server value byte budget (UTF-8)', () => {
    const ok = JSON.stringify({ pad: 'a'.repeat(16 * 1024 - 20) });
    expect(validateSettingValue('k', ok)).toBeNull();
    const tooLong = JSON.stringify({ pad: 'a'.repeat(16 * 1024) });
    expect(validateSettingValue('k', tooLong)).toContain('bytes');
    // A multibyte payload counts its UTF-8 length, not its JS length.
    const multibyte = JSON.stringify({ pad: 'é'.repeat(16 * 1024 / 2) });
    expect(validateSettingValue('k', multibyte)).toContain('bytes');
  });
});

describe('validateSettingKey', () => {
  test('requires a key within the byte budget', () => {
    expect(validateSettingKey('')).toContain('required');
    expect(validateSettingKey('k'.repeat(256))).toBeNull();
    expect(validateSettingKey('k'.repeat(257))).toContain('256');
    expect(validateSettingKey('é'.repeat(129))).toContain('256');
  });
});

describe('validateSettingsDoc', () => {
  test('collects per-entry errors and duplicate keys', () => {
    const res = validateSettingsDoc([
      { key: '', value: '' },
      { key: 'bad json', value: '{' },
      { key: 'dup', value: '1' },
      { key: 'dup', value: '2' },
    ]);
    expect(res.keys[0]).toContain('required');
    expect(res.values[1]).toContain('valid JSON');
    expect(res.keys[3]).toBe('Duplicate key.');
    expect(res.doc).toBeNull();
  });

  test('enforces the server entry-count budget', () => {
    const many = Array.from({ length: 257 }, (_, i) => ({ key: `k${i}`, value: '1' }));
    expect(validateSettingsDoc(many).doc).toContain('256');
    const exactly = Array.from({ length: 256 }, (_, i) => ({ key: `k${i}`, value: '1' }));
    expect(validateSettingsDoc(exactly).doc).toBeNull();
  });
});

describe('validateRefirePolicy', () => {
  test('requires non-negative whole budgets', () => {
    expect(validateRefirePolicy({ maxAttempts: 0, backoffSeconds: 0 })).toBeNull();
    expect(validateRefirePolicy({ maxAttempts: 3, backoffSeconds: 120 })).toBeNull();
    expect(validateRefirePolicy({ maxAttempts: -1, backoffSeconds: 0 })).toContain(
      'non-negative whole number',
    );
    expect(validateRefirePolicy({ maxAttempts: 1.5, backoffSeconds: 0 })).toContain(
      'non-negative whole number',
    );
    expect(validateRefirePolicy({ maxAttempts: 1, backoffSeconds: -2 })).toContain(
      'non-negative whole number',
    );
  });
});

describe('validateRefireRoute', () => {
  test('route keys are opaque but UTF-8 bounded', () => {
    expect(validateRefireRoute('')).toBeNull();
    expect(validateRefireRoute('deploy/build'.padEnd(256, 'x'))).toBeNull();
    expect(validateRefireRoute('r'.repeat(257))).toContain('256');
    expect(validateRefireRoute('é'.repeat(129))).toContain('256');
  });
});

describe('validateRefireVocabulary', () => {
  test('entries are non-empty and bounded in count and length', () => {
    expect(validateRefireVocabulary('transient', ['timeout'])).toBeNull();
    expect(validateRefireVocabulary('transient', [''])).toContain('1..');
    expect(validateRefireVocabulary('transient', ['v'.repeat(16 * 1024 + 1)])).toContain('1..');
    expect(
      validateRefireVocabulary('transient', Array.from({ length: 257 }, () => 'v')),
    ).toContain('256');
  });
});

describe('parseVocabulary', () => {
  test('trims lines and drops empties so pasted lists normalize', () => {
    expect(parseVocabulary('timeout, \n  \nOOMKILLED\n\n')).toEqual(['timeout,', 'OOMKILLED']);
    expect(parseVocabulary('')).toEqual([]);
  });
});

describe('validateMemberID', () => {
  test('requires a non-empty member id', () => {
    expect(validateMemberID('ov-1')).toBeNull();
    expect(validateMemberID('   ')).toContain('required');
    expect(validateMemberID('')).toContain('required');
  });
});