import type { EventEnvelope } from '../../../api/castleApi';

/** Live overlay state of one graph node, derived from the event stream. */
export type StepNodeStatus = 'idle' | 'running' | 'succeeded' | 'failed';

/**
 * Per-iteration state of a for_each node (the data ForEachStrip renders):
 * `started` counts begun iterations, `total` the declared item count (null
 * until forEachEntered), `outcome` the aggregate completion outcome and
 * `anyFailed` whether any iteration failed so far.
 */
export interface ForEachProgress {
  total: number | null;
  started: number;
  outcome: string | null;
  anyFailed: boolean;
}

export interface NodeOverlay {
  statuses: Record<string, StepNodeStatus>;
  forEach: Record<string, ForEachProgress>;
}

function payloadOf(e: EventEnvelope): Record<string, unknown> | undefined {
  return e.payload as Record<string, unknown> | undefined;
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined;
}

function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

/**
 * Walks the run's event stream in order and derives the live overlay state:
 * running (pulsing) for the active node, succeeded/failed for completed
 * ones, and — via ForEachStrip data (forEachEntered/stepIteration*) —
 * per-iteration progress for for_each nodes. Nodes without an event stay
 * unvisited (idle/dimmed). Later events win, so a retried step flips back to
 * running and a failed completion overrides an earlier success.
 */
export function selectNodeOverlay(events: EventEnvelope[]): NodeOverlay {
  const statuses: Record<string, StepNodeStatus> = {};
  const forEach: Record<string, ForEachProgress> = {};

  const progress = (node: string): ForEachProgress => {
    let p = forEach[node];
    if (!p) {
      p = { total: null, started: 0, outcome: null, anyFailed: false };
      forEach[node] = p;
    }
    return p;
  };

  for (const e of events) {
    const p = payloadOf(e);
    switch (e.type) {
      case 'stepEntered': {
        const step = str(p?.step);
        if (step) statuses[step] = 'running';
        break;
      }
      case 'stepOutcome': {
        const step = str(p?.step);
        if (!step) break;
        // The engine's dominant failure paths emit `outcome: "failure"` (or
        // any non-success verdict) with no error payload, so only a
        // success/ok outcome without an error text counts as succeeded —
        // the same rule the engine's ConsoleSink and watch CLI apply.
        const outcome = str(p?.outcome);
        const failed = !(outcome === 'success' || outcome === 'ok') || str(p?.error) !== undefined;
        statuses[step] = failed ? 'failed' : 'succeeded';
        break;
      }
      case 'stepTransition': {
        // Fallback for streams that only carry transitions: the from-node
        // moved on, so it completed; the to-node is REACHED (this is the
        // only event a terminal state node ever gets — no stepEntered
        // fires for state nodes — so it must light here). Later events
        // win: an immediately following stepEntered overrides to running,
        // and a failed verdict is never downgraded.
        const from = str(p?.from);
        if (from && statuses[from] !== 'failed') statuses[from] = 'succeeded';
        const to = str(p?.to);
        if (to && statuses[to] !== 'failed') statuses[to] = 'succeeded';
        break;
      }
      case 'forEachEntered': {
        const node = str(p?.node);
        if (!node) break;
        statuses[node] = 'running';
        progress(node).total = num(p?.count) ?? null;
        break;
      }
      case 'stepIterationStarted': {
        const node = str(p?.node);
        if (!node) break;
        statuses[node] = 'running';
        const p2 = progress(node);
        p2.started = Math.max(p2.started, (num(p?.index) ?? p2.started) + 1);
        // Payloads are protojson camelCase (mapEnvelope toJson).
        if (p?.anyFailed === true) p2.anyFailed = true;
        break;
      }
      case 'stepIterationCompleted': {
        const node = str(p?.node);
        if (!node) break;
        const p2 = progress(node);
        p2.outcome = str(p?.outcome) ?? null;
        statuses[node] = p2.outcome === 'any_failed' ? 'failed' : 'succeeded';
        break;
      }
      case 'branchEvaluated': {
        const node = str(p?.node);
        if (node) statuses[node] = 'succeeded';
        break;
      }
      case 'waitEntered': {
        const node = str(p?.node);
        if (node) statuses[node] = 'running';
        break;
      }
      case 'waitResumed': {
        const node = str(p?.node);
        if (node) statuses[node] = 'succeeded';
        break;
      }
      case 'approvalRequested': {
        const node = str(p?.node);
        if (node) statuses[node] = 'running';
        break;
      }
      case 'approvalDecision': {
        const node = str(p?.node);
        if (node) statuses[node] = 'succeeded';
        break;
      }
      case 'runFailed': {
        const step = str(p?.step);
        if (step) statuses[step] = 'failed';
        // A run that ends in a failure-named terminal state marks that node
        // even though no stepEntered event ever fires for terminal states.
        const failState = str(p?.finalState) ?? str(p?.final_state);
        if (failState) statuses[failState] = 'failed';
        // The terminal event may not name a step; fail whatever is running.
        for (const [id, status] of Object.entries(statuses)) {
          if (status === 'running') statuses[id] = 'failed';
        }
        break;
      }
      case 'runCompleted': {
        // Terminal states are never entered (no stepEntered fires for a
        // state node), so the reached final state must light from the
        // terminal event itself: mark it succeeded alongside everything
        // still running.
        const finalState = str(p?.finalState) ?? str(p?.final_state) ?? str(p?.state);
        if (finalState) statuses[finalState] = 'succeeded';
        for (const [id, status] of Object.entries(statuses)) {
          if (status === 'running') statuses[id] = 'succeeded';
        }
        break;
      }
      default:
        break;
    }
  }

  return { statuses, forEach };
}

/**
 * Events that belong to a step node, used to filter the event log when a
 * graph node is clicked. Payloads carry the node as `step` (step-shaped
 * events) or `node` (wait/approval/branch/for_each-shaped events); step
 * transitions have no node field and match when either end is the node.
 */
export function eventBelongsToStep(e: EventEnvelope, step: string): boolean {
  const p = payloadOf(e);
  const owner = str(p?.step) ?? str(p?.node);
  if (owner) return owner === step;
  if (e.type === 'stepTransition') {
    return str(p?.from) === step || str(p?.to) === step;
  }
  return false;
}