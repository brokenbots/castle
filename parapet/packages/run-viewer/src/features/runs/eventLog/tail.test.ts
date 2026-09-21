import { describe, expect, test } from 'vitest';
import { initialTailState, tailReducer, type TailState } from './tail';

describe('tailReducer', () => {
  test('starts following but not pinned', () => {
    expect(initialTailState()).toEqual({ autoFollow: true, pinned: false, unseen: 0 });
  });

  test('counts arrivals behind an unpinned, following view', () => {
    let state: TailState = initialTailState();
    state = tailReducer(state, { type: 'eventArrived' });
    state = tailReducer(state, { type: 'eventArrived' });
    expect(state).toEqual({ autoFollow: true, pinned: false, unseen: 2 });
  });

  test('counts a batched arrival by its event count, not one per update', () => {
    const state = tailReducer(initialTailState(), { type: 'eventArrived', count: 3 });
    expect(state).toEqual({ autoFollow: true, pinned: false, unseen: 3 });
  });

  test('accumulates batched arrivals onto the existing unseen count', () => {
    let state = tailReducer(initialTailState(), { type: 'eventArrived', count: 2 });
    state = tailReducer(state, { type: 'eventArrived', count: 2 });
    expect(state).toEqual({ autoFollow: true, pinned: false, unseen: 4 });
  });

  test('ignores non-positive arrival counts', () => {
    const state = initialTailState();
    expect(tailReducer(state, { type: 'eventArrived', count: 0 })).toBe(state);
  });

  test('does not count arrivals once the view is pinned', () => {
    let state = tailReducer(initialTailState(), { type: 'scrolledAtBottom' });
    const before = state;
    state = tailReducer(state, { type: 'eventArrived' });
    expect(state.pinned).toBe(true);
    expect(state.unseen).toBe(0);
    expect(state).toBe(before);
  });

  test('does not count arrivals when auto-follow is opted out', () => {
    let state = tailReducer(initialTailState(), { type: 'autoFollowChanged', enabled: false });
    state = tailReducer(state, { type: 'eventArrived' });
    state = tailReducer(state, { type: 'eventArrived' });
    expect(state).toEqual({ autoFollow: false, pinned: false, unseen: 0 });
  });

  test('pins and clears unseen when the user scrolls back to the bottom', () => {
    let state = tailReducer(initialTailState(), { type: 'eventArrived' });
    state = tailReducer(state, { type: 'scrolledAtBottom' });
    expect(state).toEqual({ autoFollow: true, pinned: true, unseen: 0 });
  });

  test('does not count arrivals when auto-follow is off after a re-enable cycle', () => {
    let state = tailReducer(initialTailState(), { type: 'autoFollowChanged', enabled: false });
    state = tailReducer(state, { type: 'autoFollowChanged', enabled: true });
    expect(state).toEqual({ autoFollow: true, pinned: true, unseen: 0 });
  });

  test('re-pinning at the bottom is a no-op while already pinned', () => {
    const state = tailReducer(initialTailState(), { type: 'scrolledAtBottom' });
    expect(tailReducer(state, { type: 'scrolledAtBottom' })).toBe(state);
  });

  test('detaches on scroll-up and preserves the unseen count', () => {
    let state = tailReducer(initialTailState(), { type: 'eventArrived' });
    state = tailReducer(state, { type: 'scrolledAtBottom' });
    state = tailReducer(state, { type: 'scrolledUp' });
    expect(state).toEqual({ autoFollow: true, pinned: false, unseen: 0 });
    state = tailReducer(state, { type: 'eventArrived' });
    expect(state.unseen).toBe(1);
  });

  test('ignores scroll-up while not pinned', () => {
    const state = initialTailState();
    expect(tailReducer(state, { type: 'scrolledUp' })).toBe(state);
  });

  test('ignores scroll-up when auto-follow is opted out', () => {
    const state = tailReducer(initialTailState(), { type: 'autoFollowChanged', enabled: false });
    expect(tailReducer(state, { type: 'scrolledUp' })).toBe(state);
  });

  test('jumping re-pins the view and clears unseen events', () => {
    let state = tailReducer(initialTailState(), { type: 'eventArrived' });
    state = tailReducer(state, { type: 'eventArrived' });
    state = tailReducer(state, { type: 'jumpRequested' });
    expect(state).toEqual({ autoFollow: true, pinned: true, unseen: 0 });
  });

  test('jumping is a no-op while pinned with nothing unseen', () => {
    const state = tailReducer(initialTailState(), { type: 'scrolledAtBottom' });
    expect(tailReducer(state, { type: 'jumpRequested' })).toBe(state);
  });

  test('enabling auto-follow re-pins and clears unseen events', () => {
    let state = tailReducer(initialTailState(), { type: 'autoFollowChanged', enabled: false });
    state = tailReducer(state, { type: 'eventArrived' });
    state = tailReducer(state, { type: 'autoFollowChanged', enabled: true });
    expect(state).toEqual({ autoFollow: true, pinned: true, unseen: 0 });
  });

  test('disabling auto-follow detaches and clears unseen events', () => {
    let state = tailReducer(initialTailState(), { type: 'eventArrived' });
    state = tailReducer(state, { type: 'autoFollowChanged', enabled: false });
    expect(state).toEqual({ autoFollow: false, pinned: false, unseen: 0 });
  });

  test('setting auto-follow to its current value is a no-op', () => {
    const state = initialTailState();
    expect(tailReducer(state, { type: 'autoFollowChanged', enabled: true })).toBe(state);
  });
});
