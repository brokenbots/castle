import { configureStore, isAction, type Middleware } from '@reduxjs/toolkit';
import { castleApi } from './api/castleApi';
import { isUnauthenticatedError } from './api/errors';
import { runsSlice } from './features/runs/runsSlice';
import { sessionExpired, sessionSlice } from './features/auth/sessionSlice';

// Any Castle API request rejected as unauthenticated mid-session (auth token
// expired or revoked while the user is working) marks the session expired so
// the app gate returns the user to the login page instead of leaving stuck
// failure states behind. Condition-rejected requests (aborts, subscription
// churn) carry no queryFn error payload and are ignored.
const authExpiryMiddleware: Middleware = (api) => (next) => (action) => {
  const result = next(action);
  if (
    isAction(action) &&
    action.type.startsWith(`${castleApi.reducerPath}/`) &&
    action.type.endsWith('/rejected') &&
    isUnauthenticatedError((action as { payload?: unknown }).payload)
  ) {
    api.dispatch(sessionExpired());
  }
  return result;
};

export const store = configureStore({
  reducer: {
    [castleApi.reducerPath]: castleApi.reducer,
    runs: runsSlice.reducer,
    session: sessionSlice.reducer,
  },
  middleware: (getDefault) => getDefault().concat(castleApi.middleware, authExpiryMiddleware),
});

export type RootState = ReturnType<typeof store.getState>;
export type AppDispatch = typeof store.dispatch;
