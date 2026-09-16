import { describe, expect, test } from 'vitest';
import type { EventEnvelope } from '../../../api/castleApi';
import { eventBelongsToStep, selectNodeOverlay } from './nodeStatus';

function event(seq: number, type: string, payload: Record<string, unknown> = {}): EventEnvelope {
  return { schemaVersion: 1, runId: 'run-1', seq, type, ts: new Date(0).toISOString(), correlationId: '', payload };
}

describe('selectNodeOverlay', () => {
  test('marks the entered step running and completed steps by outcome', () => {
    const { statuses } = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'stepOutcome', { step: 'build', outcome: 'success' }),
      event(3, 'stepEntered', { step: 'test' }),
    ]);
    expect(statuses.build).toBe('succeeded');
    expect(statuses.test).toBe('running');
  });

  test('marks failed steps from error outcomes and error payloads', () => {
    const { statuses } = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'stepOutcome', { step: 'build', outcome: 'error', error: 'boom' }),
    ]);
    expect(statuses.build).toBe('failed');

    const other = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'stepOutcome', { step: 'build', outcome: 'success', error: 'recovered with error text' }),
    ]);
    expect(other.statuses.build).toBe('failed');
  });

  test('re-entry of a completed step flips it back to running', () => {
    const { statuses } = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'stepOutcome', { step: 'build', outcome: 'success' }),
      event(3, 'stepEntered', { step: 'build' }),
    ]);
    expect(statuses.build).toBe('running');
  });

  test('falls back to transitions when no outcome event arrives', () => {
    const { statuses } = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'stepTransition', { from: 'build', to: 'test', via_outcome: 'success' }),
    ]);
    expect(statuses.build).toBe('succeeded');
  });

  test('does not downgrade a failed step when a transition follows', () => {
    const { statuses } = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'stepOutcome', { step: 'build', outcome: 'error', error: 'boom' }),
      event(3, 'stepTransition', { from: 'build', to: 'test' }),
    ]);
    expect(statuses.build).toBe('failed');
  });

  test('tracks for_each progress from ForEachStrip events', () => {
    const { statuses, forEach } = selectNodeOverlay([
      event(1, 'forEachEntered', { node: 'deploy', count: 3 }),
      event(2, 'stepIterationStarted', { node: 'deploy', index: 0, value: 'api' }),
      event(3, 'stepIterationStarted', { node: 'deploy', index: 1, value: 'web' }),
    ]);
    expect(statuses.deploy).toBe('running');
    expect(forEach.deploy).toEqual({ total: 3, started: 2, outcome: null, anyFailed: false });
  });

  test('completes a for_each node by its aggregate outcome', () => {
    const succeeded = selectNodeOverlay([
      event(1, 'forEachEntered', { node: 'deploy', count: 2 }),
      event(2, 'stepIterationStarted', { node: 'deploy', index: 0, value: 'api' }),
      event(3, 'stepIterationCompleted', { node: 'deploy', outcome: 'all_succeeded' }),
    ]);
    expect(succeeded.statuses.deploy).toBe('succeeded');
    expect(succeeded.forEach.deploy.outcome).toBe('all_succeeded');

    const failed = selectNodeOverlay([
      event(1, 'forEachEntered', { node: 'deploy', count: 2 }),
      event(2, 'stepIterationStarted', { node: 'deploy', index: 0, value: 'api', anyFailed: true }),
      event(3, 'stepIterationCompleted', { node: 'deploy', outcome: 'any_failed' }),
    ]);
    expect(failed.statuses.deploy).toBe('failed');
    expect(failed.forEach.deploy).toEqual({ total: 2, started: 1, outcome: 'any_failed', anyFailed: true });
  });

  test('maps branch, wait and approval events', () => {
    const { statuses } = selectNodeOverlay([
      event(1, 'waitEntered', { node: 'hold' }),
      event(2, 'waitResumed', { node: 'hold', mode: 'duration' }),
      event(3, 'approvalRequested', { node: 'gate' }),
      event(4, 'approvalDecision', { node: 'gate', decision: 'approve' }),
      event(5, 'branchEvaluated', { node: 'check', matchedArm: 'arm[0]', target: 'next' }),
    ]);
    expect(statuses.hold).toBe('succeeded');
    expect(statuses.gate).toBe('succeeded');
    expect(statuses.check).toBe('succeeded');
  });

  test('fails running nodes on runFailed and succeeds them on runCompleted', () => {
    const failed = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'runFailed', { reason: 'step crashed' }),
    ]);
    expect(failed.statuses.build).toBe('failed');

    const named = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'stepEntered', { step: 'test' }),
      event(3, 'runFailed', { reason: 'x', step: 'test' }),
    ]);
    expect(named.statuses.test).toBe('failed');
    expect(named.statuses.build).toBe('running');

    const completed = selectNodeOverlay([
      event(1, 'stepEntered', { step: 'build' }),
      event(2, 'runCompleted', {}),
    ]);
    expect(completed.statuses.build).toBe('succeeded');
  });

  test('returns no state for unvisited steps', () => {
    const { statuses, forEach } = selectNodeOverlay([event(1, 'stepEntered', { step: 'build' })]);
    expect(statuses).toEqual({ build: 'running' });
    expect(forEach).toEqual({});
  });
});

describe('eventBelongsToStep', () => {
  test('matches step- and node-shaped payloads and transitions', () => {
    expect(eventBelongsToStep(event(1, 'stepEntered', { step: 'build' }), 'build')).toBe(true);
    expect(eventBelongsToStep(event(2, 'forEachEntered', { node: 'deploy' }), 'deploy')).toBe(true);
    expect(eventBelongsToStep(event(3, 'stepTransition', { from: 'a', to: 'b' }), 'a')).toBe(true);
    expect(eventBelongsToStep(event(4, 'stepTransition', { from: 'a', to: 'b' }), 'b')).toBe(true);
    expect(eventBelongsToStep(event(5, 'stepTransition', { from: 'a', to: 'b' }), 'c')).toBe(false);
    expect(eventBelongsToStep(event(6, 'stepEntered', { step: 'other' }), 'build')).toBe(false);
  });
});