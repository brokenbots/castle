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

  test('toggles the selection off when the already-selected node is clicked again', async () => {
    const onSelect = vi.fn();
    render(<WorkflowDag graph={graph()} selectedId="test" onSelect={onSelect} />);
    fireEvent.click(screen.getByText('test'));
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith(null));

    // A different node still selects normally.
    fireEvent.click(screen.getByText('build'));
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith('build'));
  });

  test('swaps handle sides with the orientation', async () => {
    // Top-bottom: flow enters at the top and exits at the bottom.
    const first = render(<WorkflowDag graph={graph()} />);
    const tbHandles = Array.from(document.querySelectorAll('.react-flow__handle')).map((el) =>
      el.getAttribute('data-handlepos'),
    );
    expect(tbHandles.filter((pos) => pos === 'top').length).toBeGreaterThan(0);
    expect(tbHandles.filter((pos) => pos === 'bottom').length).toBeGreaterThan(0);
    expect(tbHandles.some((pos) => pos === 'left' || pos === 'right')).toBe(false);
    first.unmount();

    // Left-right: flow enters on the left and exits on the right.
    const second = render(<WorkflowDag graph={graph()} orientation="left-right" />);
    const lrHandles = Array.from(document.querySelectorAll('.react-flow__handle')).map(
      (el) => el.getAttribute('data-handlepos'),
    );
    expect(lrHandles.filter((pos) => pos === 'left').length).toBeGreaterThan(0);
    expect(lrHandles.filter((pos) => pos === 'right').length).toBeGreaterThan(0);
    expect(lrHandles.some((pos) => pos === 'top' || pos === 'bottom')).toBe(false);
    second.unmount();
  });

  test('zooms to the followed step and the reset control restores the full view', async () => {
    const viewport = () => document.querySelector('.react-flow__viewport') as HTMLElement;
    const view = render(<WorkflowDag graph={graph()} />);
    // Let the initial fitView settle (ResizeObserver-driven measurement).
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 100));
    });
    // Establish the full-graph framing via the reset control itself: the
    // initial fitView and the reset use the same computation, so this is
    // the value a later reset must reproduce.
    fireEvent.click(screen.getByTestId('dag-reset-view'));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 350));
    });
    const fullView = viewport().style.transform;
    expect(fullView).not.toBe('');

    // Enabling follow on the live instance re-centers the viewport on the
    // followed node (zoomed in relative to the full-graph framing).
    act(() => {
      view.rerender(<WorkflowDag graph={graph()} followStepId="deploy" />);
    });
    await vi.waitFor(
      () => {
        expect(viewport().style.transform).not.toBe(fullView);
      },
      { timeout: 1500 },
    );

    // The reset control restores the full-graph framing.
    fireEvent.click(screen.getByTestId('dag-reset-view'));
    await vi.waitFor(
      () => {
        expect(viewport().style.transform).toBe(fullView);
      },
      { timeout: 1500 },
    );
    view.unmount();
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

  describe('subworkflow explore affordance (CRI-257)', () => {
    function subworkflowGraph(): WorkflowGraph {
      return graph({
        nodes: [
          { id: 'build', kind: 'step' },
          { id: 'test', kind: 'step', subworkflow: 'qa_triage' },
          { id: 'done', kind: 'state', terminal: true, success: true },
        ],
        edges: [
          { from: 'build', via: 'success', to: 'test' },
          { from: 'test', via: 'success', to: 'done' },
        ],
      });
    }

    test('steps targeting a subworkflow render an explore affordance that opens the layer', async () => {
      const onSelect = vi.fn();
      const onExploreLayer = vi.fn();
      await renderDag(
        <WorkflowDag
          graph={subworkflowGraph()}
          onSelect={onSelect}
          onExploreLayer={onExploreLayer}
          exploreableLayers={new Set(['qa_triage'])}
        />,
      );

      const affordance = screen.getByTestId('dag-node-explore');
      expect(affordance).toBeEnabled();
      expect(affordance).toHaveAttribute('title', 'Open subworkflow qa_triage');

      fireEvent.click(affordance);
      expect(onExploreLayer).toHaveBeenCalledWith('qa_triage');
      // The affordance opens the layer; it must not toggle the node
      // selection underneath.
      expect(onSelect).not.toHaveBeenCalled();
    });

    test('without an exploreable layer the affordance renders disabled (grayed, not hidden)', async () => {
      // A callback alone does not enable the affordance: the layer must
      // also have resolved to a parsed graph.
      await renderDag(
        <WorkflowDag
          graph={subworkflowGraph()}
          onExploreLayer={() => {}}
          exploreableLayers={new Set(['other_layer'])}
        />,
      );

      const affordance = screen.getByTestId('dag-node-explore');
      expect(affordance).toBeDisabled();
      expect(affordance).toHaveAttribute(
        'title',
        'Subworkflow qa_triage graph not available yet',
      );
    });

    test('clicking a disabled affordance does not navigate and keeps selection intact', async () => {
      const onExploreLayer = vi.fn();
      await renderDag(
        <WorkflowDag graph={subworkflowGraph()} onExploreLayer={onExploreLayer} />,
      );

      fireEvent.click(screen.getByTestId('dag-node-explore'));
      expect(onExploreLayer).not.toHaveBeenCalled();
    });

    test('nodes without a subworkflow target render no affordance', async () => {
      await renderDag(<WorkflowDag graph={graph()} onExploreLayer={() => {}} />);
      expect(screen.queryByTestId('dag-node-explore')).not.toBeInTheDocument();
    });
  });
});
