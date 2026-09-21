import { EventEnvelope } from '../../api/castleApi';

interface StatusPillProps {
  status: string;
  pauseEvent?: EventEnvelope | null;
}

export function StatusPill({ status, pauseEvent }: StatusPillProps) {
  // Determine display status and styling
  let displayStatus = status;
  let bgColor = 'bg-slate-600';
  let textColor = 'text-slate-200';
  let pauseReason = '';

  if (pauseEvent) {
    displayStatus = 'paused';
    bgColor = 'bg-amber-600';
    textColor = 'text-white';

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
      if (approvers.length > 0) {
        pauseReason = `awaiting approval from ${approvers.join(', ')}`;
      } else {
        pauseReason = 'awaiting approval';
      }
    }
  } else {
    // Set colors based on status
    switch (status) {
      case 'running':
        bgColor = 'bg-blue-600';
        textColor = 'text-white';
        break;
      case 'completed':
      case 'succeeded':
        bgColor = 'bg-green-600';
        textColor = 'text-white';
        break;
      case 'failed':
        bgColor = 'bg-rose-600';
        textColor = 'text-white';
        break;
      case 'cancelled':
        bgColor = 'bg-slate-600';
        textColor = 'text-white';
        break;
    }
  }

  return (
    <div className="inline-flex flex-col items-start gap-1">
      <span className={`inline-block px-3 py-1 rounded-full text-xs font-semibold ${bgColor} ${textColor}`}>
        {displayStatus}
      </span>
      {pauseReason && (
        <span className="text-xs text-slate-400 ml-1">{pauseReason}</span>
      )}
    </div>
  );
}
