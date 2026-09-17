import { createSlice } from '@reduxjs/toolkit';

export interface SessionState {
  // Set when Castle rejects the stored token after login (a 401 /
  // `unauthenticated` observed by any data fetch or the watch stream). The
  // app gate reacts by showing the login page again; the flag clears once a
  // valid token is (re)established or the user logs out.
  authExpired: boolean;
}

const initialState: SessionState = { authExpired: false };

export const sessionSlice = createSlice({
  name: 'session',
  initialState,
  reducers: {
    sessionExpired(state) {
      state.authExpired = true;
    },
    sessionRecovered(state) {
      state.authExpired = false;
    },
  },
});

export const { sessionExpired, sessionRecovered } = sessionSlice.actions;

export const selectAuthExpired = (state: { session: SessionState }) => state.session.authExpired;
