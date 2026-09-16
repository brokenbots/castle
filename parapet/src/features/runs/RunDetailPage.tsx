import { useMemo } from 'react';
import { useParams } from 'react-router-dom';
import { useSelector } from 'react-redux';
import { useGetRunQuery, type EventEnvelope } from '../../api/castleApi';
import { selectPauseState } from './runsSlice';
import { useRunEventLog } from './eventLog/useRunEventLog';
import { EventLog } from './eventLog/EventLog';
import { StatusPill } from './StatusPill';
import { PauseAffordance } from './eventLog/PauseAffordance';
import { ForEachStrip } from './eventLog/ForEachStrip';
import { RunScopePanel } from './scopePanel/RunScopePanel';

type Edge = { from: string; to: string; via: string };

function extractStepGraph(source: string): Edge[] {
  const edges: Edge[] = [];
  const stepBlocks = source.match(/step\s+"[^"]+"\s*\{[\s\S]*?\n\}/g) ?? [];
  for (const block of stepBlocks) {
    const stepName = block.match(/step\s+"([^"]+)"/)?.[1];
    if (!stepName) continue;
    const transitions = block.matchAll(/"([^"]+)"\s*=\s*"([^"]+)"/g);
    for (const tr of transitions) {
      edges.push({ from: stepName, via: tr[1], to: tr[2] });
    }
  }
  return edges;
}

export function RunDetailPage() {
  const { id = '' } = useParams();
  const run = useGetRunQuery(id);
  const { events, log, loadEarlier } = useRunEventLog(id);
  const pauseState = useSelector(selectPauseState(id));

  const workflowSource = run.data?.workflowHash ?? '';
  const edges = workflowSource ? extractStepGraph(workflowSource) : [];
  // Only render the PR link for http(s) URLs; the publisher controls the
  // value and must not be able to inject javascript: hrefs.
  const prUrl = run.data?.prUrl?.startsWith('http://') || run.data?.prUrl?.startsWith('https://') ? run.data.prUrl : undefined;

  // Group events by for_each node
  const forEachNodes = useMemo(() => {
    const nodes = new Map<string, EventEnvelope[]>();
    for (const e of events) {
      if (e.type === 'forEachEntered' || e.type === 'stepIterationStarted' || e.type === 'stepIterationCompleted') {
        const payload = e.payload as Record<string, unknown> | undefined;
        const node = (payload?.node as string) ?? '';
        if (node) {
          if (!nodes.has(node)) nodes.set(node, []);
          nodes.get(node)!.push(e);
        }
      }
    }
    return nodes;
  }, [events]);

  if (run.isLoading) return <p>Loading…</p>;
  if (run.error || !run.data) return <p className="text-rose-400">Run not found.</p>;

  return (
    <div className="flex flex-col gap-4">
      <RunScopePanel events={events} />
      <header>
        <h2 className="text-2xl font-semibold">{run.data.workflowName}</h2>
        <p className="text-sm text-slate-400 font-mono">{run.data.runId}</p>
        <div className="mt-2 flex items-start gap-4">
          <StatusPill status={run.data.status} pauseEvent={pauseState.pauseEvent} />
          {run.data.ticket && (
            <span className="text-sm">
              ticket: <span className="font-mono">{run.data.ticket}</span>
            </span>
          )}
          {run.data.repoUrl && (
            <span className="text-sm">
              repo: <span className="font-mono">{run.data.repoUrl}</span>
            </span>
          )}
          {run.data.finalState && (
            <span className="text-sm">
              final: <span className="font-mono">{run.data.finalState}</span>
            </span>
          )}
          {prUrl && (
            <a className="text-sm text-sky-400 hover:underline" href={prUrl} target="_blank" rel="noreferrer">
              PR
            </a>
          )}
        </div>
      </header>

      {pauseState.isPaused && pauseState.pauseEvent && (
        <section>
          <PauseAffordance runId={id} pauseEvent={pauseState.pauseEvent} />
        </section>
      )}

      {forEachNodes.size > 0 && (
        <section>
          {Array.from(forEachNodes.entries()).map(([node, nodeEvents]) => (
            <ForEachStrip key={node} runId={id} events={nodeEvents} />
          ))}
        </section>
      )}

      <section>
        <h3 className="text-lg font-semibold mb-2">Events</h3>
        <EventLog
          events={events}
          hasEarlier={log.hasEarlier}
          loadingEarlier={log.loadingEarlier}
          onLoadEarlier={loadEarlier}
        />
      </section>
      <section>
        <h3 className="text-lg font-semibold mb-2">Workflow source</h3>
        <pre className="text-xs font-mono bg-slate-900 rounded p-3 overflow-auto max-h-[32vh]">
          {workflowSource}
        </pre>
      </section>
      <section>
        <h3 className="text-lg font-semibold mb-2">Step graph</h3>
        {edges.length === 0 ? (
          <p className="text-sm text-slate-400">No step transitions found.</p>
        ) : (
          <div className="bg-slate-900 rounded p-3 text-xs font-mono">
            {edges.map((edge, i) => (
              <div key={`${edge.from}:${edge.via}:${edge.to}:${i}`} className="py-1 border-b last:border-b-0 border-slate-800">
                <span className="text-sky-300">{edge.from}</span>
                <span className="text-slate-500"> --{edge.via}--&gt; </span>
                <span className="text-emerald-300">{edge.to}</span>
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}
