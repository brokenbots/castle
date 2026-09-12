import { render, screen } from '@testing-library/react';
import { Provider } from 'react-redux';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { beforeEach, describe, expect, test, vi } from 'vitest';
import { RunDetailPage } from './RunDetailPage';
import { store } from '../../store';

vi.mock('./watchRun', () => ({
  startWatch: vi.fn().mockResolvedValue(undefined),
}));

import { startWatch } from './watchRun';

// Mutable fixture so tests can vary run metadata (CRI-131) without a second
// module mock. UseGetRunQuery returns this object verbatim.
const fixture = vi.hoisted(() => ({
  data: {
    runId: 'run-1',
    criteriaId: 'ov-1',
    workflowName: 'hello',
    workflowHash:
      'workflow "hello" {\n  start_at = "build"\n  step "build" {\n    transitions = {\n      "success" = "test"\n    }\n  }\n  step "test" {\n    transitions = {\n      "success" = "done"\n    }\n  }\n  state "done" { terminal = true }\n}',
    status: 'running',
    createdAt: new Date().toISOString(),
    finalState: '',
    failureReason: '',
    ticket: '',
    repoUrl: '',
    prUrl: '',
  } as Record<string, unknown>,
}));

vi.mock('../../api/castleApi', async () => {
  const actual = await vi.importActual<typeof import('../../api/castleApi')>(
    '../../api/castleApi',
  );
  return {
    ...actual,
    useGetRunQuery: () => ({
      isLoading: false,
      error: undefined,
      data: fixture.data,
    }),
    useListEventsQuery: () => ({ data: [] }),
  };
});

describe('RunDetailPage', () => {
  beforeEach(() => {
    fixture.data.ticket = '';
    fixture.data.repoUrl = '';
    fixture.data.prUrl = '';
  });

  test('starts WatchRun with sinceSeq=0 and subscriberId', async () => {
    const randomUUID = vi
      .spyOn(crypto, 'randomUUID')
      .mockReturnValue('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa');

    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    expect(startWatch).toHaveBeenCalled();

    const firstCall = vi.mocked(startWatch).mock.calls[0];
    expect(firstCall[0]).toBe('run-1');
    expect(firstCall[1]).toBe(0);
    expect(firstCall[2]).toBe('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa');

    randomUUID.mockRestore();
  });

  test('renders workflow source and graph', async () => {
    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    expect(await screen.findByText('Step graph')).toBeInTheDocument();
    expect((await screen.findAllByText(/build/)).length).toBeGreaterThan(0);
  });

  test('renders ticket, repo and PR link for k8s-native runs', async () => {
    fixture.data.ticket = 'CRI-131';
    fixture.data.repoUrl = 'brokenbots/castle';
    fixture.data.prUrl = 'https://github.com/brokenbots/castle/pull/42';

    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('CRI-131')).toBeInTheDocument();
    expect(await screen.findByText('brokenbots/castle')).toBeInTheDocument();
    const link = screen.getByRole('link', { name: 'PR' });
    expect(link.getAttribute('href')).toBe('https://github.com/brokenbots/castle/pull/42');
    expect(link.getAttribute('rel')).toBe('noreferrer');
  });

  test('does not render PR link for non-http prUrl values', async () => {
    fixture.data.ticket = 'CRI-131';
    fixture.data.prUrl = 'javascript:alert(1)';

    render(
      <Provider store={store}>
        <MemoryRouter initialEntries={['/runs/run-1']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/runs/:id" element={<RunDetailPage />} />
          </Routes>
        </MemoryRouter>
      </Provider>,
    );

    expect(await screen.findByText('CRI-131')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'PR' })).not.toBeInTheDocument();
  });

  test('supports both proto and json codec selection', async () => {
    const { getRuntimeCodec } = await import('../../api/client');

    // Test default codec (json)
    expect(getRuntimeCodec()).toBe('json');

    // Test window.__CRITERIA__.codec override (proto)
    window.__CRITERIA__ = { codec: 'proto' };
    expect(getRuntimeCodec()).toBe('proto');

    // Test meta tag fallback (json)
    window.__CRITERIA__ = undefined;
    const meta = document.createElement('meta');
    meta.name = 'criteria-codec';
    meta.content = 'json';
    document.head.appendChild(meta);
    expect(getRuntimeCodec()).toBe('json');

    // Cleanup
    document.head.removeChild(meta);
  });
});
