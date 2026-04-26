import { useState } from 'react';
import { EventEnvelope } from '../../../api/castleApi';

interface RunScopePanelProps {
  events: EventEnvelope[];
}

interface ScopeState {
  variables: Map<string, { value: string; source: string }>;
  stepOutputs: Map<string, Map<string, string>>;
}

export function RunScopePanel({ events }: RunScopePanelProps) {
  const [isOpen, setIsOpen] = useState(false);

  // Derive current scope from events
  const scope: ScopeState = { variables: new Map(), stepOutputs: new Map() };

  for (const event of events) {
    const payload = event.payload as Record<string, unknown> | undefined;

    if (event.type === 'variableSet') {
      const name = (payload?.name as string) ?? '';
      const value = (payload?.value as string) ?? '';
      const source = (payload?.source as string) ?? '';
      if (name) {
        scope.variables.set(name, { value, source });
      }
    }

    if (event.type === 'stepOutputCaptured') {
      const step = (payload?.step as string) ?? '';
      const outputs = (payload?.outputs as Record<string, string>) ?? {};
      if (step) {
        scope.stepOutputs.set(step, new Map(Object.entries(outputs)));
      }
    }
  }

  if (!isOpen) {
    return (
      <button
        onClick={() => setIsOpen(true)}
        className="fixed top-20 right-4 bg-slate-800 hover:bg-slate-700 border border-slate-600 rounded px-3 py-2 text-sm font-semibold text-slate-300 shadow-lg"
      >
        📊 Scope
      </button>
    );
  }

  return (
    <div className="fixed top-20 right-4 w-80 bg-slate-900 border border-slate-700 rounded shadow-xl max-h-[70vh] overflow-hidden flex flex-col">
      <div className="flex items-center justify-between px-4 py-3 border-b border-slate-700">
        <h3 className="text-sm font-semibold text-white">Run Scope</h3>
        <button
          onClick={() => setIsOpen(false)}
          className="text-slate-400 hover:text-white text-lg leading-none"
        >
          ×
        </button>
      </div>

      <div className="overflow-y-auto flex-1 p-4 space-y-4">
        {/* Variables */}
        <section>
          <h4 className="text-xs font-semibold text-slate-400 mb-2 uppercase tracking-wide">Variables</h4>
          {scope.variables.size === 0 ? (
            <p className="text-xs text-slate-500 italic">No variables set</p>
          ) : (
            <div className="space-y-2">
              {Array.from(scope.variables.entries()).map(([name, { value, source }]) => (
                <div key={name} className="bg-slate-800/50 rounded p-2">
                  <div className="flex items-start justify-between gap-2">
                    <span className="font-mono text-xs text-sky-400">var.{name}</span>
                    <span className="text-xs text-slate-500">{source}</span>
                  </div>
                  <div className="font-mono text-xs text-slate-300 mt-1 break-all">{value}</div>
                </div>
              ))}
            </div>
          )}
        </section>

        {/* Step Outputs */}
        <section>
          <h4 className="text-xs font-semibold text-slate-400 mb-2 uppercase tracking-wide">Step Outputs</h4>
          {scope.stepOutputs.size === 0 ? (
            <p className="text-xs text-slate-500 italic">No step outputs captured</p>
          ) : (
            <div className="space-y-3">
              {Array.from(scope.stepOutputs.entries()).map(([step, outputs]) => (
                <div key={step} className="bg-slate-800/50 rounded p-2">
                  <div className="font-mono text-xs text-purple-400 mb-1">steps.{step}</div>
                  <div className="space-y-1 ml-2">
                    {Array.from(outputs.entries()).map(([key, val]) => (
                      <div key={key} className="flex items-start gap-2">
                        <span className="font-mono text-xs text-slate-500">{key}:</span>
                        <span className="font-mono text-xs text-slate-300 break-all flex-1">{val}</span>
                      </div>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}
