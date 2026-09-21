import { EventEnvelope } from '../../../api/castleApi';
import { DurationCountdown } from './DurationCountdown';
import { PendingSignalCard } from './PendingSignalCard';
import { ApprovalCard } from './ApprovalCard';
import type { RunCapabilities } from '../capabilities';

interface PauseAffordanceProps {
  runId: string;
  pauseEvent: EventEnvelope;
  /** Re-fetches the run and re-anchors the event log; used by the pending signal card. */
  onRefresh: () => void;
  /**
   * Capability probe result; defaults to the castle host where the control
   * RPCs exist. Hosts without them render the pending actions grayed-out.
   */
  capabilities?: RunCapabilities;
}

export function PauseAffordance({ runId, pauseEvent, onRefresh, capabilities }: PauseAffordanceProps) {
  const payload = pauseEvent.payload as Record<string, unknown> | undefined;

  if (pauseEvent.type === 'waitEntered') {
    const mode = (payload?.mode as string) ?? '';
    const duration = (payload?.duration as string) ?? '';
    const signal = (payload?.signal as string) ?? '';

    if (mode === 'duration' && pauseEvent.ts) {
      return <DurationCountdown enteredAt={pauseEvent.ts} duration={duration} />;
    }

    if (mode === 'signal') {
      return (
        <PendingSignalCard signal={signal} runId={runId} onRefresh={onRefresh} capabilities={capabilities} />
      );
    }
  }

  if (pauseEvent.type === 'approvalRequested') {
    const node = (payload?.node as string) ?? '';
    const approvers = (payload?.approvers as string[]) ?? [];
    const reason = (payload?.reason as string) ?? '';

    return (
      <ApprovalCard node={node} runId={runId} approvers={approvers} reason={reason} capabilities={capabilities} />
    );
  }

  return null;
}
