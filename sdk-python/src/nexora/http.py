from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
import secrets
import uuid
import time
from typing import Any

import httpx

from .errors import NexoraError, classify_error


@dataclass(frozen=True)
class RequestOptions:
    idempotency_key: str | None = None
    timeout: float | None = None
    headers: dict[str, str] = field(default_factory=dict)


class HTTPClient:
    def __init__(
        self,
        api_key: str,
        base_url: str,
        timeout: float,
        max_retries: int,
        retry_delay: float,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/").removesuffix("/v1")
        self.timeout = timeout
        self.max_retries = max(0, max_retries)
        self.retry_delay = max(0.0, retry_delay)
        self.session = httpx.AsyncClient(
            timeout=timeout,
            transport=transport,
            headers={"Authorization": f"Bearer {api_key}", "Accept": "application/json"},
        )

    async def close(self) -> None:
        await self.session.aclose()

    async def request(
        self,
        method: str,
        path: str,
        *,
        body: Any = None,
        query: dict[str, str | int | None] | None = None,
        options: RequestOptions | None = None,
    ) -> Any:
        options = options or RequestOptions()
        headers = dict(options.headers)
        key = options.idempotency_key or _header(headers, "Idempotency-Key") or _header(headers, "X-Idempotency-Key")
        if not key and method.upper() == "POST" and idempotency_required(path):
            key = str(uuid.uuid4())
        if key and not _header(headers, "Idempotency-Key"):
            headers["Idempotency-Key"] = key

        safe_to_retry = method.upper() in {"GET", "HEAD"} or bool(key)
        url = self.base_url + path if path != "/../health" else self.base_url + "/health"
        last_error: Exception | None = None

        for attempt in range(self.max_retries + 1):
            try:
                response = await self.session.request(
                    method,
                    url,
                    params=query,
                    json=body,
                    headers=headers,
                    timeout=options.timeout or self.timeout,
                )
            except httpx.RequestError as exc:
                last_error = exc
                if safe_to_retry and attempt < self.max_retries:
                    await asyncio.sleep(self.retry_delay * (2**attempt))
                    continue
                raise NexoraError(0, "NETWORK_ERROR", str(exc)) from exc

            if response.status_code == 204:
                return None
            content_type = response.headers.get("content-type", "")
            if "json" in content_type:
                try:
                    data: Any = response.json()
                except ValueError:
                    data = response.text
            else:
                data = response.text

            if response.is_error:
                retry_after = parse_retry_after(response.headers.get("retry-after"))
                error = classify_error(response.status_code, data, retry_after)
                if safe_to_retry and _retryable_status(response.status_code) and attempt < self.max_retries:
                    delay = retry_after if retry_after is not None else self.retry_delay * (2**attempt)
                    await asyncio.sleep(delay)
                    continue
                raise error
            return data

        raise NexoraError(0, "NETWORK_ERROR", str(last_error or "request failed"))


def idempotency_required(path: str) -> bool:
    if path in {"/wallets", "/transfers", "/transfers/batch", "/withdrawals", "/fx/convert", "/schedules", "/claimable-balances", "/payment-links", "/refunds"}:
        return True
    return path.startswith("/wallets/") and path.endswith(("/deposit/fiat", "/withdraw/fiat", "/trustlines")) or path.startswith("/claimable-balances/") and path.endswith("/claim")


def parse_retry_after(value: str | None) -> float | None:
    if not value:
        return None
    try:
        return max(0.0, float(value))
    except ValueError:
        try:
            when = parsedate_to_datetime(value)
            if when.tzinfo is None:
                when = when.replace(tzinfo=timezone.utc)
            return max(0.0, when.timestamp() - time.time())
        except (TypeError, ValueError, OverflowError):
            return None


def _header(headers: dict[str, str], name: str) -> str | None:
    return next((value for key, value in headers.items() if key.lower() == name.lower()), None)


def _retryable_status(status: int) -> bool:
    return status == 429 or status >= 500