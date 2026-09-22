export const SUBSCRIBER_ID_KEY = 'criteria.subscriber_id';

// crypto.randomUUID requires a secure context, but the castle ingress is
// plain HTTP on an internal IP, so a real browser leaves it undefined
// (CRI-284: "crypto.randomUUID is not a function" killed /runs/:id). Build
// the id from crypto.getRandomValues instead, which browsers provide in
// insecure contexts too.
export function generateSubscriberId(): string {
  const webCrypto = globalThis.crypto as Crypto | undefined;
  if (typeof webCrypto?.getRandomValues === 'function') {
    return formatUuidV4(webCrypto.getRandomValues(new Uint8Array(16)));
  }
  // No Web Crypto at all: degrade to Math.random so the id stays well-formed
  // instead of throwing. It is an opaque per-session label, not a secret.
  return formatUuidV4(Uint8Array.from({ length: 16 }, () => Math.floor(Math.random() * 256)));
}

export function formatUuidV4(bytes: Uint8Array): string {
  bytes[6] = (bytes[6] & 0x0f) | 0x40; // version 4
  bytes[8] = (bytes[8] & 0x3f) | 0x80; // RFC 4122 variant
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function subscriberIdForSession(): string {
  let id = sessionStorage.getItem(SUBSCRIBER_ID_KEY);
  if (!id) {
    id = generateSubscriberId();
    sessionStorage.setItem(SUBSCRIBER_ID_KEY, id);
  }
  return id;
}
