import { beforeAll, describe, expect, test, vi } from 'vitest';
import { act, fireEvent, render, screen } from '@testing-library/react';
import tourSource from './fixtures/tour.chcl?raw';
import { parseWorkflowHcl, type WorkflowGraph as WorkflowGraphSpec, type WorkflowGraphEdge, type WorkflowGraphNode } from './parseWorkflowHcl';
import { WorkflowGraph } from './WorkflowGraph';
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

function graph(partial?: Partial<WorkflowGraphSpec>): WorkflowGraphSpec {
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

async function renderGraph(element: React.ReactElement): Promise<void> {
  render(element);
  // React Flow renders edges in passes driven by ResizeObserver callbacks
  // (container measure → node measure → edges); flush them inside act.
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 50));
  });
}

describe('WorkflowGraph', () => {
  test('renders one node per graph node and labeled edges per transition', async () => {
    await renderGraph(<WorkflowGraph graph={graph()} />);
    const nodes = screen.getAllByTestId('graph-node');
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
    await renderGraph(<WorkflowGraph graph={parseWorkflowHcl(tourSource)} />);
    const nodes = screen.getAllByTestId('graph-node');
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
    await renderGraph(
      <WorkflowGraph
        graph={graph()}
        statuses={{ build: 'running', test: 'succeeded' }}
      />,
    );
    const buildCard = screen.getByText('build').closest('[data-testid="graph-node"]');
    expect(buildCard?.className).toContain('animate-pulse');
    expect(screen.getByLabelText('status running')).toBeInTheDocument();
    expect(screen.getByLabelText('status succeeded')).toBeInTheDocument();
  });

  test('dims unvisited nodes and highlights failed ones', async () => {
    await renderGraph(
      <WorkflowGraph
        graph={graph()}
        statuses={{ build: 'failed' }}
      />,
    );
    const failedCard = screen.getByText('build').closest('[data-testid="graph-node"]');
    expect(failedCard?.className).toContain('border-rose-500');
    const idleCard = screen.getByText('done').closest('[data-testid="graph-node"]');
    expect(idleCard?.className).toContain('opacity-60');
  });

  test('prefers live iteration progress over the declared control badge', async () => {
    await renderGraph(
      <WorkflowGraph
        graph={graph()}
        statuses={{ deploy: 'running' }}
        forEachProgress={{ deploy: { total: 3, started: 2, outcome: null, anyFailed: false } }}
      />,
    );
    expect(screen.getByText('2/3')).toBeInTheDocument();
    expect(screen.queryByText('for_each · ["api", "web"]')).not.toBeInTheDocument();
  });

  test('shows the aggregate outcome once the loop completes', async () => {
    await renderGraph(
      <WorkflowGraph
        graph={graph()}
        statuses={{ deploy: 'succeeded' }}
        forEachProgress={{ deploy: { total: 3, started: 3, outcome: 'all_succeeded', anyFailed: false } }}
      />,
    );
    expect(screen.getByText('all_succeeded (3)')).toBeInTheDocument();
  });

  test('calls onSelect with the clicked node id', async () => {
    const onSelect = vi.fn();
    render(<WorkflowGraph graph={graph()} onSelect={onSelect} />);
    const node = screen.getByText('test');
    fireEvent.click(node);
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith('test'));
  });

  test('toggles the selection off when the already-selected node is clicked again', async () => {
    const onSelect = vi.fn();
    render(<WorkflowGraph graph={graph()} selectedId="test" onSelect={onSelect} />);
    fireEvent.click(screen.getByText('test'));
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith(null));

    // A different node still selects normally.
    fireEvent.click(screen.getByText('build'));
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith('build'));
  });

  test('swaps handle sides with the orientation', async () => {
    // Top-bottom: flow enters at the top and exits at the bottom.
    const first = render(<WorkflowGraph graph={graph()} />);
    const tbHandles = Array.from(document.querySelectorAll('.react-flow__handle')).map((el) =>
      el.getAttribute('data-handlepos'),
    );
    expect(tbHandles.filter((pos) => pos === 'top').length).toBeGreaterThan(0);
    expect(tbHandles.filter((pos) => pos === 'bottom').length).toBeGreaterThan(0);
    expect(tbHandles.some((pos) => pos === 'left' || pos === 'right')).toBe(false);
    first.unmount();

    // Left-right: flow enters on the left and exits on the right.
    const second = render(<WorkflowGraph graph={graph()} orientation="left-right" />);
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
    const view = render(<WorkflowGraph graph={graph()} />);
    // Let the initial fitView settle (ResizeObserver-driven measurement).
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 100));
    });
    // Establish the full-graph framing via the reset control itself: the
    // initial fitView and the reset use the same computation, so this is
    // the value a later reset must reproduce.
    fireEvent.click(screen.getByTestId('graph-reset-view'));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 350));
    });
    const fullView = viewport().style.transform;
    expect(fullView).not.toBe('');

    // Enabling follow on the live instance re-centers the viewport on the
    // followed node (zoomed in relative to the full-graph framing).
    act(() => {
      view.rerender(<WorkflowGraph graph={graph()} followStepId="deploy" />);
    });
    await vi.waitFor(
      () => {
        expect(viewport().style.transform).not.toBe(fullView);
      },
      { timeout: 1500 },
    );

    // The reset control restores the full-graph framing.
    fireEvent.click(screen.getByTestId('graph-reset-view'));
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
    await renderGraph(<WorkflowGraph graph={graph()} statuses={overlay.statuses} />);
    expect(screen.getByLabelText('status running')).toBeInTheDocument();
    // Unvisited nodes stay dimmed/idle.
    expect(screen.getAllByLabelText('status idle')).toHaveLength(4);
  });

  describe('cyclic graph rendering', () => {
    // A review-shaped loop: build -> review -> build, plus an acyclic
    // tail.
    function loopGraph(): WorkflowGraphSpec {
      return graph({
        nodes: [
          { id: 'build', kind: 'step' },
          { id: 'review', kind: 'step' },
          { id: 'done', kind: 'state', terminal: true, success: true },
        ],
        edges: [
          { from: 'build', via: 'success', to: 'review' },
          { from: 'review', via: 'revise', to: 'build' },
          { from: 'review', via: 'approve', to: 'done' },
        ],
      });
    }

    test('renders a loop badge on the source of the upward cycle leg', async () => {
      await renderGraph(<WorkflowGraph graph={loopGraph()} />);
      const badge = screen.getByTestId('graph-node-loop-badge');
      // The badge is collapsed onto review — the node whose revise edge
      // sweeps back up to build — and names the target.
      const holder = badge.closest('[data-node-id="review"]');
      expect(holder).not.toBeNull();
      expect(badge).toHaveTextContent('↺ loops back to build');
    });

    test('renders cycle legs with the loop edge class and upward legs dashed', async () => {
      await renderGraph(<WorkflowGraph graph={loopGraph()} />);
      const loopEdges = document.querySelectorAll('.react-flow__edge.workflow-edge-loop');
      expect(loopEdges).toHaveLength(2);
      // Both legs are violet; the upward leg (review -> build, e1) is
      // further de-emphasized (lower opacity + dashed), the forward leg
      // (build -> review, e0) stays solid. React Flow applies the edge
      // style prop inline.
      for (const edge of loopEdges) {
        const path = edge.querySelector('path.react-flow__edge-path') as SVGPathElement | null;
        expect(path).not.toBeNull();
        expect(path!.style.stroke).toBe('#a78bfa');
      }
      const upPath = loopEdges[1].querySelector('path.react-flow__edge-path') as SVGPathElement;
      const forwardPath = loopEdges[0].querySelector('path.react-flow__edge-path') as SVGPathElement;
      expect(upPath.style.opacity).toBe('0.7');
      expect(forwardPath.style.opacity).toBe('0.9');
      expect(upPath.style.strokeDasharray).toBeTruthy();
      expect(forwardPath.style.strokeDasharray).toBe('');
    });

    test('renders non-cycle upward returns dimmed and dashed, without loop styling', async () => {
      // build layers test and failed onto layer 1, so test -> failed
      // sweeps at/behind its source layer — but failed never reaches
      // test: a back edge, not a loop.
      const returns = graph({
        nodes: [
          { id: 'build', kind: 'step' },
          { id: 'test', kind: 'step' },
          { id: 'failed', kind: 'state' },
        ],
        edges: [
          { from: 'build', via: 'success', to: 'test' },
          { from: 'build', via: 'failure', to: 'failed' },
          { from: 'test', via: 'failure', to: 'failed' },
        ],
      });
      await renderGraph(<WorkflowGraph graph={returns} />);
      const backEdge = document.querySelector('.react-flow__edge.workflow-edge-back');
      expect(backEdge).not.toBeNull();
      const path = backEdge!.querySelector('path.react-flow__edge-path') as SVGPathElement;
      expect(path.style.stroke).toBe('#64748b');
      expect(path.style.opacity).toBe('0.55');
      expect(path.style.strokeDasharray).toBeTruthy();
      // No loop badge: the graph is acyclic.
      expect(screen.queryByTestId('graph-node-loop-badge')).not.toBeInTheDocument();
    });

    test('exposes the loop to screen readers via aria-label on badge edges', async () => {
      await renderGraph(<WorkflowGraph graph={loopGraph()} />);
      const loopEdges = Array.from(document.querySelectorAll('.react-flow__edge.workflow-edge-loop'));
      const labeled = loopEdges.filter((edge) => edge.getAttribute('aria-label') === 'review loops back to build');
      expect(labeled).toHaveLength(1);
    });
  });

  describe('subworkflow explore affordance (CRI-257)', () => {
    function subworkflowGraph(): WorkflowGraphSpec {
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
      await renderGraph(
        <WorkflowGraph
          graph={subworkflowGraph()}
          onSelect={onSelect}
          onExploreLayer={onExploreLayer}
          exploreableLayers={new Set(['qa_triage'])}
        />,
      );

      const affordance = screen.getByTestId('graph-node-explore');
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
      await renderGraph(
        <WorkflowGraph
          graph={subworkflowGraph()}
          onExploreLayer={() => {}}
          exploreableLayers={new Set(['other_layer'])}
        />,
      );

      const affordance = screen.getByTestId('graph-node-explore');
      expect(affordance).toBeDisabled();
      expect(affordance).toHaveAttribute(
        'title',
        'Subworkflow qa_triage graph not available yet',
      );
    });

    test('clicking a disabled affordance does not navigate and keeps selection intact', async () => {
      const onExploreLayer = vi.fn();
      await renderGraph(
        <WorkflowGraph graph={subworkflowGraph()} onExploreLayer={onExploreLayer} />,
      );

      fireEvent.click(screen.getByTestId('graph-node-explore'));
      expect(onExploreLayer).not.toHaveBeenCalled();
    });

    test('nodes without a subworkflow target render no affordance', async () => {
      await renderGraph(<WorkflowGraph graph={graph()} onExploreLayer={() => {}} />);
      expect(screen.queryByTestId('graph-node-explore')).not.toBeInTheDocument();
    });
  });
});
