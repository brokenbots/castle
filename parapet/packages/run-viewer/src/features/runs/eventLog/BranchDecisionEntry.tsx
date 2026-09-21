import { useState } from 'react';
import { EventEnvelope } from '../../../api/castleApi';

interface BranchDecisionEntryProps {
  event: EventEnvelope;
}

export function BranchDecisionEntry({ event }: BranchDecisionEntryProps) {
  const [expanded, setExpanded] = useState(false);
  const payload = event.payload as Record<string, unknown> | undefined;

  const node = (payload?.node as string) ?? '';
  const matchedArm = (payload?.matchedArm as string) ?? '';
  const target = (payload?.target as string) ?? '';
  const condition = (payload?.condition as string) ?? '';

  return (
    <div className="border-b border-slate-800/60 py-2">
      <div className="flex gap-3 items-start">
        <span className="text-slate-500 w-12 shrink-0">#{event.seq}</span>
        <span className="text-sky-400 w-40 shrink-0">{event.type}</span>
        <div className="flex-1">
          <button
            onClick={() => setExpanded(!expanded)}
            className="text-left hover:bg-slate-800/50 rounded px-2 py-1 w-full"
          >
            <span className="font-mono text-xs">
              <span className="text-slate-400">node:</span> <span className="text-white">{node}</span>
              {' → '}
              <span className="text-slate-400">matched:</span>{' '}
              <span className="text-amber-400">{matchedArm}</span>
              {' → '}
              <span className="text-slate-400">target:</span> <span className="text-green-400">{target}</span>
            </span>
            {condition && (
              <div className="text-xs text-slate-500 mt-1">
                <span className="text-slate-400">condition:</span> {condition}
              </div>
            )}
            <span className="text-xs text-slate-500 ml-2">{expanded ? '▼' : '▶'}</span>
          </button>

          {expanded && (
            <div className="mt-2 ml-2 pl-3 border-l-2 border-slate-700">
              <p className="text-xs text-slate-400 mb-1">Decision details:</p>
              <div className="font-mono text-xs space-y-1">
                <div>
                  <span className="text-slate-500">Node:</span> <span className="text-white">{node}</span>
                </div>
                <div>
                  <span className="text-slate-500">Matched Arm:</span>{' '}
                  <span className="text-amber-400">{matchedArm}</span>
                </div>
                <div>
                  <span className="text-slate-500">Target:</span>{' '}
                  <span className="text-green-400">{target}</span>
                </div>
                {condition && (
                  <div>
                    <span className="text-slate-500">Condition:</span>{' '}
                    <span className="text-slate-300">{condition}</span>
                  </div>
                )}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
