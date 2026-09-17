import { Code, ConnectError } from '@connectrpc/connect';
import { describe, expect, test } from 'vitest';
import { classifyError, connectCodeName, isUnauthenticatedError } from './errors';

describe('connectCodeName', () => {
  test('renders canonical lower_snake code names', () => {
    expect(connectCodeName(Code.Unauthenticated)).toBe('unauthenticated');
    expect(connectCodeName(Code.NotFound)).toBe('not_found');
    expect(connectCodeName(Code.PermissionDenied)).toBe('permission_denied');
    expect(connectCodeName(Code.Unavailable)).toBe('unavailable');
  });
});

describe('classifyError', () => {
  test('classifies the RTK Query connect-code-name shape', () => {
    expect(classifyError({ status: 'unauthenticated', data: 'bad token' })).toBe('unauthenticated');
    expect(classifyError({ status: 'not_found', data: 'missing' })).toBe('not_found');
    expect(classifyError({ status: 'permission_denied', data: 'no' })).toBe('forbidden');
    expect(classifyError({ status: 'unavailable', data: 'offline' })).toBe('server');
    expect(classifyError({ status: 'internal', data: 'boom' })).toBe('server');
  });

  test('classifies the RTK Query HTTP status shape', () => {
    expect(classifyError({ status: 401, data: 'expired' })).toBe('unauthenticated');
    expect(classifyError({ status: 404, data: 'gone' })).toBe('not_found');
    expect(classifyError({ status: 403, data: 'no' })).toBe('forbidden');
    expect(classifyError({ status: 503, data: 'offline' })).toBe('server');
    expect(classifyError({ status: 500, data: 'boom' })).toBe('server');
  });

  test('classifies ConnectError instances from the watch stream', () => {
    expect(classifyError(new ConnectError('expired', Code.Unauthenticated))).toBe('unauthenticated');
    expect(classifyError(new ConnectError('missing', Code.NotFound))).toBe('not_found');
    expect(classifyError(new ConnectError('offline', Code.Unavailable))).toBe('server');
    expect(classifyError(new ConnectError('no', Code.PermissionDenied))).toBe('forbidden');
  });

  test('maps unrecognized errors to unknown', () => {
    expect(classifyError({ status: 'CUSTOM_ERROR', data: 'NetworkError' })).toBe('unknown');
    expect(classifyError(new Error('unexpected'))).toBe('unknown');
    expect(classifyError('a string failure')).toBe('unknown');
    expect(classifyError(null)).toBe('unknown');
    expect(classifyError(undefined)).toBe('unknown');
  });

  test('isUnauthenticatedError matches only the auth bucket', () => {
    expect(isUnauthenticatedError({ status: 'unauthenticated', data: '' })).toBe(true);
    expect(isUnauthenticatedError(new ConnectError('expired', Code.Unauthenticated))).toBe(true);
    expect(isUnauthenticatedError({ status: 'not_found', data: '' })).toBe(false);
    expect(isUnauthenticatedError({ status: 'unavailable', data: '' })).toBe(false);
    expect(isUnauthenticatedError(undefined)).toBe(false);
  });
});