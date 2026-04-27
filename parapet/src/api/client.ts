import { createPromiseClient, Interceptor, PromiseClient } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';
import { CastleService } from '../gen/overlord/v1/castle_connect';
import { OverseerService } from '../sdk/overseer';
import { getAuthToken } from '../authToken';

declare global {
  interface Window {
    __OVERLORD__?: { codec?: 'json' | 'proto' };
  }
}

export type Codec = 'json' | 'proto';

export function getRuntimeCodec(): Codec {
  const runtime = (typeof window !== 'undefined' ? window.__OVERLORD__?.codec : undefined) as
    | Codec
    | undefined;
  if (runtime === 'proto' || runtime === 'json') return runtime;
  if (typeof document !== 'undefined') {
    const meta = document.querySelector('meta[name="overlord-codec"]');
    const value = meta?.getAttribute('content');
    if (value === 'proto' || value === 'json') return value;
  }
  return 'json';
}

const authTokenInterceptor: Interceptor = (next) => async (req) => {
  const token = getAuthToken();
  if (token) {
    req.header.set('Authorization', `Bearer ${token}`);
  }
  return next(req);
};

function baseUrl(): string {
  const fromEnv = (import.meta as unknown as { env?: Record<string, string | undefined> }).env
    ?.VITE_CASTLE_URL;
  if (fromEnv) return fromEnv;
  if (typeof window !== 'undefined' && window.location) return window.location.origin;
  return 'http://localhost:8080';
}

export function createCastleTransport(codec: Codec = getRuntimeCodec()) {
  return createConnectTransport({
    baseUrl: baseUrl(),
    useBinaryFormat: codec === 'proto',
    interceptors: [authTokenInterceptor],
  });
}

export type CastleClient = PromiseClient<typeof CastleService>;
export type OverseerClient = PromiseClient<typeof OverseerService>;

export const castle: CastleClient = createPromiseClient(CastleService, createCastleTransport());
export const overseer: OverseerClient = createPromiseClient(OverseerService, createCastleTransport());
