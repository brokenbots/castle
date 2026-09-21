import { render, screen } from '@testing-library/react';
import { describe, expect, test } from 'vitest';
import { DurationCountdown } from './DurationCountdown';

describe('DurationCountdown', () => {
  test('displays countdown timer for 30s duration', () => {
    const enteredAt = new Date().toISOString();
    render(<DurationCountdown enteredAt={enteredAt} duration="30s" />);

    expect(screen.getByText(/Waiting/i)).toBeInTheDocument();
    // Timer may tick between render and assertion, so check for reasonable range
    expect(screen.getByText(/0:(2[89]|30)/)).toBeInTheDocument();
  });

  test('shows elapsed message when time has passed', () => {
    // Use a time in the past that's already elapsed
    const enteredAt = new Date(Date.now() - 10000).toISOString();
    render(<DurationCountdown enteredAt={enteredAt} duration="5s" />);

    expect(screen.getByText(/Wait elapsed/i)).toBeInTheDocument();
  });

  test('formats hours:minutes:seconds for long durations', () => {
    const enteredAt = new Date().toISOString();
    render(<DurationCountdown enteredAt={enteredAt} duration="3661s" />);

    // Timer may tick between render and assertion, so check for reasonable range
    expect(screen.getByText(/1:0[01]:0[01]/)).toBeInTheDocument();
  });
});
