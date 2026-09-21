import {
  configureStore,
  isAction,
  type Middleware,
  type ReducersMapObject,
} from '@reduxjs/toolkit';
import { castleApi, RUN_VIEWER_API_REDUCER_PATH } from './api/castleApi';
import { isUnauthenticatedError } from './api/errors';
import { sessionExpired, sessionSlice } from './features/auth/sessionSlice';
import { runsSlice } from './features/runs/runsSlice';

// Any run-viewer API request rejected as unauthenticated mid-session marks
// the session expired so hosts render their re-auth prompt (parapet's app
// gate shows the login page again) instead of leaving stuck failure states
// behind. Condition-rejected requests (aborts, subscription churn) carry no
// queryFn error payload and are ignored.
export function createUnauthenticatedErrorMiddleware(): Middleware {
  return (api) => (next) => (action) => {
    const result = next(action);
    if (
      isAction(action) &&
      action.type.startsWith(`${RUN_VIEWER_API_REDUCER_PATH}/`) &&
      action.type.endsWith('/rejected') &&
      isUnauthenticatedError((action as { payload?: unknown }).payload)
    ) {
      api.dispatch(sessionExpired());
    }
    return result;
  };
}

export interface RunViewerStoreOptions {
  /**
   * Host reducers merged alongside the run-viewer slices. Keys must not
   * collide with `castleApi`, `runs`, or `session`.
   */
  extraReducers?: ReducersMapObject;
}

/**
 * Builds a self-contained store hosting the run-viewer feature: the RTK api,
 * the runs state, and the session-expiry flag the re-auth affordances drive.
 * Hosts call this once at boot; hosts with their own reducers merge them via
 * `extraReducers`.
 */
export function createRunViewerStore(options: RunViewerStoreOptions = {}) {
  const extra = options.extraReducers ?? {};
  if ('castleApi' in extra || 'runs' in extra || 'session' in extra) {
    throw new Error("run-viewer reserves the 'castleApi', 'runs' and 'session' reducer keys");
  }
  return configureStore({
    reducer: {
      [castleApi.reducerPath]: castleApi.reducer,
      runs: runsSlice.reducer,
      session: sessionSlice.reducer,
      ...extra,
    },
    middleware: (getDefault) =>
      getDefault().concat(castleApi.middleware, createUnauthenticatedErrorMiddleware()),
  });
}

export type RunViewerStore = ReturnType<typeof createRunViewerStore>;
export type AppDispatch = RunViewerStore['dispatch'];
export type RootState = ReturnType<RunViewerStore['getState']>;