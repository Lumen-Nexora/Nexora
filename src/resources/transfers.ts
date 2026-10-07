import {
  Transfer,
  TransfersFilter,
  TransfersSort
} from '../types/transfers';
import {
  Page,
  PaginationParams,
  IterationOptions
} from '../types/pagination';
import { iterate, listAll, buildPaginationParams } from '../pagination';
import { NexoraClient } from '../client';

export class TransfersResource {
  constructor(private client: NexoraClient) {}

  async list(
    filter?: TransfersFilter,
    sort?: TransfersSort,
    params?: PaginationParams,
    signal?: AbortSignal
  ): Promise<Page<Transfer>> {
    const pagination = buildPaginationParams(params?.first, params?.after);
    const response = await this.client.request<{
      data: { transfers: Page<Transfer> };
    }>(
      '/transfers',
      {
        method: 'GET',
        params: { ...filter, ...sort, ...pagination },
        signal
      }
    );
    return response.data.transfers;
  }

  async *iterate(
    filter?: TransfersFilter,
    sort?: TransfersSort,
    initialParams?: PaginationParams,
    signal?: AbortSignal
  ): AsyncIterable<Transfer> {
    const options: IterationOptions<Transfer> = {
      fetchPage: (params, innerSignal) =>
        this.list(filter, sort, params, innerSignal),
      initialParams,
      signal
    };
    yield* iterate(options);
  }

  async listAll(
    filter?: TransfersFilter,
    sort?: TransfersSort,
    initialParams?: PaginationParams,
    signal?: AbortSignal
  ): Promise<Transfer[]> {
    const options: IterationOptions<Transfer> = {
      fetchPage: (params, innerSignal) =>
        this.list(filter, sort, params, innerSignal),
      initialParams,
      signal
    };
    return listAll(options);
  }
}
