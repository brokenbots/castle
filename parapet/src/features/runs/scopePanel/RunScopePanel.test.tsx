import { render, screen } from '@testing-library/react';
import { describe, expect, test } from 'vitest';
import { RunScopePanel } from './RunScopePanel';
import type { EventEnvelope } from '../../../api/castleApi';

// RunScopePanel is docked panel content: it renders directly from the
// event stream with no open/close affordance of its own (the dock lives in
// RunDetailPage).
describe('RunScopePanel', () => {
  test('renders its content inside the docked scope panel', () => {
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
    ];

    render(<RunScopePanel events={events} />);

    const panel = screen.getByTestId('run-scope-panel');
    expect(panel).toBeInTheDocument();
    expect(panel).not.toHaveClass('fixed');
    expect(screen.getByText('var.env')).toBeInTheDocument();
    expect(screen.getByText('prod')).toBeInTheDocument();
    expect(screen.getByText('default')).toBeInTheDocument();
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

    expect(screen.getByText('steps.build')).toBeInTheDocument();
    expect(screen.getByText('artifact:')).toBeInTheDocument();
    expect(screen.getByText('app-v1.0.tar.gz')).toBeInTheDocument();
    expect(screen.getByText('sha256:')).toBeInTheDocument();
    expect(screen.getByText('abc123')).toBeInTheDocument();
  });

  test('shows empty state when no scope events', () => {
    render(<RunScopePanel events={[]} />);

    expect(screen.getByText(/No variables set/i)).toBeInTheDocument();
    expect(screen.getByText(/No step outputs captured/i)).toBeInTheDocument();
  });
});