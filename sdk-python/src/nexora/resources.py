from __future__ import annotations

from urllib.parse import quote

from .http import HTTPClient, RequestOptions
from .models import (
    APIKeyCreated,
    APIKeySummary,
    BatchResponse,
    ConversionResponse,
    ConvertRequest,
    CreateBatchRequest,
    CreateKeyRequest,
    CreatePaymentLinkRequest,
    CreateRefundRequest,
    CreatePaymentLinkRequest,
    CreateRefundRequest,
    CreateScheduleRequest,
    CreateTransferRequest,
    CreateTrustlineRequest,
    DepositResponse,
    FeeScheduleResponse,
    FiatDepositRequest,
    PaymentLink,
    PaymentLinksResponse,
    PaymentLink,
    FiatWithdrawalRequest,
    Quote,
    QuoteRequest,
    RateResponse,
    RegisterWebhookRequest,
    Refund,
    RefundsResponse,
    Refund,
    ScheduleResponse,
    TransferResponse,
    TrustlineResponse,
    UpdateScheduleRequest,
    WalletCreated,
    WebhookEndpointResponse,
)
from .types import (
    ListCollectedResponse,
    ListDeliveriesResponse,
    ListSchedulesResponse,
    GetBalancesResponse,
    ListTransactionsQuery,
    ListTransactionsResponse,
    ListWebhooksResponse,
    WithdrawResponse,
)


class WalletsResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def create(self, options: RequestOptions | None = None) -> WalletCreated:
        return await self._http.request("POST", "/wallets", options=options)

    async def get_balances(self, wallet_id: str, options: RequestOptions | None = None) -> GetBalancesResponse:
        return await self._http.request("GET", f"/wallets/{quote(wallet_id, safe='')}/balances", options=options)

    async def create_trustline(self, wallet_id: str, request: CreateTrustlineRequest, options: RequestOptions | None = None) -> TrustlineResponse:
        return await self._http.request("POST", f"/wallets/{quote(wallet_id, safe='')}/trustlines", body=request, options=options)


class TransfersResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def create(self, request: CreateTransferRequest, options: RequestOptions | None = None) -> TransferResponse:
        return await self._http.request("POST", "/transfers", body=request, options=options)

    async def get(self, transfer_id: str, options: RequestOptions | None = None) -> TransferResponse:
        return await self._http.request("GET", f"/transfers/{quote(transfer_id, safe='')}", options=options)

    async def list(self, query: ListTransactionsQuery, options: RequestOptions | None = None) -> ListTransactionsResponse:
        return await self._http.request("GET", "/transfers", query=query, options=options)

    async def create_batch(self, request: CreateBatchRequest, options: RequestOptions | None = None) -> BatchResponse:
        return await self._http.request("POST", "/transfers/batch", body=request, options=options)

    async def get_batch(self, batch_id: str, options: RequestOptions | None = None) -> BatchResponse:
        return await self._http.request("GET", f"/transfers/batch/{quote(batch_id, safe='')}", options=options)

    async def export_batch(self, batch_id: str, options: RequestOptions | None = None) -> str:
        return await self._http.request("GET", f"/transfers/batch/{quote(batch_id, safe='')}/export", options=options)


class FXResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def quote(self, request: QuoteRequest, options: RequestOptions | None = None) -> Quote:
        return await self._http.request("POST", "/fx/quote", body=request, options=options)

    async def convert(self, request: ConvertRequest, options: RequestOptions | None = None) -> ConversionResponse:
        return await self._http.request("POST", "/fx/convert", body=request, options=options)

    async def get_rates(self, query: dict[str, str], options: RequestOptions | None = None) -> RateResponse:
        return await self._http.request("GET", "/fx/rates", query=query, options=options)


class SchedulesResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def create(self, request: CreateScheduleRequest, options: RequestOptions | None = None) -> ScheduleResponse:
        return await self._http.request("POST", "/schedules", body=request, options=options)

    async def list(self, options: RequestOptions | None = None) -> ListSchedulesResponse:
        return await self._http.request("GET", "/schedules", options=options)

    async def update(self, schedule_id: str, request: UpdateScheduleRequest, options: RequestOptions | None = None) -> ScheduleResponse:
        return await self._http.request("PATCH", f"/schedules/{quote(schedule_id, safe='')}", body=request, options=options)

    async def delete(self, schedule_id: str, options: RequestOptions | None = None) -> None:
        await self._http.request("DELETE", f"/schedules/{quote(schedule_id, safe='')}", options=options)


class WebhooksResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def create(self, request: RegisterWebhookRequest, options: RequestOptions | None = None) -> WebhookEndpointResponse:
        return await self._http.request("POST", "/webhooks", body=request, options=options)

    async def list(self, options: RequestOptions | None = None) -> ListWebhooksResponse:
        return await self._http.request("GET", "/webhooks", options=options)

    async def delete(self, webhook_id: str, options: RequestOptions | None = None) -> None:
        await self._http.request("DELETE", f"/webhooks/{quote(webhook_id, safe='')}", options=options)

    async def get_deliveries(self, webhook_id: str, query: dict[str, int] | None = None, options: RequestOptions | None = None) -> ListDeliveriesResponse:
        return await self._http.request("GET", f"/webhooks/{quote(webhook_id, safe='')}/deliveries", query=query, options=options)


class FeesResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def get(self, options: RequestOptions | None = None) -> FeeScheduleResponse:
        return await self._http.request("GET", "/fees", options=options)

    async def list_collected(self, query: dict[str, str] | None = None, options: RequestOptions | None = None) -> ListCollectedResponse:
        return await self._http.request("GET", "/admin/fees/collected", query=query, options=options)


class KeysResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def create(self, request: CreateKeyRequest | None = None, options: RequestOptions | None = None) -> APIKeyCreated:
        return await self._http.request("POST", "/keys", body=request or {}, options=options)

    async def list(self, options: RequestOptions | None = None) -> list[APIKeySummary]:
        return await self._http.request("GET", "/keys", options=options)

    async def delete(self, key_id: str, options: RequestOptions | None = None) -> None:
        await self._http.request("DELETE", f"/keys/{quote(key_id, safe='')}", options=options)


class FiatResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def deposit(self, wallet_id: str, request: FiatDepositRequest, options: RequestOptions | None = None) -> DepositResponse:
        return await self._http.request("POST", f"/wallets/{quote(wallet_id, safe='')}/deposit/fiat", body=request, options=options)

    async def withdraw(self, wallet_id: str, request: FiatWithdrawalRequest, options: RequestOptions | None = None) -> WithdrawResponse:
        return await self._http.request("POST", f"/wallets/{quote(wallet_id, safe='')}/withdraw/fiat", body=request, options=options)


class PaymentLinksResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def create(self, request: CreatePaymentLinkRequest, options: RequestOptions | None = None) -> PaymentLink:
        return await self._http.request("POST", "/payment-links", body=request, options=options)

    async def list(self, options: RequestOptions | None = None) -> PaymentLinksResponse:
        return await self._http.request("GET", "/payment-links", options=options)

    async def get(self, link_id: str, options: RequestOptions | None = None) -> PaymentLink:
        return await self._http.request("GET", f"/payment-links/{quote(link_id, safe='')}", options=options)

    async def cancel(self, link_id: str, options: RequestOptions | None = None) -> None:
        await self._http.request("DELETE", f"/payment-links/{quote(link_id, safe='')}", options=options)


class RefundsResource:
    def __init__(self, http: HTTPClient) -> None:
        self._http = http

    async def create(self, request: CreateRefundRequest, options: RequestOptions | None = None) -> Refund:
        return await self._http.request("POST", "/refunds", body=request, options=options)

    async def get(self, refund_id: str, options: RequestOptions | None = None) -> Refund:
        return await self._http.request("GET", f"/refunds/{quote(refund_id, safe='')}", options=options)

    async def list(self, original_transaction_id: str, options: RequestOptions | None = None) -> RefundsResponse:
        return await self._http.request("GET", "/refunds", query={"original_transaction_id": original_transaction_id}, options=options)