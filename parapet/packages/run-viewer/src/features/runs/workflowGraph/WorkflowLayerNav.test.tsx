import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, test, vi } from 'vitest';
import { WorkflowLayerNav } from './WorkflowLayerNav';
import { buildSubworkflowLayers } from './layers';

const PARENT_BODY =
  'workflow {\n  name = "qa_triage"\n  initial_state = "triage"\n}\nstep "triage" {\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';
const CHILD_BODY =
  'workflow {\n  name = "verdict"\n  initial_state = "review"\n}\nstep "review" {\n  outcome "success" { next = state.done }\n}\nstate "done" {\n  terminal = true\n  success  = true\n}';

function stack() {
  return buildSubworkflowLayers({
    subworkflows: [
      { name: 'qa_triage', sourcePath: '../qa_triage_v1', body: PARENT_BODY },
      { name: 'verdict', sourcePath: './verdict_v1', body: CHILD_BODY },
    ],
  });
}

describe('WorkflowLayerNav', () => {
  test('renders the root crumb plus one crumb per open layer, deepest marked current', () => {
    render(<WorkflowLayerNav rootName="linear_intake_v1" stack={stack().slice(0, 1)} onNavigate={() => {}} />);

    const breadcrumb = screen.getByTestId('layer-breadcrumb');
    expect(within(breadcrumb).getByTestId('layer-crumb-root')).toHaveTextContent('linear_intake_v1');
    const crumbs = within(breadcrumb).getAllByTestId('layer-crumb');
    expect(crumbs).toHaveLength(1);
    expect(crumbs[0]).toHaveTextContent('qa_triage');
    expect(crumbs[0]).toHaveAttribute('aria-current', 'page');
    expect(within(breadcrumb).getByTestId('layer-crumb-root')).not.toHaveAttribute('aria-current');
  });

  test('navigating from a crumb reports the depth; the root crumb reports 0', async () => {
    const onNavigate = vi.fn();
    const user = userEvent.setup();
    const layers = stack();
    render(<WorkflowLayerNav rootName="linear_intake_v1" stack={layers} onNavigate={onNavigate} />);

    await user.click(screen.getByTestId('layer-crumb-root'));
    expect(onNavigate).toHaveBeenLastCalledWith(0);

    // The first layer's crumb navigates to depth 1 even while deeper
    // layers are open.
    await user.click(screen.getAllByTestId('layer-crumb')[0]);
    expect(onNavigate).toHaveBeenLastCalledWith(1);

    const deepest = screen.getAllByTestId('layer-crumb');
    expect(deepest[1]).toHaveAttribute('aria-current', 'page');
    expect(deepest[0]).not.toHaveAttribute('aria-current');
  });

  test('the thumbnail rail renders one always-LR thumbnail per open layer', () => {
    render(<WorkflowLayerNav rootName="linear_intake_v1" stack={stack()} onNavigate={() => {}} />);

    const thumbs = screen.getAllByTestId('layer-thumb');
    expect(thumbs).toHaveLength(2);
    // Thumbnails are static SVGs labeled per layer.
    expect(within(thumbs[0]).getByRole('img', { name: 'qa_triage thumbnail' })).toBeInTheDocument();
    expect(within(thumbs[1]).getByRole('img', { name: 'verdict thumbnail' })).toBeInTheDocument();
    // The deepest layer's thumb is pressed.
    expect(thumbs[1]).toHaveAttribute('aria-pressed', 'true');
    expect(thumbs[0]).toHaveAttribute('aria-pressed', 'false');
  });

  test('thumbnail clicks navigate to that layer depth', async () => {
    const onNavigate = vi.fn();
    const user = userEvent.setup();
    render(<WorkflowLayerNav rootName="linear_intake_v1" stack={stack()} onNavigate={onNavigate} />);

    await user.click(screen.getAllByTestId('layer-thumb')[0]);
    expect(onNavigate).toHaveBeenLastCalledWith(1);
  });

  test('a layer whose body did not parse shows a placeholder thumbnail', () => {
    const layers = buildSubworkflowLayers({ subworkflows: [{ name: 'broken', body: '' }] });
    render(<WorkflowLayerNav rootName="root" stack={layers} onNavigate={() => {}} />);

    expect(screen.getAllByTestId('layer-thumb')).toHaveLength(1);
    expect(screen.getByText('no graph')).toBeInTheDocument();
    // The breadcrumb still names the layer.
    expect(screen.getByTestId('layer-crumb')).toHaveTextContent('broken');
  });
});