import { beforeAll, describe, expect, test, vi } from 'vitest';
import { act, fireEvent, render, screen } from '@testing-library/react';
import tourSource from './fixtures/tour.chcl?raw';
import { parseWorkflowHcl, type WorkflowGraph, type WorkflowGraphEdge, type WorkflowGraphNode } from './parseWorkflowHcl';
import { WorkflowDag } from './WorkflowDag';
import { selectNodeOverlay } from './nodeStatus';

beforeAll(() => {
  // React Flow measures its container; jsdom has no layout.
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(800);
  vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(800);
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(
    () =>
      ({
        x: 0, y: 0, top: 0, left: 0, right: 800, bottom: 600, width: 800, height: 600, toJSON: () => ({}),
      }) as DOMRect,
  );
});

function graph(partial?: Partial<WorkflowGraph>): WorkflowGraph {
  const nodes: WorkflowGraphNode[] = [
    { id: 'build', kind: 'step' },
    { id: 'test', kind: 'step' },
    { id: 'deploy', kind: 'step', iteration: { control: 'for_each', items: '["api", "web"]' } },
    { id: 'decide', kind: 'switch', arms: [{ condition: 'var.ok', target: 'done' }, { condition: '', target: 'done' }] },
    { id: 'done', kind: 'state', terminal: true, success: true },
  ];
  const edges: WorkflowGraphEdge[] = [
    { from: 'build', via: 'success', to: 'test' },
    { from: 'test', via: 'all_succeeded', to: 'deploy' },
    { from: 'deploy', via: 'arm[0]', to: 'decide' },
    { from: 'decide', via: 'default', to: 'done' },
  ];
  return { name: 'demo', startAt: 'build', nodes: partial?.nodes ?? nodes, edges: partial?.edges ?? edges };
}

async function renderDag(element: React.ReactElement): Promise<void> {
  render(element);
  // React Flow renders edges in passes driven by ResizeObserver callbacks
  // (container measure → node measure → edges); flush them inside act.
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 50));
  });
}

describe('WorkflowDag', () => {
  test('renders one node per graph node and labeled edges per transition', async () => {
    await renderDag(<WorkflowDag graph={graph()} />);
    const nodes = screen.getAllByTestId('dag-node');
    expect(nodes).toHaveLength(5);
    expect(nodes.map((n) => n.getAttribute('data-node-id'))).toContain('build');
    expect(screen.getByText('build')).toBeInTheDocument();
    // The iterating step and the switch render their kind labels.
    expect(screen.getByText('for_each · ["api", "web"]')).toBeInTheDocument();
    expect(screen.getByText('switch')).toBeInTheDocument();
    // Each outcome/arm edge renders one SVG text label.
    const edgeLabels = Array.from(document.querySelectorAll('.react-flow__edge-text')).map(
      (el) => el.textContent,
    );
    expect(edgeLabels).toEqual(['success', 'all_succeeded', 'arm[0]', 'default']);
  });

  test('renders the parsed tour fixture graph end to end', async () => {
    await renderDag(<WorkflowDag graph={parseWorkflowHcl(tourSource)} />);
    const nodes = screen.getAllByTestId('dag-node');
    expect(nodes).toHaveLength(8);
    expect(screen.getByText('for_each · ["alpha", "beta", "gamma"]')).toBeInTheDocument();
    // Long items expressions are truncated in the badge.
    expect(screen.getByText('parallel · ["auth", "catalog", "billin…')).toBeInTheDocument();
    expect(screen.getByText('wait')).toBeInTheDocument();
    const edgeLabels = Array.from(document.querySelectorAll('.react-flow__edge-text')).map(
      (el) => el.textContent,
    );
    expect(edgeLabels).toContain('elapsed');
    expect(edgeLabels).toContain('arm[0]');
    expect(edgeLabels).toContain('default');
  });

  test('shows live status marks and pulse on the running node', async () => {
    await renderDag(
      <WorkflowDag
        graph={graph()}
        statuses={{ build: 'running', test: 'succeeded' }}
      />,
    );
    const buildCard = screen.getByText('build').closest('[data-testid="dag-node"]');
    expect(buildCard?.className).toContain('animate-pulse');
    expect(screen.getByLabelText('status running')).toBeInTheDocument();
    expect(screen.getByLabelText('status succeeded')).toBeInTheDocument();
  });

  test('dims unvisited nodes and highlights failed ones', async () => {
    await renderDag(
      <WorkflowDag
        graph={graph()}
        statuses={{ build: 'failed' }}
      />,
    );
    const failedCard = screen.getByText('build').closest('[data-testid="dag-node"]');
    expect(failedCard?.className).toContain('border-rose-500');
    const idleCard = screen.getByText('done').closest('[data-testid="dag-node"]');
    expect(idleCard?.className).toContain('opacity-60');
  });

  test('prefers live iteration progress over the declared control badge', async () => {
    await renderDag(
      <WorkflowDag
        graph={graph()}
        statuses={{ deploy: 'running' }}
        forEachProgress={{ deploy: { total: 3, started: 2, outcome: null, anyFailed: false } }}
      />,
    );
    expect(screen.getByText('2/3')).toBeInTheDocument();
    expect(screen.queryByText('for_each · ["api", "web"]')).not.toBeInTheDocument();
  });

  test('shows the aggregate outcome once the loop completes', async () => {
    await renderDag(
      <WorkflowDag
        graph={graph()}
        statuses={{ deploy: 'succeeded' }}
        forEachProgress={{ deploy: { total: 3, started: 3, outcome: 'all_succeeded', anyFailed: false } }}
      />,
    );
    expect(screen.getByText('all_succeeded (3)')).toBeInTheDocument();
  });

  test('calls onSelect with the clicked node id', async () => {
    const onSelect = vi.fn();
    render(<WorkflowDag graph={graph()} onSelect={onSelect} />);
    const node = screen.getByText('test');
    fireEvent.click(node);
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith('test'));
  });

  test('derives overlay state through selectNodeOverlay', async () => {
    const overlay = selectNodeOverlay([
      { schemaVersion: 1, runId: 'r', seq: 1, type: 'stepEntered', ts: '', correlationId: '', payload: { step: 'build' } },
    ]);
    await renderDag(<WorkflowDag graph={graph()} statuses={overlay.statuses} />);
    expect(screen.getByLabelText('status running')).toBeInTheDocument();
    // Unvisited nodes stay dimmed/idle.
    expect(screen.getAllByLabelText('status idle')).toHaveLength(4);
  });
});
