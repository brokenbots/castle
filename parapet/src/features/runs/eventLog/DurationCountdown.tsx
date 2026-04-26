import { useEffect, useState } from 'react';

interface DurationCountdownProps {
  enteredAt: string;
  duration: string;
}

export function DurationCountdown({ enteredAt, duration }: DurationCountdownProps) {
  const [remaining, setRemaining] = useState<number>(0);

  useEffect(() => {
    const parseDuration = (d: string): number => {
      const match = d.match(/^(\d+)(ms|s|m|h)$/);
      if (!match) return 0;
      const value = parseInt(match[1], 10);
      const unit = match[2];
      switch (unit) {
        case 'ms':
          return value;
        case 's':
          return value * 1000;
        case 'm':
          return value * 60 * 1000;
        case 'h':
          return value * 60 * 60 * 1000;
        default:
          return 0;
      }
    };

    const durationMs = parseDuration(duration);
    const enteredMs = new Date(enteredAt).getTime();
    const endMs = enteredMs + durationMs;

    const updateRemaining = () => {
      const now = Date.now();
      const diff = Math.max(0, endMs - now);
      setRemaining(diff);
    };

    updateRemaining();
    const intervalId = setInterval(updateRemaining, 1000);

    return () => clearInterval(intervalId);
  }, [enteredAt, duration]);

  const seconds = Math.floor(remaining / 1000);
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);

  const displayMinutes = minutes % 60;
  const displaySeconds = seconds % 60;

  const timeString =
    hours > 0
      ? `${hours}:${String(displayMinutes).padStart(2, '0')}:${String(displaySeconds).padStart(2, '0')}`
      : `${displayMinutes}:${String(displaySeconds).padStart(2, '0')}`;

  if (remaining === 0) {
    return (
      <div className="bg-slate-800 rounded px-4 py-3 border border-slate-700">
        <p className="text-sm text-slate-300">
          <span className="text-sky-400 font-semibold">Wait elapsed</span> — duration complete, resuming...
        </p>
      </div>
    );
  }

  return (
    <div className="bg-slate-800 rounded px-4 py-3 border border-slate-700">
      <p className="text-sm text-slate-300">
        <span className="text-sky-400 font-semibold">Waiting</span> — resuming in{' '}
        <span className="font-mono text-lg text-white">{timeString}</span>
      </p>
    </div>
  );
}
