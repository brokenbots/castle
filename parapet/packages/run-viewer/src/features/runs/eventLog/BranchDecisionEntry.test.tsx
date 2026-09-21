import { render, screen, fireEvent } from '@testing-library/react';
import { describe, expect, test } from 'vitest';
import { BranchDecisionEntry } from './BranchDecisionEntry';
import type { EventEnvelope } from '../../../api/castleApi';

describe('BranchDecisionEntry', () => {
  const mockEvent: EventEnvelope = {
    schemaVersion: 1,
    runId: 'run-1',
    seq: 10,
    type: 'branchEvaluated',
    ts: new Date().toISOString(),
    correlationId: '',
    payload: {
      node: 'check',
      matchedArm: 'arm[0]',
      target: 'deploy',
      condition: 'var.env == "prod"',
    },
  };

  test('renders collapsed state by default', () => {
    render(<BranchDecisionEntry event={mockEvent} />);

    expect(screen.getByText('check')).toBeInTheDocument();
    expect(screen.getByText('arm[0]')).toBeInTheDocument();
    expect(screen.getByText('deploy')).toBeInTheDocument();
    expect(screen.getByText(/▶/)).toBeInTheDocument();
  });

  test('expands on click to show details', () => {
    render(<BranchDecisionEntry event={mockEvent} />);

    const button = screen.getByRole('button');
    fireEvent.click(button);

    expect(screen.getByText(/▼/)).toBeInTheDocument();
    expect(screen.getByText(/Decision details:/i)).toBeInTheDocument();
    expect(screen.getByText(/Matched Arm:/)).toBeInTheDocument();
  });

  test('displays condition when present', () => {
    render(<BranchDecisionEntry event={mockEvent} />);

    expect(screen.getByText(/var\.env == "prod"/)).toBeInTheDocument();
  });

  test('handles missing condition gracefully', () => {
    const eventNoCondition: EventEnvelope = {
      ...mockEvent,
      payload: {
        node: 'check',
        matchedArm: 'default',
        target: 'fallback',
        condition: '',
      },
    };

    render(<BranchDecisionEntry event={eventNoCondition} />);

    expect(screen.getByText('check')).toBeInTheDocument();
    expect(screen.getByText('default')).toBeInTheDocument();
  });
});
