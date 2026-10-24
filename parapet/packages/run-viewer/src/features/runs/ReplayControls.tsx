import { useState } from 'react';
import { useEventReplay } from './eventLog/useEventReplay';

const REPLAY_BUTTON_CLASSES = 'px-3 py-1.5 rounded-md border border-line-strong text-body font-semibold text-ink-muted hover:bg-surface-raised hover:text-ink';
// The motion toggle shares the replay button recipe plus FollowButton's
// pressed accent; the classes stay literal here so the JIT generates them.
const MOTION_BUTTON_CLASSES = `${REPLAY_BUTTON_CLASSES} aria-pressed:border-accent-strong aria-pressed:bg-accent-strong/10 aria-pressed:text-accent-strong`;

/**
 * Replay transport controls (debug tooling) plus the shared motion toggle:
 * one ticking timeline, one manual mode, one Pause/Resume button spanning
 * the live tail and the replay.
 *
 * - The motion toggle is the single Pause/Resume button in both modes and
 *   always reads the NEXT action: "Pause" while the view is moving,
 *   "Resume" while it stands still. Clicking Resume un-pauses — on the
 *   live tail that means follow live again (track the newest step);
 *   during a replay it means continue the ticker.
 * - `Replay N steps` starts the ticker at the selected speed (5s default).
 * - `Prev` / `Next` step through boundaries; **Prev pauses** on the
 *   isolated step, **Next advances and resumes** the ticker (it is also
 *   the un-pause).
 * - Changing the step number isolates that step and pauses there.
 * - While the run itself is live the timing selector is disabled: the
 *   tail moves at the engine's pace, so step timing is a replay-only dial.
 */
export function ReplayControls({
  replay,
  live,
  followEnabled,
  onToggleFollow,
}: {
  replay: ReturnType<typeof useEventReplay>;
  /** The run is still live-tailing (status running, no terminal event). */
  live: boolean;
  /** Live-tail follow mode: the motion toggle's live face. */
  followEnabled: boolean;
  onToggleFollow: () => void;
}) {
  const total = replay.stepCount;
  const [stepInput, setStepInput] = useState('');
  // The motion toggle renders even on a run with no replayable steps
  // (zero step boundaries): the live face is the run's follow control,
  // independent of the replay feature.
  if (total === 0 && !replay.active) {
    return (
      <div className="flex items-center gap-2" data-testid="replay-controls">
        <button
          type="button"
          data-testid="graph-follow-toggle"
          aria-pressed={followEnabled}
          title={followEnabled ? 'Pause following — release the view from the newest step' : 'Follow live — keep the view on the newest step'}
          onClick={onToggleFollow}
          className={MOTION_BUTTON_CLASSES}
        >
          {followEnabled ? 'Pause' : 'Resume'}
        </button>
      </div>
    );
  }
  if (total === 0) return null; // active replay over a de-populated list
  return (
    <div className="flex items-center gap-2" data-testid="replay-controls">
      {replay.active ? (
        <>
          <button
            type="button"
            data-testid="replay-prev"
            title="Rewind to the previous step boundary (pauses there)"
            onClick={replay.previous}
            className={REPLAY_BUTTON_CLASSES}
          >
            Prev
          </button>
          <button
            type="button"
            data-testid="replay-next"
            title="Advance to the next step boundary and resume the timeline"
            onClick={replay.next}
            className={REPLAY_BUTTON_CLASSES}
          >
            Next
          </button>
          {/* Jump-to-step: typing a number isolates that step and pauses. */}
          <label className="flex items-center gap-1 text-meta text-ink-muted select-none">
            step
            <input
              type="number"
              min={1}
              max={total}
              data-testid="replay-step-input"
              placeholder={String(replay.consumed)}
              value={stepInput}
              onChange={(e) => {
                setStepInput(e.target.value);
                const n = Number(e.target.value);
                if (e.target.value !== '' && Number.isFinite(n)) {
                  replay.jumpToStep(n); // isolates + pauses on the typed step
                }
              }}
              className="w-16 rounded border border-line bg-surface px-1 py-0.5 text-body text-ink"
              title="Type a step number to jump (pauses there)"
            />
            /{total}
          </label>
          {/* The motion toggle, replay face: pause/continue the ticker. */}
          <button
            type="button"
            data-testid="replay-pause"
            aria-pressed={replay.active && !replay.paused && !replay.finished}
            title={
              replay.finished
                ? 'Replay finished — the full loaded history is shown'
                : replay.paused
                  ? 'Continue the replay'
                  : 'Pause the replay'
            }
            onClick={replay.paused ? replay.resume : replay.pause}
            className={MOTION_BUTTON_CLASSES}
          >
            {replay.paused || replay.finished ? 'Resume' : 'Pause'}
          </button>
          {/* Timing (replay-only dial): disabled while the run is live —
              the tail moves at the engine's pace. */}
          <label data-testid="replay-speed" className="flex items-center gap-1 text-meta text-ink-muted select-none">
            <select
              data-testid="replay-speed-select"
              value={replay.speedMs}
              disabled={live}
              onChange={(e) => replay.setSpeedMs(Number(e.target.value))}
              className="rounded border border-line bg-surface px-1 py-0.5 text-body text-ink disabled:cursor-not-allowed disabled:opacity-50"
              title={live ? "Timing is the engine's while the run streams live" : 'Seconds per replayed step'}
            >
              {[500, 1000, 2500, 5000, 10000].map((ms) => (
                <option key={ms} value={ms}>
                  {ms / 1000}s
                </option>
              ))}
            </select>
          </label>
        </>
      ) : (
        <>
          {/* The motion toggle, live face: follow the live tail. Reads
              Pause while following, Resume to follow again. */}
          <button
            type="button"
            data-testid="graph-follow-toggle"
            aria-pressed={followEnabled}
            title={followEnabled ? 'Pause following — release the view from the newest step' : 'Follow live — keep the view on the newest step'}
            onClick={onToggleFollow}
            className={MOTION_BUTTON_CLASSES}
          >
            {followEnabled ? 'Pause' : 'Resume'}
          </button>
          <button
            type="button"
            data-testid="replay-start"
            title={`Replay the run over the graph: one step every 5 seconds (${total} steps)`}
            onClick={replay.start}
            className={REPLAY_BUTTON_CLASSES}
          >
            Replay {total} steps
          </button>
        </>
      )}
    </div>
  );
}