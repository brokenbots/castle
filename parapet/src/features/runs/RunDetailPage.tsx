import { useEffect, useMemo } from 'react';
import { useParams } from 'react-router-dom';
import { useDispatch, useSelector } from 'react-redux';
import {
  useGetRunQuery,
  useListEventsQuery,
  EventEnvelope,
} from '../../api/castleApi';
import { selectRunEvents, selectPauseState, runsSlice } from './runsSlice';
import { subscriberIdForSession } from './subscriberId';
import { startWatch } from './watchRun';
import { StatusPill } from './StatusPill';
import { PauseAffordance } from './eventLog/PauseAffordance';
import { BranchDecisionEntry } from './eventLog/BranchDecisionEntry';
import { ForEachStrip } from './eventLog/ForEachStrip';
import { RunScopePanel } from './scopePanel/RunScopePanel';

function renderPayload(e: EventEnvelope) {
  const p = e.payload as Record<string, unknown> | undefined;
  if (e.type === 'stepLog') return String((p as { chunk?: string } | undefined)?.chunk ?? '');
  return JSON.stringify(p ?? null);
}

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
  const initial = useListEventsQuery({ runId: id });
  const dispatch = useDispatch();
  const live = useSelector(selectRunEvents(id));
  const pauseState = useSelector(selectPauseState(id));
  const subscriberId = useMemo(() => subscriberIdForSession(), []);

  useEffect(() => {
    if (!id) return;
    const ctrl = new AbortController();
    void startWatch(id, 0, subscriberId, dispatch, ctrl.signal);
    return () => {
      ctrl.abort();
      dispatch(runsSlice.actions.runCleared(id));
    };
  }, [id, dispatch, subscriberId]);

  const events = useMemo(() => {
    const bySeq = new Map<number, EventEnvelope>();
    for (const e of initial.data ?? []) bySeq.set(e.seq, e);
    for (const e of live) bySeq.set(e.seq, e);
    return Array.from(bySeq.values()).sort((a, b) => a.seq - b.seq);
  }, [initial.data, live]);

  const workflowSource = run.data?.workflowHash ?? '';
  const edges = workflowSource ? extractStepGraph(workflowSource) : [];

  // Group events by for_each node
  const forEachNodes = useMemo(() => {
    const nodes = new Map<string, EventEnvelope[]>();
    for (const e of events) {
      if (e.type === 'forEachEntered' || e.type === 'forEachIteration' || e.type === 'forEachOutcome') {
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
          {run.data.finalState && (
            <span className="text-sm">
              final: <span className="font-mono">{run.data.finalState}</span>
            </span>
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
        <div className="font-mono text-xs bg-slate-900 rounded p-3 max-h-[60vh] overflow-auto">
          {events.map((e) => {
            if (e.type === 'branchEvaluated') {
              return <BranchDecisionEntry key={e.seq} event={e} />;
            }
            return (
              <div key={e.seq} className="border-b border-slate-800/60 py-1 flex gap-3">
                <span className="text-slate-500 w-12 shrink-0">#{e.seq}</span>
                <span className="text-sky-400 w-40 shrink-0">{e.type}</span>
                <span className="whitespace-pre-wrap break-all">{renderPayload(e)}</span>
              </div>
            );
          })}
        </div>
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
