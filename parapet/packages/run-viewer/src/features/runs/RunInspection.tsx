import { useMemo, useState, type ReactNode } from 'react';
import { useInspectRunQuery } from '../../api/castleApi';
import { classifyError } from '../../api/errors';
import { DetailPanel } from '../../components/DetailPanel';
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

// Compact overview strip for the run detail page's static header: the same
// InspectRun data as the drawer's full panel, reduced to the state fields an
// operator scans at a glance. Polls on the same cadence gate.
export function RunInspectionSummary({ runId, status }: { runId: string; status: string }) {
  const active = ACTIVE_STATUSES.has(status);
  const inspection = useInspectRunQuery(
    { runId },
    { pollingInterval: active ? RUN_INSPECTION_POLL_INTERVAL_MS : 0 },
  );
  const now = Date.now();
  const lastActivity = inspection.data?.lastActivityAt;

  const chip = (label: string, value: ReactNode, mono = false, testId?: string) => (
    <span className="flex items-baseline gap-1">
      <span className="text-meta uppercase tracking-wide text-ink-faint">{label}</span>
      <span data-testid={testId} className={`text-body text-ink ${mono ? 'font-mono' : ''}`}>
        {value}
      </span>
    </span>
  );

  if (inspection.error) {
    return (
      <span className="text-body text-ink-muted" data-testid="inspection-summary-unavailable">
        details unavailable
      </span>
    );
  }

  return (
    <div data-testid="inspection-summary" className="flex flex-wrap items-center gap-x-4 gap-y-1">
      {chip(
        'step',
        inspection.data?.currentStep || '—',
        true,
        'inspection-summary-step',
      )}
      {chip('pending', inspection.data?.pendingPermissions ?? 0, false, 'inspection-summary-pending')}
      {chip(
        'active',
        lastActivity ? (
          <span title={formatAbsoluteTime(lastActivity)}>{formatRelativeTime(lastActivity, now)}</span>
        ) : (
          '—'
        ),
        false,
        'inspection-summary-active',
      )}
      {chip('adapter', inspection.data?.adapter || '—', true, 'inspection-summary-adapter')}
    </div>
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

  // Castle rejects an inspection when the caller is not the run's owner
  // (CRI-194): that is an access boundary, not a session failure, so it gets
  // an explicit access state instead of the generic "unavailable" notice —
  // and the global auth middleware must never treat it as session expiry.
  const errorKind = inspection.error ? classifyError(inspection.error) : undefined;

  // Relative label computed against the render clock; each poll tick (active
  // runs) re-renders it, and a terminal run's label freezing at its last
  // fetch is the correct final answer.
  const now = Date.now();
  const lastActivity = inspection.data?.lastActivityAt;

  return (
    <DetailPanel id="run-inspection-inner" title="Inspection" collapsible={false}>
      <div data-testid="run-inspection">
      {inspection.error ? (
        errorKind === 'forbidden' ? (
          <p className="text-sm text-danger" data-testid="inspection-access-denied">
            Access denied — your token is not allowed to inspect this run.
          </p>
        ) : (
          <p className="text-sm text-danger">Inspection unavailable.</p>
        )
      ) : inspection.isLoading ? (
        <p className="text-sm text-ink-muted">Loading inspection…</p>
      ) : (
        <>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm">
            <dt className="text-ink-muted">Current step</dt>
            <Value mono testId="inspection-current-step">
              {inspection.data?.currentStep || '—'}
            </Value>
            <dt className="text-ink-muted">Pending permissions</dt>
            <Value testId="inspection-pending-permissions">
              {inspection.data?.pendingPermissions ?? 0}
            </Value>
            <dt className="text-ink-muted">Last activity</dt>
            <Value>
              {lastActivity ? (
                <span title={formatAbsoluteTime(lastActivity)}>
                  {formatRelativeTime(lastActivity, now)}
                </span>
              ) : (
                '—'
              )}
            </Value>
            <dt className="text-ink-muted">Adapter</dt>
            <Value mono>{inspection.data?.adapter || '—'}</Value>
            <dt className="text-ink-muted">Session</dt>
            <Value mono>{inspection.data?.sessionId || '—'}</Value>
          </dl>
          <div className="mt-2">
            {state.kind === 'empty' ? (
              <p className="text-sm text-ink-muted" data-testid="adapter-state-empty">
                No adapter state.
              </p>
            ) : (
              <>
                <button
                  type="button"
                  data-testid="adapter-state-toggle"
                  aria-expanded={stateOpen}
                  onClick={() => setStateOpen((open) => !open)}
                  className="text-sm text-accent-strong hover:underline"
                >
                  {state.kind === 'raw' ? 'Adapter state (invalid JSON — raw value)' : 'Adapter state'}
                </button>
                {stateOpen && (
                  <pre
                    data-testid="adapter-state-json"
                    className="mt-2 text-xs font-mono bg-surface rounded p-3 overflow-auto max-h-[32vh] whitespace-pre-wrap break-all"
                  >
                    {state.text}
                  </pre>
                )}
              </>
            )}
          </div>
        </>
      )}
      </div>
    </DetailPanel>
  );
}