import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, test, vi } from 'vitest';
import {
  usePauseRunMutation,
  useResumeMutation,
  useStopRunMutation,
} from '../../api/castleApi';
import { RunControls } from './RunControls';
import { NO_CONTROLS_TOOLTIP } from './capabilities';
import type { RunCapabilities } from './capabilities';

vi.mock('../../api/castleApi', async () => {
  const actual = await vi.importActual<typeof import('../../api/castleApi')>(
    '../../api/castleApi',
  );
  return {
    ...actual,
    usePauseRunMutation: vi.fn(),
    useResumeMutation: vi.fn(),
    useStopRunMutation: vi.fn(),
  };
});

type ControlMeta = {
  isLoading?: boolean;
  error?: { status: string; data: string } | undefined;
  isSuccess?: boolean;
};

type PauseHook = ReturnType<typeof usePauseRunMutation>;
type ResumeHook = ReturnType<typeof useResumeMutation>;
type StopHook = ReturnType<typeof useStopRunMutation>;

function controlTuple(
  trigger: ReturnType<typeof vi.fn>,
  meta: ControlMeta,
): [unknown, Record<string, unknown>] {
  return [trigger, { isLoading: false, isSuccess: false, ...meta }];
}

const triggers = {
  pause: vi.fn().mockResolvedValue({ data: { issuedAt: '2026-09-16T17:00:00.000Z' } }),
  resume: vi.fn().mockResolvedValue({ data: { issuedAt: '2026-09-16T17:00:00.000Z' } }),
  stop: vi.fn().mockResolvedValue({ data: { issuedAt: '2026-09-16T17:00:00.000Z' } }),
};

function setMocks(opts: {
  pause?: ControlMeta;
  resume?: ControlMeta;
  stop?: ControlMeta;
}) {
  vi.mocked(usePauseRunMutation).mockReturnValue(
    controlTuple(triggers.pause, opts.pause ?? {}) as unknown as PauseHook,
  );
  vi.mocked(useResumeMutation).mockReturnValue(
    controlTuple(triggers.resume, opts.resume ?? {}) as unknown as ResumeHook,
  );
  vi.mocked(useStopRunMutation).mockReturnValue(
    controlTuple(triggers.stop, opts.stop ?? {}) as unknown as StopHook,
  );
}

function renderControls(status: string, isPaused = false, capabilities?: RunCapabilities) {
  return render(
    <RunControls
      runId="run-1"
      status={status}
      pauseState={{ isPaused, pauseEvent: null }}
      capabilities={capabilities}
    />,
  );
}

beforeEach(() => {
  triggers.pause.mockClear();
  triggers.resume.mockClear();
  triggers.stop.mockClear();
  setMocks({});
});

