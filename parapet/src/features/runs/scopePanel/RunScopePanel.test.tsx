import { render, screen, fireEvent } from '@testing-library/react';
import { describe, expect, test } from 'vitest';
import { RunScopePanel } from './RunScopePanel';
import type { EventEnvelope } from '../../../api/castleApi';

describe('RunScopePanel', () => {
  test('reflects VariableSet events', () => {
    const events: EventEnvelope[] = [
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 1,
        type: 'variableSet',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { name: 'env', value: 'prod', source: 'default' },
      },
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 2,
        type: 'variableSet',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { name: 'region', value: 'us-west-2', source: 'step_output:configure' },
      },
    ];

    render(<RunScopePanel events={events} />);

    // Open the panel
    const button = screen.getByRole('button', { name: /Scope/i });
    fireEvent.click(button);

    expect(screen.getByText('var.env')).toBeInTheDocument();
    expect(screen.getByText('prod')).toBeInTheDocument();
    expect(screen.getByText('default')).toBeInTheDocument();

    expect(screen.getByText('var.region')).toBeInTheDocument();
    expect(screen.getByText('us-west-2')).toBeInTheDocument();
    expect(screen.getByText('step_output:configure')).toBeInTheDocument();
  });

  test('reflects StepOutputCaptured events', () => {
    const events: EventEnvelope[] = [
      {
        schemaVersion: 1,
        runId: 'run-1',
        seq: 1,
        type: 'stepOutputCaptured',
        ts: new Date().toISOString(),
        correlationId: '',
        payload: { step: 'build', outputs: { artifact: 'app-v1.0.tar.gz', sha256: 'abc123' } },
      },
    ];

    render(<RunScopePanel events={events} />);

    // Open the panel
    const button = screen.getByRole('button', { name: /Scope/i });
    fireEvent.click(button);

    expect(screen.getByText('steps.build')).toBeInTheDocument();
    expect(screen.getByText('artifact:')).toBeInTheDocument();
    expect(screen.getByText('app-v1.0.tar.gz')).toBeInTheDocument();
    expect(screen.getByText('sha256:')).toBeInTheDocument();
    expect(screen.getByText('abc123')).toBeInTheDocument();
  });

  test('shows empty state when no scope events', () => {
    const events: EventEnvelope[] = [];

    render(<RunScopePanel events={events} />);

    // Open the panel
    const button = screen.getByRole('button', { name: /Scope/i });
    fireEvent.click(button);

    expect(screen.getByText(/No variables set/i)).toBeInTheDocument();
    expect(screen.getByText(/No step outputs captured/i)).toBeInTheDocument();
  });
});
