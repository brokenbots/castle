import '@testing-library/jest-dom/vitest';
import { afterAll, afterEach, beforeAll, vi } from 'vitest';
import { clearAuthToken } from '../authToken';
import { server } from './mocks/server';

vi.stubGlobal('AbortController', window.AbortController);
vi.stubGlobal('AbortSignal', window.AbortSignal);

// @xyflow/react (workflow DAG) uses ResizeObserver, which jsdom does not
// provide. React Flow only renders edges once nodes report their measured
// dimensions, so the stub notifies the callback asynchronously (synchronous
// notification breaks React Flow's measure passes); dimension readers use
// getBoundingClientRect.
class ResizeObserverStub {
  private callback: ResizeObserverCallback;

  constructor(callback: ResizeObserverCallback) {
    this.callback = callback;
  }

  observe(target: Element): void {
    queueMicrotask(() => {
      const box = target.getBoundingClientRect();
      this.callback(
        [
          {
            target,
            contentRect: {
              x: box.x,
              y: box.y,
              width: box.width,
              height: box.height,
              top: box.top,
              left: box.left,
              right: box.right,
              bottom: box.bottom,
              toJSON: () => ({}),
            },
          } as unknown as ResizeObserverEntry,
        ],
        this as unknown as ResizeObserver,
      );
    });
  }

  unobserve(): void {}

  disconnect(): void {}
}
vi.stubGlobal('ResizeObserver', ResizeObserverStub);

// React Flow reads the viewport zoom via window.DOMMatrixReadOnly, which
// jsdom does not implement; only the 2D scale (m22) is consumed.
class DOMMatrixReadOnlyStub {
  m22: number;

  constructor(transform?: string) {
    const values = transform?.match(/matrix\(([^)]+)\)/)?.[1]?.split(',').map((v) => Number.parseFloat(v)) ?? [];
    this.m22 = values.length >= 4 ? values[3] : 1;
  }
}
vi.stubGlobal('DOMMatrixReadOnly', DOMMatrixReadOnlyStub);

// Edge label measurement in @xyflow/react calls SVGTextElement.getBBox,
// which jsdom does not implement. A zero box is enough for rendering.
Object.defineProperty(SVGElement.prototype, 'getBBox', {
  value: () => ({ x: 0, y: 0, width: 0, height: 0 }),
  configurable: true,
});

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  clearAuthToken();
});
afterAll(() => server.close());
