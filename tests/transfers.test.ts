import { describe, it, expect, vi, beforeEach } from 'vitest';
import { TransfersResource } from '../src/resources/transfers';
import { NexoraClient } from '../src/client';
import type { Transfer, TransfersFilter } from '../src/types/transfers';

const mockClient = {
  request: vi.fn()
} as unknown as NexoraClient;

const createMockResponse = (transfers: Transfer[], hasNextPage: boolean, endCursor?: string) => ({
  data: {
    transfers: {
      nodes: transfers,
      pageInfo: {
        hasNextPage,
        endCursor: endCursor ?? (transfers.length > 0 ? transfers[transfers.length - 1].id : null)
      }
    }
  }
});

describe('TransfersResource', () => {
  let transfers: TransfersResource;

  beforeEach(() => {
    vi.clearAllMocks();
    transfers = new TransfersResource(mockClient);
  });

  describe('list', () => {
    it('should return a single page of transfers', async () => {
      const mockTransfers = [
        { id: '1', amount: '100', asset: 'USDC', from: 'A', to: 'B', timestamp: '2023-01-01', transactionHash: '0x1' }
      ];
      mockClient.request.mockResolvedValue(createMockResponse(mockTransfers, false));

      const result = await transfers.list();
      expect(result.nodes).toEqual(mockTransfers);
      expect(result.pageInfo.hasNextPage).toBe(false);
    });

    it('should pass pagination params', async () => {
      const mockTransfers = [
        { id: '1', amount: '100', asset: 'USDC', from: 'A', to: 'B', timestamp: '2023-01-01', transactionHash: '0x1' }
      ];
      mockClient.request.mockResolvedValue(createMockResponse(mockTransfers, false));

      await transfers.list(undefined, undefined, { first: 50, after: 'cursor123' });
      expect(mockClient.request).toHaveBeenCalledWith(
        '/transfers',
        expect.objectContaining({
          method: 'GET',
          params: expect.objectContaining({
            first: 50,
            after: 'cursor123'
          })
        })
      );
    });

    it('should pass filter and sort params', async () => {
      const filter: TransfersFilter = { from: 'A', asset: 'USDC' };
      const sort = { orderBy: 'timestamp', orderDirection: 'desc' };
      const mockTransfers = [
        { id: '1', amount: '100', asset: 'USDC', from: 'A', to: 'B', timestamp: '2023-01-01', transactionHash: '0x1' }
      ];
      mockClient.request.mockResolvedValue(createMockResponse(mockTransfers, false));

      await transfers.list(filter, sort);
      expect(mockClient.request).toHaveBeenCalledWith(
        '/transfers',
        expect.objectContaining({
          method: 'GET',
          params: expect.objectContaining({
            ...filter,
            ...sort
          })
        })
      );
    });
  });

  describe('iterate', () => {
    it('should iterate over multiple pages', async () => {
      const page1 = [
        { id: '1', amount: '100', asset: 'USDC', from: 'A', to: 'B', timestamp: '2023-01-01', transactionHash: '0x1' }
      ];
      const page2 = [
        { id: '2', amount: '200', asset: 'USDC', from: 'A', to: 'C', timestamp: '2023-01-02', transactionHash: '0x2' }
      ];

      mockClient.request
        .mockResolvedValueOnce(createMockResponse(page1, true, '1'))
        .mockResolvedValueOnce(createMockResponse(page2, false, '2'));

      const items: Transfer[] = [];
      for await (const item of transfers.iterate()) {
        items.push(item);
      }

      expect(items).toEqual([...page1, ...page2]);
      expect(mockClient.request).toHaveBeenCalledTimes(2);
    });

    it('should stop on abort signal', async () => {
      const page1 = [
        { id: '1', amount: '100', asset: 'USDC', from: 'A', to: 'B', timestamp: '2023-01-01', transactionHash: '0x1' }
      ];
      const page2 = [
        { id: '2', amount: '200', asset: 'USDC', from: 'A', to: 'C', timestamp: '2023-01-02', transactionHash: '0x2' }
      ];

      mockClient.request
        .mockResolvedValueOnce(createMockResponse(page1, true, '1'))
        .mockResolvedValueOnce(createMockResponse(page2, false, '2'));

      const controller = new AbortController();
      const items: Transfer[] = [];

      setTimeout(() => controller.abort(), 0);

      try {
        for await (const item of transfers.iterate(undefined, undefined, undefined, controller.signal)) {
          items.push(item);
        }
        expect.fail('Should have thrown');
      } catch (error) {
        expect((error as Error).name).toBe('CursorPaginationError');
      }

      expect(mockClient.request).toHaveBeenCalledTimes(1);
    });
  });

  describe('listAll', () => {
    it('should return all items across pages', async () => {
      const page1 = [
        { id: '1', amount: '100', asset: 'USDC', from: 'A', to: 'B', timestamp: '2023-01-01', transactionHash: '0x1' }
      ];
      const page2 = [
        { id: '2', amount: '200', asset: 'USDC', from: 'A', to: 'C', timestamp: '2023-01-02', transactionHash: '0x2' }
      ];

      mockClient.request
        .mockResolvedValueOnce(createMockResponse(page1, true, '1'))
        .mockResolvedValueOnce(createMockResponse(page2, false, '2'));

      const result = await transfers.listAll();
      expect(result).toEqual([...page1, ...page2]);
    });
  });
});
