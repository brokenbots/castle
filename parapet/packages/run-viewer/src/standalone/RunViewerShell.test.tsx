import { render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { RunViewerShell } from './RunViewerShell';
import { createRunViewerStore } from '../store';
import { resetRunDataSource, setRunDataSource } from '../api/dataSource';
import { NO_CONTROLS_TOOLTIP } from '../features/runs/capabilities';
import type { RunDataSource } from '../api/dataSource';
import type { Agent, Run, RunInspection, EventEnvelope } from '../api/castleApi';

// The shell must run against the seam, not the castle Connect client: the
// fake below stands in for the CRI-255 loopback the standalone data source
// talks to, and the assertions hold only if the shell resolves through
// setRunDataSource.
const RUN: Run = {
  runId: 'run-1',
  criteriaId: 'crn:v1:criteria:workflow/demo',
  workflowName: 'demo',
  workflowHash: 'workflow {\n  name = "demo"\n}\nstep "build" {\n}',
  status: 'running',
  createdAt: '2026-02-05T08:30:00.000Z',
  finalState: '',
  failureReason: '',
  ticket: '',
  repoUrl: '',
  prUrl: '',
};

const AGENT: Agent = {
  criteriaId: 'crn:v1:criteria:workflow/demo',
  name: 'demo',
  labels: {},
  status: 'online',
};

const INSPECTION: RunInspection = {
  runId: 'run-1',
  sessionId: 'sess-1',
  adapter: 'linear',
  currentStep: 'build',
  pendingPermissions: 0,
  stateJson: '{}',
};

// Type-only alias keeps the never-resolving openRunStream promise honest.
type RunStreamEnd = Awaited<ReturnType<RunDataSource['openRunStream']>>;

function fakeDataSource(): RunDataSource {
  return {
    listRuns: vi.fn(async () => ({ runs: [RUN], nextPageToken: '' })),
    getRun: vi.fn(async () => RUN),
    inspectRun: vi.fn(async () => INSPECTION),
    listRunEvents: vi.fn(async () => ({ events: [], lastSeq: 0, nextSinceSeq: null })),
    listAgents: vi.fn(async () => [AGENT]),
    getAgent: vi.fn(async () => AGENT),
    connectionStatus: vi.fn(async () => undefined),
    resume: vi.fn(async () => ({})),
    pauseRun: vi.fn(async () => ({})),
    stopRun: vi.fn(async () => ({})),
    // The stream stays open for the whole test; watchRun keeps the
    // reconnect UI in its connecting/live state without noise.
    openRunStream: vi.fn(
      (_args, onEvent) =>
        new Promise<RunStreamEnd>(() => {
          onEvent({
            schemaVersion: 1,
            runId: 'run-1',
            seq: 0,
            type: 'watchReady',
            correlationId: '',
            payload: null,
          } satisfies EventEnvelope);
        }),
    ),
  };
}

describe('RunViewerShell', () => {
  let dataSource: RunDataSource;

  beforeEach(() => {
    dataSource = fakeDataSource();
    setRunDataSource(dataSource);
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa');
  });

  afterEach(() => {
    resetRunDataSource();
    vi.restoreAllMocks();
    window.location.hash = '';
  });

  test('serves the run list at the shell root', async () => {
    window.location.hash = '';
    render(<RunViewerShell store={createRunViewerStore()} />);

    const link = await screen.findByRole('link', { name: 'run-1' });
    expect(link.getAttribute('href')).toBe('#/runs/run-1');
    expect(dataSource.listRuns).toHaveBeenCalled();
  });

  test('serves the run detail screen from the local data source', async () => {
    window.location.hash = '#/runs/run-1';
    render(<RunViewerShell store={createRunViewerStore()} />);

    expect(await screen.findByTestId('run-detail-layout')).toBeInTheDocument();
    expect(await screen.findByText('Workflow source')).toBeInTheDocument();
    expect(dataSource.getRun).toHaveBeenCalledWith('run-1');
  });

  test('grays out the control matrix when the host has no control RPC', async () => {
    window.location.hash = '#/runs/run-1';
    render(<RunViewerShell store={createRunViewerStore()} />);

    const layout = await screen.findByTestId('run-detail-layout');
    // Capability probe: the standalone host renders every control disabled
    // with the shared tooltip — grayed-out is the contract, not hidden.
    await waitFor(() => {
      const buttons = [/Paus/, 'Resume', 'Stop'].map((name) =>
        within(layout).getByRole('button', { name }),
      );
      for (const button of buttons) {
        expect(button).toBeDisabled();
        expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
      }
    });
  });

  test('grays out the approval actions of a paused run when the host has no control RPC', async () => {
    window.location.hash = '#/runs/run-1';
    // A paused run awaiting approval is reachable in standalone mode: the
    // loopback data source replays approvalRequested events from the
    // run-state files. The approval actions must gray out with the rest of
    // the control matrix, not render live.
    setRunDataSource({
      ...dataSource,
      openRunStream: vi.fn((_args, onEvent) => {
        onEvent({
          schemaVersion: 1,
          runId: 'run-1',
          seq: 1,
          type: 'approvalRequested',
          ts: '2026-02-05T08:31:00.000Z',
          correlationId: '',
          payload: { node: 'deploy', approvers: ['alice'], reason: 'ship it' },
        } satisfies EventEnvelope);
        return new Promise<RunStreamEnd>(() => {});
      }),
    });
    render(<RunViewerShell store={createRunViewerStore()} />);

    const layout = await screen.findByTestId('run-detail-layout');
    await waitFor(() => {
      expect(within(layout).getByText(/Approval Required/i)).toBeInTheDocument();
    });
    for (const name of [/Approve/i, /Reject/i]) {
      const button = within(layout).getByRole('button', { name });
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
    }
  });

  test('redirects unknown hashes to the run list', async () => {
    window.location.hash = '#/bogus';
    render(<RunViewerShell store={createRunViewerStore()} />);

    expect(await screen.findByRole('link', { name: 'run-1' })).toBeInTheDocument();
    expect(window.location.hash).toBe('#/');
  });
});
