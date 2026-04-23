import { configureStore } from '@reduxjs/toolkit';
import { castleApi } from './api/castleApi';

export const store = configureStore({
  reducer: {
    [castleApi.reducerPath]: castleApi.reducer,
  },
  middleware: (getDefault) => getDefault().concat(castleApi.middleware),
});

export type RootState = ReturnType<typeof store.getState>;
export type AppDispatch = typeof store.dispatch;
