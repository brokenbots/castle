import { useEffect, useRef } from 'react';
import type { HclRange } from './hcl';

interface WorkflowSourceViewProps {
  source: string;
  /** Exact source range to highlight (e.g. the selected node's declaration). */
  highlight?: HclRange | null;
  testId?: string;
}

/**
 * Read-only workflow source renderer. When the graph model supplies an exact
 * declaration range (recorded by the HCL parser at parse time), the block is
 * highlighted inside the same <pre> — no re-scanning — and scrolled into
 * view so a node click points at its source (CRI-257).
 */
export function WorkflowSourceView({ source, highlight = null, testId = 'workflow-source-view' }: WorkflowSourceViewProps) {
  const markRef = useRef<HTMLElement>(null);
  const valid =
    !!highlight && highlight.start >= 0 && highlight.end > highlight.start && highlight.end <= source.length;

  useEffect(() => {
    const el = markRef.current;
    if (!el || typeof el.scrollIntoView !== 'function') return;
    el.scrollIntoView({ block: 'center' });
  }, [highlight?.start, highlight?.end]);

  return (
    <pre data-testid={testId} className="text-xs font-mono bg-slate-900 rounded p-3 overflow-auto max-h-[32vh]">
      {valid ? (
        <>
          {source.slice(0, highlight!.start)}
          <mark ref={markRef} data-testid="workflow-source-highlight" className="bg-sky-500/30 text-sky-100 rounded-sm">
            {source.slice(highlight!.start, highlight!.end)}
          </mark>
          {source.slice(highlight!.end)}
        </>
      ) : (
        source
      )}
    </pre>
  );
}