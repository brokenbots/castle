import type { EventEnvelope } from '../../../api/castleApi';

/** Text body of a stepLog chunk (also used for block coalescing). */
export function renderPayloadText(e: EventEnvelope): string {
  const p = e.payload as Record<string, unknown> | undefined;
  return String((p as { chunk?: string } | undefined)?.chunk ?? '');
}

/** Shape of an adapterEvent envelope payload (`criteria.v1.AdapterEvent`). */
interface AdapterEventPayload {
  step?: string;
  adapter?: string;
  kind?: string;
  data?: Record<string, unknown>;
}

// The delta-family markers the coalescer accepts: an explicit delta string
// inside the data record, or a kind that names itself a delta stream.
function isDeltaData(data: Record<string, unknown> | undefined, kind: string): boolean {
  if (typeof (data as { delta?: unknown } | undefined)?.delta === 'string') return true;
  return /delta$/i.test(kind);
}

/** Whether the envelope opens/extends a coalescable adapterEvent stream. */
export function adapterStreamKey(e: EventEnvelope): string {
  if (e.type !== 'adapterEvent') return '';
  const p = (e.payload ?? {}) as AdapterEventPayload;
  const adapter = p.adapter ?? '';
  const kind = p.kind ?? '';
  if (!adapter || !kind) return '';
  return isDeltaData(p.data, kind) ? `adapter:${adapter}:${kind}` : '';
}

/**
 * The streamed text of a streamable event: a stepLog chunk or an adapter
 * delta fragment. Blocks join these in emission order, so a delta stream
 * reads back as the complete message text.
 */
export function streamChunkText(e: EventEnvelope): string {
  if (e.type === 'stepLog') return renderPayloadText(e);
  const p = (e.payload ?? {}) as AdapterEventPayload;
  const raw = (p.data as { delta?: unknown } | undefined)?.delta;
  return typeof raw === 'string' ? raw : '';
}

/** Text of a completed agent message record, first matching key wins. */
function agentMessageText(data: Record<string, unknown> | undefined): string {
  for (const key of ['text', 'content', 'message', 'body']) {
    const v = (data as Record<string, unknown> | undefined)?.[key];
    if (typeof v === 'string' && v !== '' && (data as Record<string, unknown>).delta === undefined) return v;
  }
  return '';
}

/** Tool invocation summary fields with the common wire-key fallbacks. */
function toolInvocationView(data: Record<string, unknown> | undefined): { tool: string; result: string } {
  const tool =
    ['tool', 'name', 'tool_name', 'command'].map((k) => (data as Record<string, unknown> | undefined)?.[k]).find(
      (v): v is string => typeof v === 'string' && v !== '',
    ) ?? '';
  const result =
    ['result', 'output', 'status', 'response'].map((k) => (data as Record<string, unknown> | undefined)?.[k]).find(
      (v) => v !== undefined && v !== null && v !== '',
    );
  const resultText = result === undefined ? '' : typeof result === 'string' ? result : JSON.stringify(result);
  return { tool, result: resultText };
}

export type EventSummaryTone =
  | 'mono'
  | 'success'
  | 'danger'
  | 'warning'
  | 'muted'
  | 'plain';

export interface EventSummaryPart {
  /** Faint label prefix (the payload key in human form). */
  label: string;
  text: string;
  tone?: EventSummaryTone;
}

// Bounds for the collapsed single-line summary: a handful of top-level
// pairs, values head-truncated. The full record stays reachable by
// expanding the row.
const MAX_SUMMARY_PARTS = 6;
const VALUE_HEAD_CHARS = 72;

function scalarText(value: unknown): string {
  if (value === null || value === undefined || value === '') return '(empty)';
  if (typeof value === 'boolean') return value ? 'true' : 'false';
  if (typeof value === 'string') {
    return value.length > VALUE_HEAD_CHARS ? `${value.slice(0, VALUE_HEAD_CHARS)}…` : value;
  }
  if (typeof value === 'number') return String(value);
  const json = JSON.stringify(value);
  if (json === undefined) return '(complex)';
  return json.length > VALUE_HEAD_CHARS ? `${json.slice(0, VALUE_HEAD_CHARS)}…` : json;
}

/** Compact head of a raw value for inline summaries (strings stay raw). */
function scalarHead(value: unknown): string {
  const text = typeof value === 'string' ? value : JSON.stringify(value) ?? '';
  return text.length > VALUE_HEAD_CHARS ? `${text.slice(0, VALUE_HEAD_CHARS)}…` : text || '(empty)';
}

