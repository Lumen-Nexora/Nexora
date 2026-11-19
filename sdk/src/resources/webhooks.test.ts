import { describe, it, expect, vi } from 'vitest';
import { WebhooksResource } from './webhooks';
import { HttpClient, HttpResponse } from '../http';
import {
  WebhookEndpointResponse,
  ListWebhooksResponse,
  ListDeliveriesResponse,
  ListWebhookSigningSecretsResponse,
  RotateWebhookSigningSecretResponse,
} from '../types';

describe('WebhooksResource', () => {
  it('registers a webhook endpoint', async () => {
    const mockResponse: HttpResponse<WebhookEndpointResponse> = {
      status: 201,
      headers: new Headers(),
      data: {
        id: 'wh_test_1',
        url: 'https://example.com/api/webhooks',
        events: ['transfer.confirmed', 'transfer.failed'],
        status: 'active',
        created_at: '2026-11-19T12:00:00Z',
      },
    };

    const mockHttp = {
      request: vi.fn(async () => mockResponse),
    } as unknown as HttpClient;

    const resource = new WebhooksResource(mockHttp);
    const result = await resource.create({
      url: 'https://example.com/api/webhooks',
      events: ['transfer.confirmed', 'transfer.failed'],
    });

    expect(result.id).toBe('wh_test_1');
    expect(result.events).toContain('transfer.confirmed');
    expect(mockHttp.request).toHaveBeenCalledWith({
      method: 'POST',
      path: '/webhooks',
      body: {
        url: 'https://example.com/api/webhooks',
        events: ['transfer.confirmed', 'transfer.failed'],
      },
      signal: undefined,
      idempotencyKey: undefined,
    });
  });

  it('lists registered webhooks', async () => {
    const mockResponse: HttpResponse<ListWebhooksResponse> = {
      status: 200,
      headers: new Headers(),
      data: {
        webhooks: [
          {
            id: 'wh_test_1',
            url: 'https://example.com/api/webhooks',
            events: ['transfer.confirmed'],
            status: 'active',
            created_at: '2026-11-19T12:00:00Z',
          },
        ],
      },
    };

    const mockHttp = {
      request: vi.fn(async () => mockResponse),
    } as unknown as HttpClient;

    const resource = new WebhooksResource(mockHttp);
    const result = await resource.list();

    expect(result.webhooks).toHaveLength(1);
    expect(result.webhooks[0].id).toBe('wh_test_1');
  });

  it('rotates signing secret with grace period', async () => {
    const mockResponse: HttpResponse<RotateWebhookSigningSecretResponse> = {
      status: 200,
      headers: new Headers(),
      data: {
        new_secret: 'whsec_new_secret_1234567890',
        active_version: 2,
        retiring_version: 1,
        retiring_secret_valid_until: '2026-11-20T12:00:00Z',
      },
    };

    const mockHttp = {
      request: vi.fn(async () => mockResponse),
    } as unknown as HttpClient;

    const resource = new WebhooksResource(mockHttp);
    const result = await resource.rotateSigningSecret({
      retiring_secret_valid_for_seconds: 86400,
    });

    expect(result.active_version).toBe(2);
    expect(result.new_secret).toBe('whsec_new_secret_1234567890');
    expect(mockHttp.request).toHaveBeenCalledWith({
      method: 'POST',
      path: '/webhooks/secret/rotate',
      body: { retiring_secret_valid_for_seconds: 86400 },
      signal: undefined,
      idempotencyKey: undefined,
    });
  });
});
