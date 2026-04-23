import { useParams } from 'react-router-dom';
import { useGetRunQuery, useListEventsQuery, EventEnvelope } from '../../api/castleApi';

function renderPayload(e: EventEnvelope) {
  const p = e.payload as Record<string, unknown>;
  if (e.type === 'step.log') return String((p as { chunk?: string }).chunk ?? '');
  return JSON.stringify(p);
}

export function RunDetailPage() {
  const { id = '' } = useParams();
  const run = useGetRunQuery(id, { pollingInterval: 1500 });
  const events = useListEventsQuery({ runId: id }, { pollingInterval: 1000 });

  if (run.isLoading) return <p>Loading…</p>;
  if (run.error || !run.data) return <p className="text-rose-400">Run not found.</p>;

  return (
    <div className="flex flex-col gap-4">
      <header>
        <h2 className="text-2xl font-semibold">{run.data.WorkflowName}</h2>
        <p className="text-sm text-slate-400 font-mono">{run.data.ID}</p>
        <p className="mt-2 text-sm">
          <span className="mr-4">status: <span className="font-semibold">{run.data.Status}</span></span>
          <span>step: <span className="font-mono">{run.data.CurrentStep}</span></span>
        </p>
      </header>
      <section>
        <h3 className="text-lg font-semibold mb-2">Events</h3>
        <div className="font-mono text-xs bg-slate-900 rounded p-3 max-h-[60vh] overflow-auto">
          {(events.data ?? []).map((e) => (
            <div key={e.seq} className="border-b border-slate-800/60 py-1 flex gap-3">
              <span className="text-slate-500 w-12 shrink-0">#{e.seq}</span>
              <span className="text-sky-400 w-40 shrink-0">{e.type}</span>
              <span className="whitespace-pre-wrap break-all">{renderPayload(e)}</span>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}
