import { useState } from 'react';
import { EventEnvelope } from '../../../api/castleApi';

interface ForEachStripProps {
  runId: string;
  events: EventEnvelope[];
}

export function ForEachStrip({ events }: ForEachStripProps) {
  const [expanded, setExpanded] = useState(false);

  // Find ForEachEntered, StepIterationStarted events, and StepIterationCompleted
  const entered = events.find((e) => e.type === 'forEachEntered');
  const iterations = events.filter((e) => e.type === 'stepIterationStarted');
  const outcome = events.find((e) => e.type === 'stepIterationCompleted');

  if (!entered) return null;

  const enteredPayload = entered.payload as Record<string, unknown> | undefined;
  const node = (enteredPayload?.node as string) ?? '';
  const count = (enteredPayload?.count as number) ?? 0;

  const current = iterations.length;

  // If completed, show summary
  if (outcome) {
    const outcomePayload = outcome.payload as Record<string, unknown> | undefined;
    const outcomeStr = (outcomePayload?.outcome as string) ?? '';
    const successCount = iterations.filter((it) => {
      const p = it.payload as Record<string, unknown> | undefined;
      return !(p?.anyFailed as boolean);
    }).length;

    return (
      <div className="bg-surface-raised/50 rounded px-4 py-2 border border-line mb-2">
        <button
          onClick={() => setExpanded(!expanded)}
          className="text-sm font-mono text-ink-muted hover:text-ink w-full text-left"
        >
          <span className="text-accent-strong">for_each</span> <span className="text-ink-faint">{node}</span> —{' '}
          <span className="text-success">
            {successCount}/{count} succeeded
          </span>
          {outcomeStr === 'any_failed' && (
            <span className="text-danger ml-2">({count - successCount} failed)</span>
          )}
          <span className="text-xs text-ink-faint ml-2">{expanded ? '▼' : '▶'}</span>
        </button>

        {expanded && (
          <div className="mt-2 space-y-1 text-xs font-mono">
            {iterations.map((it, idx) => {
              const p = it.payload as Record<string, unknown> | undefined;
              const index = (p?.index as number) ?? idx;
              const value = (p?.value as string) ?? '';
              const anyFailed = p?.anyFailed as boolean;
              return (
                <div key={it.seq} className="flex gap-2 text-ink-muted">
                  <span className="w-8">#{index}</span>
                  <span className="flex-1 truncate">{value}</span>
                  <span className={anyFailed ? 'text-danger' : 'text-success'}>
                    {anyFailed ? '✗' : '✓'}
                  </span>
                </div>
              );
            })}
          </div>
        )}
      </div>
    );
  }

  // In progress
  const dots = Array.from({ length: count }, (_, i) => (i < current ? '●' : '○'));

  return (
    <div className="bg-surface-raised/50 rounded px-4 py-2 border border-line mb-2">
      <p className="text-sm font-mono text-ink-muted">
        <span className="text-accent-strong">for_each</span> <span className="text-ink-faint">{node}</span> —{' '}
        <span className="text-accent-strong">
          [{dots.join('')}] {current} of {count}
        </span>
      </p>
    </div>
  );
}
