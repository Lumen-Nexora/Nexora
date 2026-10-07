import { HttpClient, HttpClientConfig } from './http';
import { WalletsResource } from './resources/wallets';
import { TransfersResource } from './resources/transfers';
import { FXResource } from './resources/fx';
import { SchedulesResource } from './resources/schedules';
import { WebhooksResource } from './resources/webhooks';
import { FeesResource } from './resources/fees';
import { KeysResource } from './resources/keys';
import { FiatResource } from './resources/fiat';
import { PaymentLinksResource } from './resources/payment_links';
import { RefundsResource } from './resources/refunds';

export interface NexoraClientConfig {
  apiKey: string;
  baseUrl?: string;
  timeout?: number;
  maxRetries?: number;
  retryDelay?: number;
  /**
   * Default idempotency key to use for financial mutations when the caller does not
   * supply one. Useful for cross-process retries where the key must be stable.
   */
  idempotencyKey?: string;
  /**
   * When true, requests without an idempotency key will throw before being sent.
   * Defaults to false.
   */
  requireIdempotencyKey?: boolean;
}

const DEFAULT_BASE_URL = 'https://api.nexora.io';
const DEFAULT_TIMEOUT = 30_000;
const DEFAULT_MAX_RETRIES = 0;
const DEFAULT_RETRY_DELAY = 500;

export class NexoraClient {
  readonly wallets: WalletsResource;
  readonly transfers: TransfersResource;
  readonly fx: FXResource;
  readonly schedules: SchedulesResource;
  readonly webhooks: WebhooksResource;
  readonly fees: FeesResource;
  readonly keys: KeysResource;
  readonly fiat: FiatResource;
  readonly paymentLinks: PaymentLinksResource;
  readonly refunds: RefundsResource;

  private http: HttpClient;

  constructor(config: NexoraClientConfig) {
    if (!config.apiKey) {
      throw new Error('apiKey is required');
    }

    const httpConfig: HttpClientConfig = {
      baseUrl: config.baseUrl ?? DEFAULT_BASE_URL,
      apiKey: config.apiKey,
      timeout: config.timeout && config.timeout > 0 ? config.timeout : DEFAULT_TIMEOUT,
      maxRetries: Math.max(0, config.maxRetries ?? DEFAULT_MAX_RETRIES),
      retryDelay: Math.max(0, config.retryDelay ?? DEFAULT_RETRY_DELAY),
      idempotencyKey: config.idempotencyKey,
      requireIdempotencyKey: config.requireIdempotencyKey ?? false,
    };

    this.http = new HttpClient(httpConfig);
    this.wallets = new WalletsResource(this.http);
    this.transfers = new TransfersResource(this.http);
    this.fx = new FXResource(this.http);
    this.schedules = new SchedulesResource(this.http);
    this.webhooks = new WebhooksResource(this.http);
    this.fees = new FeesResource(this.http);
    this.keys = new KeysResource(this.http);
    this.fiat = new FiatResource(this.http);
    this.paymentLinks = new PaymentLinksResource(this.http);
    this.refunds = new RefundsResource(this.http);
  }

  async health(options?: { signal?: AbortSignal }): Promise<{
    status: string;
    services?: Record<string, string>;
  }> {
    const res = await this.http.request<{
      status: string;
      services?: Record<string, string>;
    }>({
      method: 'GET',
      path: '/../health',
      signal: options?.signal,
    });
    return res.data;
  }
}
