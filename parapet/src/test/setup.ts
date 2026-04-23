import '@testing-library/jest-dom/vitest';
import { afterAll, afterEach, beforeAll, vi } from 'vitest';
import { clearAuthToken } from '../authToken';
import { server } from './mocks/server';

class MockWebSocket {
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: (() => void) | null = null;

  constructor(_url: string) {}

  close() {
    if (this.onclose) this.onclose();
  }
}

vi.stubGlobal('WebSocket', MockWebSocket);
vi.stubGlobal('AbortController', window.AbortController);
vi.stubGlobal('AbortSignal', window.AbortSignal);

beforeAll(() => server.listen());
afterEach(() => {
  server.resetHandlers();
  clearAuthToken();
});
afterAll(() => server.close());
