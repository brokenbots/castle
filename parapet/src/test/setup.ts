import '@testing-library/jest-dom/vitest';
import { afterAll, afterEach, beforeAll, vi } from 'vitest';
import { clearAuthToken } from '../authToken';
import { server } from './mocks/server';

vi.stubGlobal('AbortController', window.AbortController);
vi.stubGlobal('AbortSignal', window.AbortSignal);

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  clearAuthToken();
});
afterAll(() => server.close());
