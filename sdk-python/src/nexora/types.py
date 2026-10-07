from __future__ import annotations

from typing import Required, TypedDict

from .models import (
    APIKeyCreated,
    APIKeySummary,
    BatchResponse,
    Balance,
    ConversionResponse,
    CreateBatchRequest,
    CreateKeyRequest,
    CreatePaymentLinkRequest,
    CreateRefundRequest,
    CreateScheduleRequest,
    CreateTransferRequest,
    CreateTrustlineRequest,
    FeeCollectionSummary,
    FeeScheduleResponse,
    FiatDepositRequest,
    FiatWithdrawalRequest,
    PaymentLink,
    HealthResponse,
    Quote,
    QuoteRequest,
    RateResponse,
    RegisterWebhookRequest,
    Refund,
    ScheduleResponse,
    TransferResponse,
    UpdateScheduleRequest,
    WalletCreated,
    WalletBalances,
    WebhookDelivery,
    WebhookEndpointResponse,
    WebhookEndpointSummary,
    WithdrawalResponse,
)

QuoteResponse = Quote
DepositRequest = FiatDepositRequest
WithdrawRequest = FiatWithdrawalRequest
WithdrawResponse = WithdrawalResponse
CreateWalletResponse = WalletCreated
GetBalancesResponse = WalletBalances


class ListTransactionsQuery(TypedDict, total=False):
    wallet_id: Required[str]
    limit: int
    offset: int


class ListTransactionsResponse(TypedDict):
    transactions: list[TransferResponse]


class ListSchedulesResponse(TypedDict):
    schedules: list[ScheduleResponse]


class ListWebhooksResponse(TypedDict):
    endpoints: list[WebhookEndpointSummary]


class ListDeliveriesResponse(TypedDict):
    deliveries: list[WebhookDelivery]


class ListCollectedResponse(TypedDict):
    summary: list[FeeCollectionSummary]


class ListPaymentLinksResponse(TypedDict):
    payment_links: list[PaymentLink]


class ListRefundsResponse(TypedDict):
    refunds: list[Refund]