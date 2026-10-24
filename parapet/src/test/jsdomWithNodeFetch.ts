import { builtinEnvironments } from 'vitest/environments';

// jsdom's Request/Response/AbortController classes are incompatible with
// msw/node's interceptor chain: connect-web constructs controllers with the
// global AbortController, and the interceptor validates `signal instanceof
// AbortSignal` against Node's (undici) class inside `new Request(...)` — a
// jsdom signal fails that check, so every wire call dies as
// ConnectError [unknown] and every wire-level test sees the wrong error code.
//
// This environment keeps Node's fetch primitives reachable: it snapshots
// them BEFORE the jsdom populate pass and re-installs them afterwards, so
// the app under test, connect-web and msw all agree on one class set.
const KEEP = [
  'fetch',
  'Request',
  'Response',
  'Headers',
  'AbortController',
  'AbortSignal',
] as const;

// Neither jsdom's populate pass nor Node 26 expose a working storage
// implementation in the test VM (Node's global storage getter is inert
// without --localstorage-file), so tests polyfill it. A Map-backed
// Storage satisfies everything app code does with it.
function createMemoryStorage(): Storage {
  const data = new Map<string, string>();
  const order: string[] = [];
  return {
    get length() {
      return order.length;
    },
    clear: () => {
      data.clear();
      order.length = 0;
    },
    getItem: (key: string) => data.get(key) ?? null,
    key: (index: number) => order[index] ?? null,
    removeItem: (key: string) => {
      data.delete(key);
      const at = order.indexOf(key);
      if (at >= 0) order.splice(at, 1);
    },
    setItem: (key: string, value: string) => {
      if (!data.has(key)) order.push(key);
      data.set(key, String(value));
    },
  } as unknown as Storage;
}

const base = builtinEnvironments.jsdom;

type EnvGlobal = Record<string, unknown>;

interface EnvSetupOptions {
  jsdom?: Record<string, unknown>;
  [key: string]: unknown;
}

export default {
  name: 'jsdom-with-node-fetch',
  transformMode: base.transformMode,
  async setup(global: EnvGlobal, options: EnvSetupOptions) {
    // Snapshot Node's classes before jsdom's populateGlobal swaps them.
    const nodeGlobals: Record<string, unknown> = {};
    for (const key of KEEP) {
      const value = global[key];
      if (typeof value === 'function') nodeGlobals[key] = value;
    }
    const teardownBase = await base.setup(global as never, options as never);
    // Re-install Node's classes on top (plain data properties — getters
    // installed by populateGlobal are configurable, so defineProperty wins).
    for (const key of KEEP) {
      const value = nodeGlobals[key];
      if (value !== undefined) {
        Object.defineProperty(global, key, {
          value,
          writable: true,
          configurable: true,
          enumerable: true,
        });
      }
    }
    // Storage polyfill (see above).
    Object.defineProperty(global, 'localStorage', {
      value: createMemoryStorage(),
      writable: true,
      configurable: true,
      enumerable: true,
    });
    Object.defineProperty(global, 'sessionStorage', {
      value: createMemoryStorage(),
      writable: true,
      configurable: true,
      enumerable: true,
    });
    return {
      async teardown(global2: Record<string, unknown>) {
        await teardownBase?.teardown(global2);
      },
    };
  },
};