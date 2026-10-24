/**
 * The standard follow toggle widget: one shared button recipe for every
 * follow control in the run view (graph toolbar, drawer panels, event
 * log). A toggled chip — accent tinted when pressed, plain otherwise —
 * driven by `aria-pressed`; labels stay the caller's.
 */
export function FollowButton({
  label,
  pressed,
  testId,
  title,
  onToggle,
}: {
  label: string;
  pressed: boolean;
  testId: string;
  title: string;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      data-testid={testId}
      aria-pressed={pressed}
      title={title}
      onClick={onToggle}
      className="rounded-md border px-3 py-1.5 text-body font-semibold text-ink-muted hover:bg-surface-raised hover:text-ink aria-pressed:border-accent-strong aria-pressed:bg-accent-strong/10 aria-pressed:text-accent-strong"
    >
      {label}
    </button>
  );
}