import { Code, ConnectError } from '@connectrpc/connect';

// Error classification shared by the page-state UI. The data layer surfaces
// errors in two shapes: RTK Query endpoints (fakeBaseQuery) return
// `{ status, data }` where `status` is the canonical connect code name
// (castleApi.toError) or an HTTP status number, and the watch stream surfaces
// ConnectError instances directly. Both are reduced to one of a few UI kinds
// so pages can render auth, not-found and server errors differently instead
// of a single dead-end failure text.
export type PageErrorKind =
  | 'unauthenticated'
  | 'not_found'
  | 'forbidden'
  | 'server'
  | 'unknown';

// Canonical lower_snake connect code name (e.g. Code.Unauthenticated →
// "unauthenticated") so the UI can render readable inline errors.
export function connectCodeName(code: Code): string {
  const name = Code[code];
  if (!name) return String(code);
  return (
    name.charAt(0).toLowerCase() +
    name.slice(1).replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`)
  );
}

// Connect code names (lower_snake) that mean "Castle answered but failed".
// Everything not in the specific buckets below and not one of these lands in
// `unknown` (e.g. CUSTOM_ERROR from a thrown non-Connect error).
const SERVER_CODE_NAMES = new Set([
  'unavailable',
  'internal',
  'data_loss',
  'unimplemented',
  'deadline_exceeded',
  'aborted',
  'canceled',
  'resource_exhausted',
  'unknown',
]);

function classifyStatus(status: string | number): PageErrorKind {
  if (status === 'unauthenticated' || status === 401) return 'unauthenticated';
  if (status === 'not_found' || status === 404) return 'not_found';
  if (status === 'permission_denied' || status === 'forbidden' || status === 403) {
    return 'forbidden';
  }
  if (SERVER_CODE_NAMES.has(String(status)) || (typeof status === 'number' && status >= 500)) {
    return 'server';
  }
  return 'unknown';
}

// True when Castle rejected the caller's credentials (HTTP 401 / connect
// `unauthenticated`). The shell reacts to this by returning the user to the
// login gate rather than rendering a dead-end error.
export function isUnauthenticatedError(err: unknown): boolean {
  return classifyError(err) === 'unauthenticated';
}

export function classifyError(err: unknown): PageErrorKind {
  if (err == null) return 'unknown';
  // ConnectError (the watch stream surfaces these directly).
  if (err instanceof ConnectError) {
    return classifyStatus(connectCodeName(err.code));
  }
  // RTK Query error shape ({ status, data }) — a connect code name or an
  // HTTP status number.
  if (typeof err === 'object') {
    const { status } = err as { status?: unknown };
    if (typeof status === 'string' || typeof status === 'number') {
      return classifyStatus(status);
    }
  }
  return 'unknown';
}