// Value tone by key/outcome shape: the branch decision vocabulary
// (matched arm → warning, target → success) plus the run-status colors.
export function toneForKey(key: string, value: unknown): EventSummaryTone {
  const k = key.toLowerCase();
  if (k === 'outcome' || k === 'status' || k === 'finalstate' || k === 'ok') {
    if (value === 'success' || value === true) return 'success';
    if (value === 'failure' || value === false) return 'danger';
    return 'muted';
  }
  if (k === 'target' || k === 'next') return 'success';
  if (k === 'matchedarm' || k === 'arm') return 'warning';
  if (k === 'error' || k === 'failure' || k === 'crash') return 'danger';
  if (k === 'message' && typeof value === 'string' && /fail|error|denied/i.test(value)) {
    return 'danger';
  }
  if (k === 'step' || k === 'node' || k === 'name') return 'mono';
  return 'plain';
}

/** `matchedArm` → `matched arm`; acronyms stay readable (`KB-224` etc. are values, not keys). */
export function fieldLabel(key: string): string {
  return key
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/_/g, ' ')
    .toLowerCase();
}

/**
 * Collapsed summary of ANY event. Adapter events render by their kind:
 *
 * - `agent.message` → the completed message text (`text`/`content`/…),
 *   with the emitting adapter as a leading mono part.
 * - `tool.invocation` → the tool name plus its result/status value.
 * - delta streams do not render here: the coalescer folds them into one
 *   stepLog-style block whose body is the joined delta text.
 * - anything else → the generic top-level key-value list (kind data rides
 *   as its compact JSON head).
 *
 * All other event types render the payload's top-level key-value pairs, in
 * wire order. Empty records collapse to a dash. Scalars, and head-truncated
 * compact JSON for nested values, keep the line single-height; the expanded
 * view carries the full record.
 */
export function eventSummaryParts(e: EventEnvelope): EventSummaryPart[] {
  const p = e.payload as Record<string, unknown> | undefined;
  if (e.type === 'stepLog') {
    const chunk = renderPayloadText(e);
    const head = chunk.split('\n')[0] ?? '';
    return [
      {
        label: 'output',
        text: head.length > VALUE_HEAD_CHARS ? `${head.slice(0, VALUE_HEAD_CHARS)}…` : head || '(empty output)',
        tone: 'muted',
      },
    ];
  }
  if (e.type === 'adapterEvent') {
    const { adapter, kind, data } = (p ?? {}) as AdapterEventPayload;
    if (isDeltaData(data, kind ?? '')) {
      // Coalescer-consumed: block text comes from streamChunkText.
      return [{ label: 'output', text: streamChunkText(e) || '(streaming)', tone: 'muted' }];
    }
    const parts: EventSummaryPart[] = [];
    if (adapter) parts.push({ label: 'adapter', text: adapter, tone: 'mono' });
    if (kind?.startsWith('agent.message')) {
      const text = agentMessageText(data);
      return [
        ...parts,
        {
          label: 'message',
          text: text.length > VALUE_HEAD_CHARS ? `${text.slice(0, VALUE_HEAD_CHARS)}…` : text || '(empty message)',
          tone: 'plain',
        },
      ];
    }
    if (kind?.startsWith('tool.invocation')) {
      const { tool, result } = toolInvocationView(data);
      if (tool) parts.push({ label: 'tool', text: tool, tone: 'mono' });
      if (result) parts.push({ label: 'result', text: scalarHead(result), tone: 'muted' });
      if (parts.length > (parts[0]?.label === 'adapter' ? 1 : 0)) return parts;
    }
    if (!adapter && !kind) return [{ label: 'payload', text: '(empty)', tone: 'muted' }];
    if (adapter && kind) {
      if (parts.length === 1) parts.push({ label: 'kind', text: kind, tone: 'muted' });
      return parts;
    }
    return parts.length > 0 ? parts : [{ label: 'payload', text: '(empty)', tone: 'muted' }];
  }
  if (!p || typeof p !== 'object' || Array.isArray(p)) {
    const s = p === undefined ? '' : JSON.stringify(p) ?? '';
    return [{ label: 'payload', text: s.slice(0, VALUE_HEAD_CHARS) || '—', tone: 'muted' }];
  }
  const entries = Object.entries(p);
  if (entries.length === 0) return [{ label: 'payload', text: '(empty)', tone: 'muted' }];
  return entries.slice(0, MAX_SUMMARY_PARTS).map(([key, value]) => ({
    label: fieldLabel(key),
    text: scalarText(value),
    tone: toneForKey(key, value),
  }));
}

/** Pretty multi-line form of an expanded value: JSON for records, text for scalars. */
export function expandedValueText(value: unknown): string {
  if (value === null || value === undefined) return '—';
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean') return String(value);
  return JSON.stringify(value, null, 2) ?? '—';
}