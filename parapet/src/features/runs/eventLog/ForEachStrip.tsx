import { useState } from 'react';
import { EventEnvelope } from '../../../api/castleApi';

interface ForEachStripProps {
  runId: string;
  events: EventEnvelope[];
}

export function ForEachStrip({ events }: ForEachStripProps) {
  const [expanded, setExpanded] = useState(false);

  // Find ForEachEntered, ForEachIteration events, and ForEachOutcome
  const entered = events.find((e) => e.type === 'forEachEntered');
  const iterations = events.filter((e) => e.type === 'forEachIteration');
  const outcome = events.find((e) => e.type === 'forEachOutcome');

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
      <div className="bg-slate-800/50 rounded px-4 py-2 border border-slate-700 mb-2">
        <button
          onClick={() => setExpanded(!expanded)}
          className="text-sm font-mono text-slate-300 hover:text-white w-full text-left"
        >
          <span className="text-purple-400">for_each</span> <span className="text-slate-500">{node}</span> —{' '}
          <span className="text-green-400">
            {successCount}/{count} succeeded
          </span>
          {outcomeStr === 'any_failed' && (
            <span className="text-rose-400 ml-2">({count - successCount} failed)</span>
          )}
          <span className="text-xs text-slate-500 ml-2">{expanded ? '▼' : '▶'}</span>
        </button>

        {expanded && (
          <div className="mt-2 space-y-1 text-xs font-mono">
            {iterations.map((it, idx) => {
              const p = it.payload as Record<string, unknown> | undefined;
              const index = (p?.index as number) ?? idx;
              const value = (p?.value as string) ?? '';
              const anyFailed = p?.anyFailed as boolean;
              return (
                <div key={it.seq} className="flex gap-2 text-slate-400">
                  <span className="w-8">#{index}</span>
                  <span className="flex-1 truncate">{value}</span>
                  <span className={anyFailed ? 'text-rose-400' : 'text-green-400'}>
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
    <div className="bg-slate-800/50 rounded px-4 py-2 border border-slate-700 mb-2">
      <p className="text-sm font-mono text-slate-300">
        <span className="text-purple-400">for_each</span> <span className="text-slate-500">{node}</span> —{' '}
        <span className="text-sky-400">
          [{dots.join('')}] {current} of {count}
        </span>
      </p>
    </div>
  );
}
