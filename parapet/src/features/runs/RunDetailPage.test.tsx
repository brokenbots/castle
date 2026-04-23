import { render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { describe, expect, test, vi } from 'vitest';
import { RunDetailPage } from './RunDetailPage';

vi.mock('../../api/castleApi', () => ({
  useGetRunQuery: () => ({
    isLoading: false,
    error: undefined,
    data: {
      ID: 'run-1',
      OverseerID: 'ov-1',
      WorkflowName: 'hello',
      WorkflowHCL:
        'workflow "hello" {\n  start_at = "build"\n  step "build" {\n    transitions = {\n      "success" = "test"\n    }\n  }\n  step "test" {\n    transitions = {\n      "success" = "done"\n    }\n  }\n  state "done" { terminal = true }\n}',
      Status: 'running',
      CurrentStep: 'build',
      LastSeq: 2,
      CreatedAt: new Date().toISOString(),
    },
  }),
  useListEventsQuery: () => ({ data: [] }),
}));

describe('RunDetailPage', () => {
  test('renders workflow source and graph', async () => {
    render(
      <MemoryRouter initialEntries={['/runs/run-1']}>
        <Routes>
          <Route path="/runs/:id" element={<RunDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    expect(await screen.findByText('Step graph')).toBeInTheDocument();
    expect((await screen.findAllByText(/build/)).length).toBeGreaterThan(0);
  });
});
