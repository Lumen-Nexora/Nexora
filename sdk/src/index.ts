export { NexoraClient } from './client';
export type { NexoraClientConfig } from './client';
export type { RequestOptions } from './http';

export {
  NexoraError,
  AuthenticationError,
  NotFoundError,
  ValidationError,
  RateLimitError,
  ConflictError,
  RepeatedCursorError,
} from './errors';

export { createPage, paginate, paginatePages, paginateAll } from './pagination';
export type { Page, PageFetcher, PaginateOptions } from './pagination';

export type {
  // Wallet
  CreateWalletResponse,
  Balance,
  GetBalancesResponse,
  CreateTrustlineRequest,
  TrustlineResponse,
  // Transfer
  TransactionStatus,
  TransactionType,
  CreateTransferRequest,
  TransferResponse,
  ListTransactionsQuery,
  ListTransactionsResponse,
  // Batch
  BatchStatus,
  BatchItemRequest,
  CreateBatchRequest,
  BatchTransferResponse,
  BatchResponse,
  // FX
  QuoteRequest,
  QuoteResponse,
  ConvertRequest,
  ConversionResponse,
  GetRatesQuery,
  RateResponse,
  // Fees
  FeeScheduleResponse,
  FeeCollectionSummary,
  TenantFeeTotal,
  ListCollectedQuery,
  ListCollectedResponse,
  // Fiat
  DepositRequest,
  DepositResponse,
  WithdrawRequest,
  WithdrawResponse,
  CreatePaymentLinkRequest,
  PaymentLinkResponse,
  PaymentLinksResponse,
  CreateRefundRequest,
  RefundResponse,
  RefundsResponse,
  // Webhook
  EventType,
  DeliveryStatus,
  RegisterWebhookRequest,
  WebhookEndpointResponse,
  ListWebhooksResponse,
  WebhookDeliveryResponse,
  ListDeliveriesResponse,
  WebhookSigningSecretStatus,
  WebhookSigningSecretMetadata,
  ListWebhookSigningSecretsResponse,
  RotateWebhookSigningSecretRequest,
  RotateWebhookSigningSecretResponse,
  // Schedule
  ScheduleFrequency,
  ScheduleStatus,
  CreateScheduleRequest,
  UpdateScheduleRequest,
  ScheduleResponse,
  ListSchedulesResponse,
  ScheduleRunStatus,
  ScheduleRunResponse,
  ListScheduleRunsResponse,
  // API Key
  APIKeyScope,
  APIKeyScopeResource,
  CreateKeyRequest,
  CreateKeyResponse,
  APIKeyResponse,
  // Health
  HealthResponse,
} from './types';

// Re-export resource classes for advanced usage
export { WalletsResource } from './resources/wallets';
export { TransfersResource } from './resources/transfers';
export { FXResource } from './resources/fx';
export { SchedulesResource } from './resources/schedules';
export { WebhooksResource } from './resources/webhooks';
export { FeesResource } from './resources/fees';
export { KeysResource } from './resources/keys';
export { FiatResource } from './resources/fiat';
export { PaymentLinksResource } from './resources/payment_links';
export { RefundsResource } from './resources/refunds';
