import { useMemo, useState, type ReactNode } from 'react';
import { useInspectRunQuery } from '../../api/castleApi';
import { formatAbsoluteTime, formatRelativeTime } from './time';

// Poll cadence for the inspection query while the run is active
// (running/paused). Matches the run list's poll cadence.
export const RUN_INSPECTION_POLL_INTERVAL_MS = 12_000;

// A run counts as active — and worth polling — only while it is running or
// paused. Terminal runs (and not-yet-started pending ones) are fetched once:
// pollingInterval 0 stops the poll instead of hard-polling a finished run
// forever.
const ACTIVE_STATUSES = new Set(['running', 'paused']);

export interface AdapterStateView {
  // 'pretty' — well-formed JSON, re-serialized with 2-space indentation;
  // 'raw' — not JSON; the opaque string is shown as-is;
  // 'empty' — the adapter reported no state.
  kind: 'pretty' | 'raw' | 'empty';
  text: string;
}

// InspectRun returns the adapter's opaque state, pretty-printed upstream
// when well-formed. Parse defensively: whitespace-only counts as empty and
// malformed JSON never throws — it renders raw, so a buggy or hostile
// adapter payload can never blank the page.
export function adapterStateView(raw: string | undefined | null): AdapterStateView {
  const s = typeof raw === 'string' ? raw : '';
  if (!s.trim()) return { kind: 'empty', text: '' };
  try {
    return { kind: 'pretty', text: JSON.stringify(JSON.parse(s), null, 2) };
  } catch {
    return { kind: 'raw', text: s };
  }
}

function Value({
  children,
  mono = false,
  testId,
}: {
  children: ReactNode;
  mono?: boolean;
  testId?: string;
}) {
  return (
    <dd data-testid={testId} className={mono ? 'font-mono' : undefined}>
      {children}
    </dd>
  );
}

export function RunInspection({ runId, status }: { runId: string; status: string }) {
  const [stateOpen, setStateOpen] = useState(false);
  const active = ACTIVE_STATUSES.has(status);
  const inspection = useInspectRunQuery(
    { runId },
    // Active runs refresh on an interval without a page reload; every other
    // status is fetched once. Flipping the gate when the status prop
    // transitions (e.g. running -> succeeded) stops the poll.
    { pollingInterval: active ? RUN_INSPECTION_POLL_INTERVAL_MS : 0 },
  );

  const state = useMemo(
    () => adapterStateView(inspection.data?.stateJson),
    [inspection.data?.stateJson],
  );

  // Relative label computed against the render clock; each poll tick (active
  // runs) re-renders it, and a terminal run's label freezing at its last
  // fetch is the correct final answer.
  const now = Date.now();
  const lastActivity = inspection.data?.lastActivityAt;

  return (
    <section data-testid="run-inspection">
      <h3 className="text-lg font-semibold mb-2">Inspection</h3>
      {inspection.error ? (
        <p className="text-sm text-rose-400">Inspection unavailable.</p>
      ) : inspection.isLoading ? (
        <p className="text-sm text-slate-400">Loading inspection…</p>
      ) : (
        <>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm">
            <dt className="text-slate-400">Current step</dt>
            <Value mono testId="inspection-current-step">
              {inspection.data?.currentStep || '—'}
            </Value>
            <dt className="text-slate-400">Pending permissions</dt>
            <Value testId="inspection-pending-permissions">
              {inspection.data?.pendingPermissions ?? 0}
            </Value>
            <dt className="text-slate-400">Last activity</dt>
            <Value>
              {lastActivity ? (
                <span title={formatAbsoluteTime(lastActivity)}>
                  {formatRelativeTime(lastActivity, now)}
                </span>
              ) : (
                '—'
              )}
            </Value>
            <dt className="text-slate-400">Adapter</dt>
            <Value mono>{inspection.data?.adapter || '—'}</Value>
            <dt className="text-slate-400">Session</dt>
            <Value mono>{inspection.data?.sessionId || '—'}</Value>
          </dl>
          <div className="mt-2">
            {state.kind === 'empty' ? (
              <p className="text-sm text-slate-400" data-testid="adapter-state-empty">
                No adapter state.
              </p>
            ) : (
              <>
                <button
                  type="button"
                  data-testid="adapter-state-toggle"
                  aria-expanded={stateOpen}
                  onClick={() => setStateOpen((open) => !open)}
                  className="text-sm text-sky-300 hover:underline"
                >
                  {state.kind === 'raw' ? 'Adapter state (invalid JSON — raw value)' : 'Adapter state'}
                </button>
                {stateOpen && (
                  <pre
                    data-testid="adapter-state-json"
                    className="mt-2 text-xs font-mono bg-slate-900 rounded p-3 overflow-auto max-h-[32vh] whitespace-pre-wrap break-all"
                  >
                    {state.text}
                  </pre>
                )}
              </>
            )}
          </div>
        </>
      )}
    </section>
  );
}