import type { EventEnvelope } from '../../../api/castleApi';

/** Text body of an event row: the stepLog chunk or the JSON payload. */
export function renderPayloadText(e: EventEnvelope): string {
  const p = e.payload as Record<string, unknown> | undefined;
  if (e.type === 'stepLog') return String((p as { chunk?: string } | undefined)?.chunk ?? '');
  return JSON.stringify(p ?? null);
}