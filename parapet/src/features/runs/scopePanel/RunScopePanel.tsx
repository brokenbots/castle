import { useMemo } from 'react';
import { EventEnvelope } from '../../../api/castleApi';

interface RunScopePanelProps {
  events: EventEnvelope[];
}

interface ScopeState {
  variables: Map<string, { value: string; source: string }>;
  stepOutputs: Map<string, Map<string, string>>;
}

// Scope view rendered inside the run page's docked panel. Derives the
// current scope from the event stream: VariableSet events fold into the
// variable map, StepOutputCaptured events into the per-step output map.
export function RunScopePanel({ events }: RunScopePanelProps) {
  const scope = useMemo<ScopeState>(() => {
    const derived: ScopeState = { variables: new Map(), stepOutputs: new Map() };

    for (const event of events) {
      const payload = event.payload as Record<string, unknown> | undefined;

      if (event.type === 'variableSet') {
        const name = (payload?.name as string) ?? '';
        const value = (payload?.value as string) ?? '';
        const source = (payload?.source as string) ?? '';
        if (name) {
          derived.variables.set(name, { value, source });
        }
      }

      if (event.type === 'stepOutputCaptured') {
        const step = (payload?.step as string) ?? '';
        const outputs = (payload?.outputs as Record<string, string>) ?? {};
        if (step) {
          derived.stepOutputs.set(step, new Map(Object.entries(outputs)));
        }
      }
    }

    return derived;
  }, [events]);

  return (
    <div data-testid="run-scope-panel" className="flex flex-col gap-4">
      <section>
        <h4 className="mb-2 text-meta font-semibold uppercase tracking-wide text-ink-muted">Variables</h4>
        {scope.variables.size === 0 ? (
          <p className="text-meta italic text-ink-faint">No variables set</p>
        ) : (
          <div className="space-y-2">
            {Array.from(scope.variables.entries()).map(([name, { value, source }]) => (
              <div key={name} className="rounded-md bg-surface-raised p-2">
                <div className="flex items-start justify-between gap-2">
                  <span className="font-mono text-meta text-accent-strong">var.{name}</span>
                  <span className="text-meta text-ink-faint">{source}</span>
                </div>
                <div className="mt-1 break-all font-mono text-meta text-ink-muted">{value}</div>
              </div>
            ))}
          </div>
        )}
      </section>

      <section>
        <h4 className="mb-2 text-meta font-semibold uppercase tracking-wide text-ink-muted">Step Outputs</h4>
        {scope.stepOutputs.size === 0 ? (
          <p className="text-meta italic text-ink-faint">No step outputs captured</p>
        ) : (
          <div className="space-y-3">
            {Array.from(scope.stepOutputs.entries()).map(([step, outputs]) => (
              <div key={step} className="rounded-md bg-surface-raised p-2">
                <div className="mb-1 font-mono text-meta text-purple-400">steps.{step}</div>
                <div className="ml-2 space-y-1">
                  {Array.from(outputs.entries()).map(([key, val]) => (
                    <div key={key} className="flex items-start gap-2">
                      <span className="font-mono text-meta text-ink-faint">{key}:</span>
                      <span className="flex-1 break-all font-mono text-meta text-ink-muted">{val}</span>
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}