describe('RunControls', () => {
  describe('control enabling matrix by status', () => {
    const matrix: Array<{
      status: string;
      isPaused?: boolean;
      pause: boolean;
      resume: boolean;
      stop: boolean;
    }> = [
      { status: 'running', pause: true, resume: false, stop: true },
      // A wait event pauses the run before the status flips: resume must
      // already be available while the run still reports "running".
      { status: 'running', isPaused: true, pause: false, resume: true, stop: true },
      // Control-paused runs report "paused" without a wait event yet.
      { status: 'paused', pause: false, resume: true, stop: true },
      { status: 'pending', pause: false, resume: false, stop: true },
      { status: 'succeeded', pause: false, resume: false, stop: false },
      { status: 'failed', pause: false, resume: false, stop: false },
      { status: 'cancelled', pause: false, resume: false, stop: false },
    ];

    for (const m of matrix) {
      test(`status=${m.status} paused=${Boolean(m.isPaused)} → pause:${m.pause ? 'on' : 'off'} resume:${m.resume ? 'on' : 'off'} stop:${m.stop ? 'on' : 'off'}`, () => {
        renderControls(m.status, Boolean(m.isPaused));
        const pause = screen.getByRole('button', { name: /Paus/ });
        const resume = screen.getByRole('button', { name: 'Resume' });
        const stop = screen.getByRole('button', { name: 'Stop' });
        expect(pause.hasAttribute('disabled')).toBe(!m.pause);
        expect(resume.hasAttribute('disabled')).toBe(!m.resume);
        expect(stop.hasAttribute('disabled')).toBe(!m.stop);
      });
    }
  });

  test('terminal runs expose a tooltip instead of live controls', () => {
    renderControls('succeeded');
    for (const name of [/Paus/, 'Resume', 'Stop']) {
      const button = screen.getByRole('button', { name });
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute(
        'title',
        'Run is succeeded — controls unavailable',
      );
    }
  });

  test('pause acts immediately without a dialog', async () => {
    const user = userEvent.setup();
    renderControls('running');
    await user.click(screen.getByRole('button', { name: 'Pause' }));
    expect(triggers.pause).toHaveBeenCalledWith({ runId: 'run-1' });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  test('resume acts immediately without a dialog', async () => {
    const user = userEvent.setup();
    renderControls('paused');
    await user.click(screen.getByRole('button', { name: 'Resume' }));
    expect(triggers.resume).toHaveBeenCalledWith({ runId: 'run-1' });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  test('in-flight pause shows the optimistic pending state and blocks other controls', () => {
    setMocks({ pause: { isLoading: true } });
    renderControls('running');
    const pause = screen.getByRole('button', { name: 'Pausing…' });
    expect(pause).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Resume' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Stop' })).toBeDisabled();
  });

  test('in-flight resume shows the optimistic pending state', () => {
    setMocks({ resume: { isLoading: true } });
    renderControls('running', true);
    const resume = screen.getByRole('button', { name: 'Resuming…' });
    expect(resume).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Pause' })).toBeDisabled();
  });

  test('in-flight stop blocks the trigger with a busy tooltip', () => {
    setMocks({ stop: { isLoading: true } });
    renderControls('running');
    const stop = screen.getByRole('button', { name: 'Stop' });
    expect(stop).toBeDisabled();
    expect(stop).toHaveAttribute('title', 'A control action is in flight');
  });

  test('stop opens a destructive confirm dialog and cancelling issues nothing', async () => {
    const user = userEvent.setup();
    renderControls('running');
    await user.click(screen.getByRole('button', { name: 'Stop' }));

    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(dialog).toHaveTextContent('Stop run?');
    expect(dialog).toHaveTextContent('run-1');
    expect(triggers.stop).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(triggers.stop).not.toHaveBeenCalled();
  });

  test('escape closes the stop confirm dialog without issuing StopRun', async () => {
    const user = userEvent.setup();
    renderControls('running');
    await user.click(screen.getByRole('button', { name: 'Stop' }));
    expect(screen.getByRole('dialog')).toBeInTheDocument();

    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(triggers.stop).not.toHaveBeenCalled();
  });

  describe('capability gating (controls axis)', () => {
    test('no control RPC → all buttons render disabled with the controls tooltip even when running', async () => {
      renderControls('running', false, { controls: false });

      const pause = screen.getByRole('button', { name: /Paus/ });
      const resume = screen.getByRole('button', { name: 'Resume' });
      const stop = screen.getByRole('button', { name: 'Stop' });
      for (const button of [pause, resume, stop]) {
        expect(button).toBeDisabled();
        expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
      }
      // Grayed out is the contract, not hidden: the buttons stay in the DOM.
      expect(triggers.pause).not.toHaveBeenCalled();
    });

    test('capabilities default to the castle host so the status matrix stays authoritative', () => {
      renderControls('running');
      expect(screen.getByRole('button', { name: /Paus/ })).toBeEnabled();
      expect(screen.getByRole('button', { name: 'Stop' })).toBeEnabled();
      expect(screen.getByRole('button', { name: /Paus/ }).getAttribute('title')).not.toBe(
        NO_CONTROLS_TOOLTIP,
      );
    });

    test('the capability axis dominates the status matrix (succeeded run, no controls)', () => {
      renderControls('succeeded', false, { controls: false });
      for (const name of [/Paus/, 'Resume', 'Stop']) {
        const button = screen.getByRole('button', { name });
        expect(button).toBeDisabled();
        expect(button).toHaveAttribute('title', NO_CONTROLS_TOOLTIP);
      }
    });
  });
});
