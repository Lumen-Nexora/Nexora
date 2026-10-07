import { describe, it, expect } from 'vitest';
import {
  classifyError,
  AuthenticationError,
  NotFoundError,
  ConflictError,
  PermissionError,
  RateLimitError,
  ValidationError,
  NexoraError,
} from './errors';

// Helpers to build the envelope the API actually returns
function envelope(code: string, message: string) {
  return { error: { code, message } };
}

function validationEnvelope(message: string, validationErrors: unknown[]) {
  return {
    error: { code: 'BAD_REQUEST', message },
    validation_errors: validationErrors,
  };
}

describe('classifyError', () => {
  it('returns NotFoundError with the server message on 404', () => {
    const err = classifyError(404, envelope('NOT_FOUND', 'wallet not found'));
    expect(err).toBeInstanceOf(NotFoundError);
    expect(err.message).toBe('wallet not found');
    expect(err.code).toBe('NOT_FOUND');
    expect(err.statusCode).toBe(404);
  });

  it('returns ValidationError with the server message on 400', () => {
    const err = classifyError(
      400,
      envelope('BAD_REQUEST', 'source and destination wallets must differ'),
    );
    expect(err).toBeInstanceOf(ValidationError);
    expect(err.message).toBe('source and destination wallets must differ');
    expect(err.code).toBe('BAD_REQUEST');
  });

  it('attaches validation_errors as details on 400', () => {
    const rows = [{ row: 1, field: 'amount', value: '-1', reason: 'must be positive' }];
    const err = classifyError(400, validationEnvelope('invalid rows', rows));
    expect(err).toBeInstanceOf(ValidationError);
    expect(err.details).toEqual(rows);
  });

  it('returns ConflictError with the server message on 409', () => {
    const err = classifyError(
      409,
      envelope('REVIEW_ALREADY_DECIDED', 'compliance review has already been decided'),
    );
    expect(err).toBeInstanceOf(ConflictError);
    expect(err.message).toBe('compliance review has already been decided');
    expect(err.code).toBe('REVIEW_ALREADY_DECIDED');
    expect(err.statusCode).toBe(409);
  });

  it('returns RateLimitError with the server message on 429', () => {
    const err = classifyError(429, envelope('RATE_LIMITED', 'request quota exceeded'));
    expect(err).toBeInstanceOf(RateLimitError);
    expect(err.message).toBe('request quota exceeded');
    expect(err.code).toBe('RATE_LIMITED');
    expect(err.statusCode).toBe(429);
  });

  it('returns AuthenticationError with the server message on 401', () => {
    const err = classifyError(401, envelope('UNAUTHORIZED', 'invalid api key'));
    expect(err).toBeInstanceOf(AuthenticationError);
    expect(err.message).toBe('invalid api key');
    expect(err.statusCode).toBe(401);
  });

  it('preserves the response request ID and embedded status', () => {
    const err = classifyError(401, {
      error: {
        code: 'UNAUTHORIZED',
        message: 'invalid api key',
        status: 401,
        request_id: 'req-sdk-test',
      },
    });
    expect(err.statusCode).toBe(401);
    expect(err.requestId).toBe('req-sdk-test');
  });

  it('returns a plain NexoraError with code and message on 422', () => {
    const err = classifyError(422, envelope('QUOTE_EXPIRED', 'quote has expired'));
    expect(err).toBeInstanceOf(NexoraError);
    expect(err.message).toBe('quote has expired');
    expect(err.code).toBe('QUOTE_EXPIRED');
    expect(err.statusCode).toBe(422);
  });

  it('returns UNKNOWN_ERROR when the envelope is missing', () => {
    const err = classifyError(503, { something: 'unexpected' });
    expect(err.code).toBe('UNKNOWN_ERROR');
    expect(err.statusCode).toBe(503);
  });

  it('returns UNKNOWN_ERROR when body is not an object', () => {
    const err = classifyError(500, null);
    expect(err.code).toBe('UNKNOWN_ERROR');
  });

  it('returns PermissionError naming the missing scope on 403 INSUFFICIENT_SCOPE', () => {
    const err = classifyError(
      403,
      envelope('INSUFFICIENT_SCOPE', 'API key does not have the required scope: transfers:write'),
    );
    expect(err).toBeInstanceOf(PermissionError);
    expect(err).toBeInstanceOf(NexoraError);
    expect(err.code).toBe('INSUFFICIENT_SCOPE');
    expect(err.statusCode).toBe(403);
    expect((err as PermissionError).requiredScope).toBe('transfers:write');
  });

  it('keeps other 403s as a plain NexoraError', () => {
    const err = classifyError(403, envelope('TRANSFER_BLOCKED_SANCTIONS', 'transfer blocked'));
    expect(err).not.toBeInstanceOf(PermissionError);
    expect(err.statusCode).toBe(403);
    expect(err.code).toBe('TRANSFER_BLOCKED_SANCTIONS');
  });
});
