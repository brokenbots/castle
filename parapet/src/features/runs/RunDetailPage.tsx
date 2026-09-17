import { useMemo, useState } from 'react';
import { useParams } from 'react-router-dom';
import { useSelector } from 'react-redux';
import { useGetRunQuery, type EventEnvelope } from '../../api/castleApi';
import { selectPauseState } from './runsSlice';
import { useRunEventLog } from './eventLog/useRunEventLog';
import { EventLog } from './eventLog/EventLog';
import { StatusPill } from './StatusPill';
import { RunControls } from './RunControls';
import { RunInspection } from './RunInspection';
import { PauseAffordance } from './eventLog/PauseAffordance';
import { ForEachStrip } from './eventLog/ForEachStrip';
import { RunScopePanel } from './scopePanel/RunScopePanel';
import { PageHeader } from '../../components/PageHeader';
import { DockedPanel } from '../../components/DockedPanel';
import { extractTextEdges, parseWorkflowHcl, type WorkflowGraph } from './workflowGraph/parseWorkflowHcl';
import { WorkflowDag } from './workflowGraph/WorkflowDag';
import { eventBelongsToStep, selectNodeOverlay } from './workflowGraph/nodeStatus';

/**
 * Parses the run's workflow HCL into the DAG model. The `workflowHash` run
 * field carries the full workflow source (castle rpc mapping), so the graph
 * is derived entirely client-side. Any parse failure — or a source with no
 * parseable nodes — yields null and the page keeps the text-edge fallback,
 * so the panel never blanks.
 */
function parseGraph(source: string): WorkflowGraph | null {
  if (!source) return null;
  try {
    const graph = parseWorkflowHcl(source);
    return graph.nodes.length > 0 ? graph : null;
  } catch {
    // Any failure (typed or not — e.g. a RangeError from pathologically
    // nested input) falls back; the page has no error boundary, so an
    // escaping throw would blank it.
    return null;
  }
}

export function RunDetailPage() {
  const { id = '' } = useParams();
  const run = useGetRunQuery(id);
  const { events, log, loadEarlier } = useRunEventLog(id);
  const pauseState = useSelector(selectPauseState(id));
  const [selectedStep, setSelectedStep] = useState<{ runId: string; step: string } | null>(null);
  // The scope view lives in a docked right-side panel (part of the page
  // layout, never overlapping the log). The dock can be closed and reopened
  // from the header's Scope toggle.
  const [scopeOpen, setScopeOpen] = useState(true);

  const workflowSource = run.data?.workflowHash ?? '';
  const graph = useMemo(() => parseGraph(workflowSource), [workflowSource]);
  const fallbackEdges = useMemo(
    () => (graph ? [] : workflowSource ? extractTextEdges(workflowSource) : []),
    [graph, workflowSource],
  );
  // The overlay maps the event stream onto graph nodes: running (pulsing)
  // for the active step, succeeded/failed for finished ones, per-iteration
  // for_each progress from ForEachStrip data, and unvisited nodes dimmed.
  const overlay = useMemo(
    () => (graph ? selectNodeOverlay(events) : { statuses: {}, forEach: {} }),
    [graph, events],
  );
  const selected = selectedStep && selectedStep.runId === (run.data?.runId ?? '') ? selectedStep.step : null;
  const visibleEvents = useMemo(
    () => (selected ? events.filter((e) => eventBelongsToStep(e, selected)) : events),
    [events, selected],
  );

  // Only render the PR link for http(s) URLs; the publisher controls the
  // value and must not be able to inject javascript: hrefs.
  const prUrl = run.data?.prUrl?.startsWith('http://') || run.data?.prUrl?.startsWith('https://') ? run.data.prUrl : undefined;

  // A run is live-tailing while its status is running and no terminal event
  // has arrived yet (the status can lag the event stream).
  const running = useMemo(() => {
    if (!run.data || run.data.status !== 'running') return false;
    return !events.some((e) => e.type === 'runCompleted' || e.type === 'runFailed');
  }, [run.data, events]);

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
  if (run.error || !run.data) return <p className="text-danger">Run not found.</p>;

  return (
    <div className="flex h-full min-h-0 items-stretch gap-4" data-testid="run-detail-layout">
      <div className="flex min-w-0 flex-1 flex-col gap-4 overflow-y-auto pr-1">
        <PageHeader
          title={run.data.workflowName}
          meta={run.data.runId}
          actions={
            <>
              <StatusPill status={run.data.status} pauseEvent={pauseState.pauseEvent} />
              <RunControls runId={run.data.runId} status={run.data.status} pauseState={pauseState} />
              <button
                type="button"
                data-testid="scope-toggle"
                aria-pressed={scopeOpen}
                onClick={() => setScopeOpen((open) => !open)}
                className="rounded-md border border-line-strong px-3 py-1.5 text-body text-ink-muted hover:bg-surface-raised hover:text-ink"
              >
                Scope
              </button>
            </>
          }
        >
          <div className="flex flex-wrap items-center gap-4 text-body">
            {run.data.ticket && (
              <span>
                ticket: <span className="font-mono">{run.data.ticket}</span>
              </span>
            )}
            {run.data.repoUrl && (
              <span>
                repo: <span className="font-mono">{run.data.repoUrl}</span>
              </span>
            )}
            {run.data.finalState && (
              <span>
                final: <span className="font-mono">{run.data.finalState}</span>
              </span>
            )}
            {prUrl && (
              <a className="text-accent-strong hover:underline" href={prUrl} target="_blank" rel="noreferrer">
                PR
              </a>
            )}
          </div>
        </PageHeader>

        <RunInspection runId={id} status={run.data.status} />

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

        {selected && (
          <div className="flex items-center gap-2 text-xs" data-testid="step-filter">
            <span className="rounded bg-sky-950 px-2 py-1 text-sky-300 border border-sky-800 font-mono">
              Filtered to step: {selected}
            </span>
            <button
              type="button"
              data-testid="clear-step-filter"
              onClick={() => setSelectedStep(null)}
              className="text-slate-400 hover:text-slate-200"
            >
              Clear filter
            </button>
          </div>
        )}

        <section>
          <h3 className="text-lg font-semibold mb-2">Events</h3>
          <EventLog
            events={visibleEvents}
            running={running}
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
          {graph ? (
            <WorkflowDag
              graph={graph}
              statuses={overlay.statuses}
              forEachProgress={overlay.forEach}
              selectedId={selected}
              onSelect={(nodeId) =>
                setSelectedStep(nodeId === null ? null : { runId: run.data!.runId, step: nodeId })
              }
            />
          ) : fallbackEdges.length === 0 ? (
            <p className="text-sm text-slate-400">No step transitions found.</p>
          ) : (
            <div className="bg-slate-900 rounded p-3 text-xs font-mono">
              {fallbackEdges.map((edge, i) => (
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
      {scopeOpen && (
        <DockedPanel title="Run Scope" testId="scope-dock" onClose={() => setScopeOpen(false)}>
          <RunScopePanel events={events} />
        </DockedPanel>
      )}
    </div>
  );
}
