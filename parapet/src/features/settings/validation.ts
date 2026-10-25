// Client-side shape validation for the settings console (KB-327). The rules
// mirror castle's server-side validation exactly (castle/internal/store
// project.go, the rpc convention limits, refire budgets and vocabularies) so
// a form that passes here cannot be bounced by the server for shape reasons:
// non-empty setting values must parse as JSON, the ONE interpreted key
// (allow_anonymous_reads) must be a JSON boolean, and refire entries are
// bounded to the same per-value byte budget the server enforces.
export const SETTING_KEY_MAX_BYTES = 256;
export const SETTING_VALUE_MAX_BYTES = 16 * 1024;
export const SETTING_MAX_ENTRIES = 256;
export const ROUTE_MAX_BYTES = 256;
export const VOCAB_VALUE_MAX_BYTES = 16 * 1024;
export const VOCAB_MAX_ENTRIES = 256;

// allow_anonymous_reads (KB-326) is the ONE interpreted settings key: its
// value must be the JSON boolean true or false.
export const ALLOW_ANON_READS_KEY = 'allow_anonymous_reads';

// Castle constrains project ids to lowercase slugs (store.projectIDPattern).
export const PROJECT_ID_PATTERN = /^[a-z0-9][a-z0-9-_.]{0,127}$/;

const encoder = new TextEncoder();

export function utf8ByteLength(s: string): number {
  return encoder.encode(s).length;
}

/** Project slug shape: null when valid, an explanatory message otherwise. */
export function validateProjectID(id: string): string | null {
  if (!PROJECT_ID_PATTERN.test(id)) {
    return 'Use a lowercase slug (a-z, 0-9, -, _, .) starting with a letter or digit, up to 128 characters.';
  }
  return null;
}

/** Empty entries mean "entry absent" on the wire; a value must be valid JSON. */
export function validateSettingValue(key: string, value: string): string | null {
  if (utf8ByteLength(value) > SETTING_VALUE_MAX_BYTES) {
    return `Value exceeds ${SETTING_VALUE_MAX_BYTES} bytes.`;
  }
  if (value === '') return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    return 'Value must be valid JSON.';
  }
  if (key === ALLOW_ANON_READS_KEY && typeof parsed !== 'boolean') {
    return `${ALLOW_ANON_READS_KEY} must be the JSON boolean true or false.`;
  }
  return null;
}

/** Setting key shape: null when valid, an explanatory message otherwise. */
export function validateSettingKey(key: string): string | null {
  if (key === '') return 'Key is required.';
  if (utf8ByteLength(key) > SETTING_KEY_MAX_BYTES) {
    return `Key length must be 1..${SETTING_KEY_MAX_BYTES} bytes.`;
  }
  return null;
}

/** Whole-doc shape: entry count and duplicate keys, on top of entry rules. */
export function validateSettingsDoc(
  entries: { key: string; value: string }[],
): { keys: Record<number, string>; values: Record<number, string>; doc: string | null } {
  const keys: Record<number, string> = {};
  const values: Record<number, string> = {};
  const seen = new Set<string>();
  let doc: string | null = null;
  if (entries.length > SETTING_MAX_ENTRIES) {
    doc = `Settings doc exceeds ${SETTING_MAX_ENTRIES} entries.`;
  }
  entries.forEach((entry, i) => {
    const keyError = validateSettingKey(entry.key);
    if (keyError) keys[i] = keyError;
    const valueError = validateSettingValue(entry.key, entry.value);
    if (valueError) values[i] = valueError;
    if (seen.has(entry.key)) keys[i] = 'Duplicate key.';
    if (entry.key !== '') seen.add(entry.key);
  });
  return { keys, values, doc };
}

/** Refire policy shape (mirrors refire.ParsePolicy + the RPC shape rules). */
export function validateRefirePolicy(
  policy: { maxAttempts: number; backoffSeconds: number },
): string | null {
  if (!Number.isInteger(policy.maxAttempts) || policy.maxAttempts < 0) {
    return 'Max attempts must be a non-negative whole number (0 disables refiring).';
  }
  if (!Number.isInteger(policy.backoffSeconds) || policy.backoffSeconds < 0) {
    return 'Backoff seconds must be a non-negative whole number.';
  }
  return null;
}

/** Route key shape: opaque, but bounded like the server validates. */
export function validateRefireRoute(route: string): string | null {
  if (utf8ByteLength(route) > ROUTE_MAX_BYTES) {
    return `Route length must be at most ${ROUTE_MAX_BYTES} bytes.`;
  }
  return null;
}

/** One classification vocabulary entry: non-empty, bounded. */
export function validateRefireVocabulary(name: string, values: string[]): string | null {
  if (values.length > VOCAB_MAX_ENTRIES) {
    return `${name} exceeds ${VOCAB_MAX_ENTRIES} entries.`;
  }
  for (const v of values) {
    if (v.length === 0 || utf8ByteLength(v) > VOCAB_VALUE_MAX_BYTES) {
      return `${name} value length must be 1..${VOCAB_VALUE_MAX_BYTES} bytes.`;
    }
  }
  return null;
}

/** Split a textarea's lines into vocabulary entries (trimmed, empties dropped). */
export function parseVocabulary(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

/** Member id shape: non-empty like the server validates. */
export function validateMemberID(id: string): string | null {
  if (id.trim() === '') return 'Member id is required.';
  return null;
}
