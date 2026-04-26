import { render, screen, fireEvent } from '@testing-library/react';
import { describe, expect, test } from 'vitest';
import { ForEachStrip } from './ForEachStrip';
import type { EventEnvelope } from '../../../api/castleApi';

describe('ForEachStrip', () => {
  test('shows progress strip during iteration', () => {
    const events: EventEnvelope[] = [
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 1,
        type: 'forEachEntered',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', count: 5 },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 2,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 0, value: 'item-0', anyFailed: false },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 3,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 1, value: 'item-1', anyFailed: false },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 4,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 2, value: 'item-2', anyFailed: false },
      },
    ];

    render(<ForEachStrip runId="run-1" events={events} />);

    expect(screen.getByText(/\[●●●○○\]/)).toBeInTheDocument();
    expect(screen.getByText(/3 of 5/)).toBeInTheDocument();
  });

  test('shows summary on completion with all succeeded', () => {
    const events: EventEnvelope[] = [
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 1,
        type: 'forEachEntered',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', count: 3 },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 2,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 0, value: 'item-0', anyFailed: false },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 3,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 1, value: 'item-1', anyFailed: false },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 4,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 2, value: 'item-2', anyFailed: false },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 5,
        type: 'forEachOutcome',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', outcome: 'all_succeeded', target: 'done' },
      },
    ];

    render(<ForEachStrip runId="run-1" events={events} />);

    expect(screen.getByText(/3\/3 succeeded/)).toBeInTheDocument();
  });

  test('expands to show iteration details', () => {
    const events: EventEnvelope[] = [
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 1,
        type: 'forEachEntered',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', count: 2 },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 2,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 0, value: 'item-0', anyFailed: false },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 3,
        type: 'forEachIteration',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', index: 1, value: 'item-1', anyFailed: false },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 4,
        type: 'forEachOutcome',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { node: 'deploy', outcome: 'all_succeeded', target: 'done' },
      },
    ];

    render(<ForEachStrip runId="run-1" events={events} />);

    const button = screen.getByRole('button');
    fireEvent.click(button);

    expect(screen.getByText('item-0')).toBeInTheDocument();
    expect(screen.getByText('item-1')).toBeInTheDocument();
  });
});
