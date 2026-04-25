const SUBSCRIBER_ID_KEY = 'overlord.subscriber_id';

export function subscriberIdForSession(): string {
  let id = sessionStorage.getItem(SUBSCRIBER_ID_KEY);
  if (!id) {
    id = crypto.randomUUID();
    sessionStorage.setItem(SUBSCRIBER_ID_KEY, id);
  }
  return id;
}
