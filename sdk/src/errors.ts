export interface NexoraErrorBody {
  code: string;
  message: string;
  status?: number;
  request_id?: string;
  details?: unknown;
}

export class NexoraError extends Error {
  readonly statusCode: number;
  readonly code: string;
  readonly requestId?: string;
  readonly details?: unknown;

  constructor(statusCode: number, body: NexoraErrorBody) {
    super(body.message);
    this.name = 'NexoraError';
    this.statusCode = body.status ?? statusCode;
    this.code = body.code;
    this.requestId = body.request_id;
    this.details = body.details;
  }
}

export class AuthenticationError extends NexoraError {
  constructor(body: NexoraErrorBody) {
    super(401, body);
    this.name = 'AuthenticationError';
  }
}

export class NotFoundError extends NexoraError {
  constructor(body: NexoraErrorBody) {
    super(404, body);
    this.name = 'NotFoundError';
  }
}

export class ValidationError extends NexoraError {
  constructor(body: NexoraErrorBody) {
    super(400, body);
    this.name = 'ValidationError';
  }
}

export class RateLimitError extends NexoraError {
  retryAfter?: number;

  constructor(body: NexoraErrorBody, retryAfter?: number) {
    super(429, body);
    this.name = 'RateLimitError';
    this.retryAfter = retryAfter;
  }
}

/**
 * 403 raised when the API key lacks a scope the operation needs
 * (error code `INSUFFICIENT_SCOPE`). `requiredScope` names the missing scope.
 */
export class PermissionError extends NexoraError {
  readonly requiredScope?: string;

  constructor(body: NexoraErrorBody) {
    super(403, body);
    this.name = 'PermissionError';
    const match = /required scope: (\S+)/.exec(body.message);
    this.requiredScope = match?.[1];
  }
}

export class ConflictError extends NexoraError {
  constructor(body: NexoraErrorBody) {
    super(409, body);
    this.name = 'ConflictError';
  }
}

export class RepeatedCursorError extends NexoraError {
  readonly cursor: string;

  constructor(cursor: string) {
    super(0, {
      code: 'REPEATED_CURSOR',
      message: `Repeated cursor detected: "${cursor}". Halting pagination to prevent an infinite loop.`,
    });
    this.name = 'RepeatedCursorError';
    this.cursor = cursor;
  }
}

export function classifyError(status: number, body: unknown): NexoraError {
  // The API wraps errors as { "error": { "code": "...", "message": "..." } }
  // with an optional top-level "validation_errors" array for 400s.
  const envelope = body as { error?: NexoraErrorBody; validation_errors?: unknown };
  const detail = envelope?.error;

  if (typeof detail?.code === 'string' && typeof detail?.message === 'string') {
    const parsed: NexoraErrorBody = {
      code: detail.code,
      message: detail.message,
      status: detail.status ?? status,
      request_id: detail.request_id,
      details: envelope.validation_errors ?? detail.details,
    };

    switch (status) {
      case 400:
        return new ValidationError(parsed);
      case 401:
        return new AuthenticationError(parsed);
      case 403:
        return parsed.code === 'INSUFFICIENT_SCOPE'
          ? new PermissionError(parsed)
          : new NexoraError(status, parsed);
      case 404:
        return new NotFoundError(parsed);
      case 409:
        return new ConflictError(parsed);
      case 422:
        return new NexoraError(status, parsed);
      case 429:
        return new RateLimitError(parsed);
      default:
        return new NexoraError(status, parsed);
    }
  }

  return new NexoraError(status, {
    code: 'UNKNOWN_ERROR',
    message: `Request failed with status ${status}`,
  });
}
