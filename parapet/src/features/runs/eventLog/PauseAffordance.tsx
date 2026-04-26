import { EventEnvelope } from '../../../api/castleApi';
import { DurationCountdown } from './DurationCountdown';
import { PendingSignalCard } from './PendingSignalCard';
import { ApprovalCard } from './ApprovalCard';

interface PauseAffordanceProps {
  runId: string;
  pauseEvent: EventEnvelope;
}

export function PauseAffordance({ runId, pauseEvent }: PauseAffordanceProps) {
  const payload = pauseEvent.payload as Record<string, unknown> | undefined;

  if (pauseEvent.type === 'waitEntered') {
    const mode = (payload?.mode as string) ?? '';
    const duration = (payload?.duration as string) ?? '';
    const signal = (payload?.signal as string) ?? '';

    if (mode === 'duration' && pauseEvent.ts) {
      return <DurationCountdown enteredAt={pauseEvent.ts} duration={duration} />;
    }

    if (mode === 'signal') {
      return <PendingSignalCard signal={signal} runId={runId} />;
    }
  }

  if (pauseEvent.type === 'approvalRequested') {
    const node = (payload?.node as string) ?? '';
    const approvers = (payload?.approvers as string[]) ?? [];
    const reason = (payload?.reason as string) ?? '';

    return <ApprovalCard node={node} runId={runId} approvers={approvers} reason={reason} />;
  }

  return null;
}
