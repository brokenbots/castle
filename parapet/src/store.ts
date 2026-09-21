import { setRunAuthTokenProvider, createRunViewerStore, type RunViewerStore } from '@castle/run-viewer';
import { getAuthToken } from './authToken';

// The run-viewer package owns the store shape (api + runs + session); the
// parapet console only wires its auth-token store into the transport.
setRunAuthTokenProvider(getAuthToken);

export const store: RunViewerStore = createRunViewerStore();

export type RootState = ReturnType<typeof store.getState>;
export type AppDispatch = typeof store.dispatch;