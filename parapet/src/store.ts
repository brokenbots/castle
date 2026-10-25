import { setRunAuthTokenProvider, createRunViewerStore, type RunViewerStore } from '@castle/run-viewer';
import { getAuthToken } from './authToken';
import { settingsApi, SETTINGS_API_REDUCER_PATH } from './features/settings/settingsApi';

// The run-viewer package owns the store shape (api + runs + session); the
// parapet console wires its auth-token store into the transport and hosts
// the settings console api slice alongside the viewer slices.
setRunAuthTokenProvider(getAuthToken);

export const store: RunViewerStore = createRunViewerStore({
  extraReducers: { [SETTINGS_API_REDUCER_PATH]: settingsApi.reducer },
  extraMiddleware: [settingsApi.middleware],
  extraApiPaths: [SETTINGS_API_REDUCER_PATH],
});

export type RootState = ReturnType<typeof store.getState>;
export type AppDispatch = typeof store.dispatch;