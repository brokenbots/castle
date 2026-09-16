/**
 * Tail state for the live event log.
 *
 * - `autoFollow`: user opt-out switch. When on, new arrivals keep the view
 *   pinned to the bottom while the user has not scrolled away.
 * - `pinned`: whether the view is currently pinned to the bottom.
 * - `unseen`: count of events that arrived while the user was scrolled up.
 */
export interface TailState {
  autoFollow: boolean;
  pinned: boolean;
  unseen: number;
}

export interface TailAction {
  type: 'eventArrived' | 'scrolledAtBottom' | 'scrolledUp' | 'jumpRequested' | 'autoFollowChanged';
  /** Number of newly arrived events; several can land in a single render. */
  count?: number;
  enabled?: boolean;
}

export const initialTailState = (): TailState => ({
  autoFollow: true,
  pinned: false,
  unseen: 0,
});

export function tailReducer(state: TailState, action: TailAction): TailState {
  switch (action.type) {
    case 'eventArrived': {
      // Arrivals only accumulate unseen counts while following and not pinned.
      if (!state.autoFollow || state.pinned) return state;
      const count = action.count ?? 1;
      if (count <= 0) return state;
      return { ...state, unseen: state.unseen + count };
    }
    case 'scrolledAtBottom':
      if (!state.autoFollow || state.pinned) return state;
      return { ...state, pinned: true, unseen: 0 };
    case 'scrolledUp':
      if (!state.autoFollow || !state.pinned) return state;
      return { ...state, pinned: false };
    case 'jumpRequested':
      if (state.pinned && state.unseen === 0) return state;
      return { ...state, pinned: true, unseen: 0 };
    case 'autoFollowChanged': {
      const enabled = action.enabled ?? false;
      if (enabled === state.autoFollow) return state;
      if (enabled) return { ...state, autoFollow: true, pinned: true, unseen: 0 };
      return { ...state, autoFollow: false, pinned: false, unseen: 0 };
    }
    default:
      return state;
  }
}
