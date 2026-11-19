import { describe, it, expect, vi } from 'vitest';
import { WalletsResource } from './wallets';
import { HttpClient, HttpResponse } from '../http';
import { CreateWalletResponse, GetBalancesResponse, TrustlineResponse } from '../types';

describe('WalletsResource', () => {
  it('creates a wallet with automatic idempotency key', async () => {
    const mockResponse: HttpResponse<CreateWalletResponse> = {
      status: 201,
      headers: new Headers(),
      data: {
        id: 'w_test_123',
        public_key: 'GABCD1234567890STUVWXYZ',
        tenant_id: 't_test_001',
        status: 'active',
        created_at: '2026-11-19T12:00:00Z',
      },
    };

    const mockHttp = {
      request: vi.fn(async () => mockResponse),
    } as unknown as HttpClient;

    const resource = new WalletsResource(mockHttp);
    const result = await resource.create();

    expect(result.id).toBe('w_test_123');
    expect(result.public_key).toBe('GABCD1234567890STUVWXYZ');
    expect(mockHttp.request).toHaveBeenCalledWith(
      expect.objectContaining({
        method: 'POST',
        path: '/wallets',
        idempotencyKey: expect.any(String),
      }),
    );
  });

  it('retrieves balances for a specific wallet', async () => {
    const mockResponse: HttpResponse<GetBalancesResponse> = {
      status: 200,
      headers: new Headers(),
      data: {
        wallet_id: 'w_test_123',
        balances: [
          {
            asset: 'XLM',
            balance: '100.5000000',
            reserved: '1.0000000',
            available: '99.5000000',
          },
          {
            asset: 'USDC',
            issuer: 'GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5',
            balance: '500.0000000',
            reserved: '0.0000000',
            available: '500.0000000',
          },
        ],
      },
    };

    const mockHttp = {
      request: vi.fn(async () => mockResponse),
    } as unknown as HttpClient;

    const resource = new WalletsResource(mockHttp);
    const result = await resource.getBalances('w_test_123');

    expect(result.wallet_id).toBe('w_test_123');
    expect(result.balances).toHaveLength(2);
    expect(result.balances[1].asset).toBe('USDC');
    expect(mockHttp.request).toHaveBeenCalledWith({
      method: 'GET',
      path: '/wallets/w_test_123/balances',
      signal: undefined,
    });
  });

  it('creates an asset trustline on Stellar', async () => {
    const mockResponse: HttpResponse<TrustlineResponse> = {
      status: 200,
      headers: new Headers(),
      data: {
        wallet_id: 'w_test_123',
        asset: 'USDC',
        issuer: 'GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5',
        limit: '1000000.0000000',
        status: 'established',
      },
    };

    const mockHttp = {
      request: vi.fn(async () => mockResponse),
    } as unknown as HttpClient;

    const resource = new WalletsResource(mockHttp);
    const result = await resource.createTrustline('w_test_123', {
      asset: 'USDC',
      issuer: 'GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5',
      limit: '1000000.0000000',
    });

    expect(result.status).toBe('established');
    expect(result.asset).toBe('USDC');
    expect(mockHttp.request).toHaveBeenCalledWith(
      expect.objectContaining({
        method: 'POST',
        path: '/wallets/w_test_123/trustlines',
        body: {
          asset: 'USDC',
          issuer: 'GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5',
          limit: '1000000.0000000',
        },
        idempotencyKey: expect.any(String),
      }),
    );
  });
});
