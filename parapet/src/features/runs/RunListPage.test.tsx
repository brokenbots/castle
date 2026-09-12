import { render, screen } from '@testing-library/react';
import { Provider } from 'react-redux';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, test, vi } from 'vitest';
import { RunListPage } from './RunListPage';
import { store } from '../../store';

vi.mock('../../api/castleApi', async () => {
  const actual = await vi.importActual<typeof import('../../api/castleApi')>(
    '../../api/castleApi',
  );
  return {
    ...actual,
    useListRunsQuery: () => ({
      isLoading: false,
      error: undefined,
      data: [
        {
          runId: '11111111-2222-3333-4444-555555555555',
          criteriaId: 'ov-1',
          workflowName: 'hello',
          workflowHash: '',
          status: 'running',
          finalState: '',
          failureReason: '',
          ticket: 'CRI-131',
        },
        {
          runId: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
          criteriaId: 'ov-1',
          workflowName: 'agent-only',
          workflowHash: '',
          status: 'pending',
          finalState: '',
          failureReason: '',
        },
      ],
    }),
  };
});

describe('RunListPage', () => {
  test('renders ticket column with k8s-native ticket identifiers', () => {
    render(
      <Provider store={store}>
        <MemoryRouter>
          <RunListPage />
        </MemoryRouter>
      </Provider>,
    );

    expect(screen.getByRole('columnheader', { name: 'Ticket' })).toBeInTheDocument();
    expect(screen.getByText('CRI-131')).toBeInTheDocument();
    // Agent-initiated runs carry no ticket and leave the cell empty.
    expect(screen.getByText('agent-only')).toBeInTheDocument();
  });
});