from __future__ import annotations

from typing import Any

import httpx

from .http import HTTPClient, RequestOptions
from .models import HealthResponse
from .resources import (
    FXResource,
    FeesResource,
    FiatResource,
    KeysResource,
    PaymentLinksResource,
    RefundsResource,
    SchedulesResource,
    TransfersResource,
    WalletsResource,
    WebhooksResource,
)


class NexoraClient:
    def __init__(
        self,
        api_key: str,
        *,
        base_url: str = "https://api.nexora.io",
        timeout: float = 30.0,
        max_retries: int = 0,
        retry_delay: float = 0.5,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        if not api_key:
            raise ValueError("api_key is required")
        self._http = HTTPClient(api_key, base_url, timeout, max_retries, retry_delay, transport)
        self.wallets = WalletsResource(self._http)
        self.transfers = TransfersResource(self._http)
        self.fx = FXResource(self._http)
        self.schedules = SchedulesResource(self._http)
        self.webhooks = WebhooksResource(self._http)
        self.fees = FeesResource(self._http)
        self.keys = KeysResource(self._http)
        self.fiat = FiatResource(self._http)
        self.payment_links = PaymentLinksResource(self._http)
        self.refunds = RefundsResource(self._http)

    async def health(self, options: RequestOptions | None = None) -> HealthResponse:
        return await self._http.request("GET", "/../health", options=options)

    async def aclose(self) -> None:
        await self._http.close()

    async def __aenter__(self) -> NexoraClient:
        return self

    async def __aexit__(self, *_: Any) -> None:
        await self.aclose()