import { render, screen } from '@testing-library/react';
import { describe, expect, test, vi } from 'vitest';
import { WorkflowSourceView } from './WorkflowSourceView';

describe('WorkflowSourceView', () => {
  test('renders the source unhighlighted when no range is given', () => {
    render(<WorkflowSourceView source={'step "a" {\n}\n'} />);
    expect(screen.getByTestId('workflow-source-view').textContent).toBe('step "a" {\n}\n');
    expect(screen.queryByTestId('workflow-source-highlight')).not.toBeInTheDocument();
  });

  test('highlights exactly the given range and scrolls to it', () => {
    const scrollIntoView = vi.fn();
    // jsdom does not implement scrollIntoView; stub it on Element (the
    // prototype the <mark> inherits).
    Object.defineProperty(Element.prototype, 'scrollIntoView', {
      value: scrollIntoView,
      configurable: true,
      writable: true,
    });
    // source indices: "step \"a\" {\n}\n" spans [0,13); the "b" block spans
    // [13, 25) — header through closing brace, trailing newline excluded.
    const source = 'step "a" {\n}\nstep "b" {\n}\n';
    render(<WorkflowSourceView source={source} highlight={{ start: 13, end: 25 }} />);
    const highlight = screen.getByTestId('workflow-source-highlight');
    expect(highlight.textContent).toBe('step "b" {\n}');
    // The scroll effect ran once for the range.
    expect(scrollIntoView).toHaveBeenCalledWith({ block: 'center' });
  });

  test('ignores out-of-bounds or empty ranges', () => {
    const source = 'step "a" {\n}\n';
    render(<WorkflowSourceView source={source} highlight={{ start: 100, end: 120 }} />);
    expect(screen.queryByTestId('workflow-source-highlight')).not.toBeInTheDocument();
    render(<WorkflowSourceView source={source} highlight={{ start: 0, end: 0 }} />);
    expect(screen.queryByTestId('workflow-source-highlight')).not.toBeInTheDocument();
  });
});