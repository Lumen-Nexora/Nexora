from __future__ import annotations

from typing import Any


class NexoraError(Exception):
    def __init__(
        self,
        http_status: int,
        code: str,
        message: str,
        details: Any = None,
        retry_after: float | None = None,
    ) -> None:
        super().__init__(message)
        self.http_status = http_status
        self.code = code
        self.message = message
        self.details = details
        self.retry_after = retry_after


class AuthenticationError(NexoraError):
    pass


class NotFoundError(NexoraError):
    pass


class ValidationError(NexoraError):
    pass


class ConflictError(NexoraError):
    pass


class RateLimitError(NexoraError):
    pass


def classify_error(http_status: int, body: Any, retry_after: float | None = None) -> NexoraError:
    envelope = body.get("error") if isinstance(body, dict) else None
    if isinstance(envelope, dict) and isinstance(envelope.get("code"), str) and isinstance(envelope.get("message"), str):
        details = body.get("validation_errors", envelope.get("details"))
        cls: type[NexoraError]
        cls = {
            400: ValidationError,
            401: AuthenticationError,
            404: NotFoundError,
            409: ConflictError,
            429: RateLimitError,
        }.get(http_status, NexoraError)
        return cls(http_status, envelope["code"], envelope["message"], details, retry_after)
    return NexoraError(http_status, "UNKNOWN_ERROR", f"request failed with status {http_status}", body, retry_after)