import type { EventEnvelope } from '../../api/castleApi';
import { RUN_STATUS_PILL_CLASSES, RUN_STATUS_TEXT_COLORS } from './runStatus';

interface StatusPillProps {
  status: string;
  pauseEvent?: EventEnvelope | null;
}

// The run-detail header pill: solid status-colored chip plus, for a paused
// run, the concrete wait reason (duration countdown, awaited signal, or
// approval queue) derived from the pending pause event.
export function StatusPill({ status, pauseEvent }: StatusPillProps) {
  let displayStatus = status;
  let pillClass = RUN_STATUS_PILL_CLASSES[status] ?? 'bg-surface-raised text-ink';
  let pauseReason = '';

  if (pauseEvent) {
    displayStatus = 'paused';
    pillClass = RUN_STATUS_PILL_CLASSES.paused;

    const payload = pauseEvent.payload as Record<string, unknown> | undefined;

    if (pauseEvent.type === 'waitEntered') {
      const mode = (payload?.mode as string) ?? '';
      const signal = (payload?.signal as string) ?? '';
      const duration = (payload?.duration as string) ?? '';

      if (mode === 'duration') {
        pauseReason = `waiting ${duration}`;
      } else if (mode === 'signal') {
        pauseReason = `waiting for signal "${signal}"`;
      }
    }

    if (pauseEvent.type === 'approvalRequested') {
      const approvers = (payload?.approvers as string[]) ?? [];
      pauseReason =
        approvers.length > 0
          ? `awaiting approval from ${approvers.join(', ')}`
          : 'awaiting approval';
    }
  }

  return (
    <div className="inline-flex flex-col items-start gap-1">
      <span
        className={`inline-block rounded-full px-3 py-1 text-meta font-semibold ${pillClass}`}
      >
        {displayStatus}
      </span>
      {pauseReason && (
        <span className={`text-meta ${RUN_STATUS_TEXT_COLORS.paused} ml-1`}>{pauseReason}</span>
      )}
    </div>
  );
}