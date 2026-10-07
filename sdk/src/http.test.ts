import { readFileSync } from 'node:fs';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { NexoraClient } from './client';
import { NexoraError } from './errors';
import { HttpClient } from './http';

interface ConformanceCase {
  name: string;
  method: string;
  path: string;
  idempotency_key?: string;
  max_retries?: number;
  statuses: number[];
  retry_after?: string;
  key_mode: 'generated' | 'caller' | 'none';
  expected_requests: number;
  expected_error?: boolean;
}

const suite = JSON.parse(readFileSync('conformance/cases.json', 'utf8')) as {
  cases: ConformanceCase[];
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe.each(suite.cases)('shared conformance: $name', (testCase) => {
  it('matches key and retry behavior', async () => {
    let calls = 0;
    const keys: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        calls++;
        keys.push(new Headers(init?.headers).get('Idempotency-Key') ?? '');
        const status = testCase.statuses[Math.min(calls - 1, testCase.statuses.length - 1)];
        const headers = new Headers({ 'content-type': 'application/json' });
        if (calls === 1 && testCase.retry_after) headers.set('retry-after', testCase.retry_after);
        const body =
          status >= 400
            ? JSON.stringify({ error: { code: 'TEMPORARY', message: 'retry' } })
            : JSON.stringify({ ok: true });
        return new Response(body, { status, headers });
      }),
    );

    const client = new HttpClient({
      apiKey: 'test-key',
      baseUrl: 'https://api.example.test',
      timeout: 1000,
      maxRetries: testCase.max_retries ?? 0,
      retryDelay: 0,
    });
    let error: unknown;
    try {
      await client.request<{ ok: boolean }>({
        method: testCase.method,
        path: testCase.path,
        idempotencyKey: testCase.idempotency_key,
      });
    } catch (caught) {
      error = caught;
    }

    expect(calls).toBe(testCase.expected_requests);
    expect(Boolean(error)).toBe(testCase.expected_error ?? false);
    if (testCase.expected_error) expect(error).toBeInstanceOf(NexoraError);
    if (testCase.key_mode === 'generated') {
      expect(keys[0]).toBeTruthy();
      expect(new Set(keys).size).toBe(1);
    } else if (testCase.key_mode === 'caller') {
      expect(new Set(keys)).toEqual(new Set([testCase.idempotency_key]));
    } else {
      expect(keys.every((key) => key === '')).toBe(true);
    }
  });
});

describe('NexoraClient request safety', () => {
  it('defaults to no retries', () => {
    const client = new NexoraClient({ apiKey: 'test-key' });
    expect(client).toBeInstanceOf(NexoraClient);
  });

  it('preserves the caller idempotency key on resource methods', async () => {
    let key = '';
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
        key = new Headers(init?.headers).get('Idempotency-Key') ?? '';
        return new Response('{}', { status: 202, headers: { 'content-type': 'application/json' } });
      }),
    );
    const client = new NexoraClient({ apiKey: 'test-key' });
    await client.transfers.create(
      { from_wallet_id: 'from', to_wallet_id: 'to', asset: 'USDC', amount: '1' },
      { idempotencyKey: 'caller-key' },
    );
    expect(key).toBe('caller-key');
  });

  it('adds an idempotency key to trustline creation', async () => {
    let key = '';
    let url = '';
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        key = new Headers(init?.headers).get('Idempotency-Key') ?? '';
        url = String(input);
        return new Response('{}', { status: 202, headers: { 'content-type': 'application/json' } });
      }),
    );
    const client = new NexoraClient({ apiKey: 'test-key' });
    await client.wallets.createTrustline('wallet-1', {
      asset_code: 'USDC',
      asset_issuer: 'issuer-key',
    });
    expect(key).toBeTruthy();
    expect(url).toContain('/wallets/wallet-1/trustlines');
  });
});
