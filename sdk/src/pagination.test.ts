import { describe, it, expect, vi } from 'vitest';
import { createPage, paginate, paginateAll, paginatePages, Page, PageFetcher } from './pagination';
import { RepeatedCursorError, NexoraError } from './errors';
import { RequestOptions } from './http';

describe('createPage helper', () => {
  it('creates a standard page with valid nextCursor', () => {
    const page = createPage(['item1', 'item2'], 'cursor_123');
    expect(page.items).toEqual(['item1', 'item2']);
    expect(page.nextCursor).toBe('cursor_123');
    expect(page.next_cursor).toBe('cursor_123');
    expect(page.hasNextPage).toBe(true);
  });

  it('creates a terminal page when nextCursor is null or whitespace', () => {
    const page1 = createPage(['item1'], null);
    expect(page1.nextCursor).toBeNull();
    expect(page1.hasNextPage).toBe(false);

    const page2 = createPage(['item1'], '   ');
    expect(page2.nextCursor).toBeNull();
    expect(page2.hasNextPage).toBe(false);
  });

  it('handles empty items array gracefully', () => {
    const page = createPage([]);
    expect(page.items).toEqual([]);
    expect(page.nextCursor).toBeNull();
    expect(page.hasNextPage).toBe(false);
  });
});

describe('Cursor pagination generator and helpers', () => {
  it('handles zero pages (empty results)', async () => {
    const fetchPage = vi.fn<PageFetcher<string>>(async () => createPage([], null));

    const items: string[] = [];
    for await (const item of paginate({ fetchPage })) {
      items.push(item);
    }

    expect(items).toEqual([]);
    expect(fetchPage).toHaveBeenCalledTimes(1);

    const all = await paginateAll({ fetchPage });
    expect(all).toEqual([]);
  });

  it('handles single page response without further pages', async () => {
    const fetchPage = vi.fn<PageFetcher<number>>(async () => createPage([10, 20, 30], null));

    const items: number[] = [];
    for await (const num of paginate({ fetchPage })) {
      items.push(num);
    }

    expect(items).toEqual([10, 20, 30]);
    expect(fetchPage).toHaveBeenCalledTimes(1);

    const all = await paginateAll({ fetchPage });
    expect(all).toEqual([10, 20, 30]);
  });

  it('iterates across multiple pages preserving items order', async () => {
    const pages: Record<string, Page<string>> = {
      initial: createPage(['t1', 't2'], 'cur_1'),
      cur_1: createPage(['t3', 't4'], 'cur_2'),
      cur_2: createPage(['t5'], null),
    };

    const fetchPage = vi.fn<PageFetcher<string, { cursor?: string; wallet_id: string }>>(
      async (query) => {
        const key = query.cursor ?? 'initial';
        return pages[key];
      },
    );

    const items: string[] = [];
    for await (const tx of paginate({
      fetchPage,
      query: { wallet_id: 'w_abc' },
    })) {
      items.push(tx);
    }

    expect(items).toEqual(['t1', 't2', 't3', 't4', 't5']);
    expect(fetchPage).toHaveBeenCalledTimes(3);

    // Also verify paginatePages yields typed Page objects
    const pageList: Page<string>[] = [];
    for await (const page of paginatePages({
      fetchPage,
      query: { wallet_id: 'w_abc' },
    })) {
      pageList.push(page);
    }
    expect(pageList.length).toBe(3);
    expect(pageList[0].items).toEqual(['t1', 't2']);
    expect(pageList[1].items).toEqual(['t3', 't4']);
    expect(pageList[2].items).toEqual(['t5']);
  });

  it('preserves all original filters, sort orders, and parameters across pages', async () => {
    const queryLog: Record<string, unknown>[] = [];
    const fetchPage = vi.fn<PageFetcher<string, Record<string, unknown>>>(async (query) => {
      queryLog.push({ ...query });
      if (query.cursor === 'cur_1') {
        return createPage(['b'], null);
      }
      return createPage(['a'], 'cur_1');
    });

    const initialQuery = {
      wallet_id: 'w_main',
      limit: 50,
      order: 'desc',
      external_reference: 'ref_999',
      tag: 'payroll',
    };

    const all = await paginateAll({
      fetchPage,
      query: initialQuery,
    });

    expect(all).toEqual(['a', 'b']);
    expect(queryLog).toHaveLength(2);
    expect(queryLog[0]).toEqual(initialQuery);
    expect(queryLog[1]).toEqual({
      ...initialQuery,
      cursor: 'cur_1',
    });
  });

  it('supports custom cursorParam name', async () => {
    const queryLog: Record<string, unknown>[] = [];
    const fetchPage = vi.fn<PageFetcher<string, Record<string, unknown>>>(async (query) => {
      queryLog.push({ ...query });
      if (query.after === 'tok_2') {
        return createPage(['z'], null);
      }
      return createPage(['y'], 'tok_2');
    });

    const all = await paginateAll({
      fetchPage,
      query: { size: 10 },
      cursorParam: 'after',
    });

    expect(all).toEqual(['y', 'z']);
    expect(queryLog[0]).toEqual({ size: 10 });
    expect(queryLog[1]).toEqual({ size: 10, after: 'tok_2' });
  });

  it('stops fetching additional pages when breaking out of iteration early', async () => {
    let callCount = 0;
    const fetchPage = vi.fn<PageFetcher<number>>(async () => {
      callCount++;
      return createPage([1, 2, 3], `cursor_${callCount}`);
    });

    const collected: number[] = [];
    for await (const item of paginate({ fetchPage })) {
      collected.push(item);
      if (collected.length === 2) {
        break; // Break early on page 1
      }
    }

    expect(collected).toEqual([1, 2]);
    expect(fetchPage).toHaveBeenCalledTimes(1);
    expect(callCount).toBe(1);
  });

  it('throws AbortError when AbortSignal is already aborted', async () => {
    const fetchPage = vi.fn<PageFetcher<string>>(async () => createPage(['never_called'], null));

    const controller = new AbortController();
    controller.abort();

    await expect(async () => {
      // eslint-disable-next-line @typescript-eslint/no-unused-vars
      for await (const _ of paginate({
        fetchPage,
        options: { signal: controller.signal },
      })) {
        // empty
      }
    }).rejects.toThrow();

    expect(fetchPage).not.toHaveBeenCalled();
  });

  it('cancels iteration and stops fetching when signal is aborted mid-stream', async () => {
    const controller = new AbortController();
    let pageCount = 0;

    const fetchPage = vi.fn<PageFetcher<number>>(async () => {
      pageCount++;
      return createPage([pageCount * 10], `cur_${pageCount}`);
    });

    const collected: number[] = [];
    await expect(async () => {
      for await (const num of paginate({
        fetchPage,
        options: { signal: controller.signal },
      })) {
        collected.push(num);
        if (collected.length === 1) {
          controller.abort();
        }
      }
    }).rejects.toThrow();

    expect(collected).toEqual([10]);
    // Page 2 should never be requested
    expect(fetchPage).toHaveBeenCalledTimes(1);
  });

  it('detects repeated cursor and throws typed RepeatedCursorError', async () => {
    let callCount = 0;
    const fetchPage = vi.fn<PageFetcher<string>>(async () => {
      callCount++;
      // Malformed server repeatedly returns the same cursor
      return createPage([`item_${callCount}`], 'loop_cursor_001');
    });

    let caughtError: unknown = null;
    try {
      // eslint-disable-next-line @typescript-eslint/no-unused-vars
      for await (const _ of paginate({ fetchPage })) {
        // continue
      }
    } catch (err) {
      caughtError = err;
    }

    expect(caughtError).toBeInstanceOf(RepeatedCursorError);
    const repeatedErr = caughtError as RepeatedCursorError;
    expect(repeatedErr.cursor).toBe('loop_cursor_001');
    expect(repeatedErr.code).toBe('REPEATED_CURSOR');
    expect(repeatedErr.message).toContain('Repeated cursor detected: "loop_cursor_001"');
    // Did not loop indefinitely
    expect(fetchPage).toHaveBeenCalledTimes(2);
  });

  it('propagates HTTP failures directly without retrying outside policy', async () => {
    const error500 = new NexoraError(500, {
      code: 'INTERNAL_ERROR',
      message: 'Database connection failed',
    });

    const fetchPage = vi.fn<PageFetcher<string>>(async () => {
      throw error500;
    });

    await expect(async () => {
      await paginateAll({ fetchPage });
    }).rejects.toThrow(error500);

    expect(fetchPage).toHaveBeenCalledTimes(1);
  });

  it('forwards custom RequestOptions on every page request', async () => {
    const passedOptions: (RequestOptions | undefined)[] = [];
    const fetchPage = vi.fn<PageFetcher<string>>(async (_q, opts) => {
      passedOptions.push(opts);
      if (passedOptions.length === 1) {
        return createPage(['first'], 'c2');
      }
      return createPage(['second'], null);
    });

    const customOptions: RequestOptions = {
      idempotencyKey: 'idem_test',
    };

    await paginateAll({
      fetchPage,
      options: customOptions,
    });

    expect(passedOptions).toHaveLength(2);
    expect(passedOptions[0]).toEqual(customOptions);
    expect(passedOptions[1]).toEqual(customOptions);
  });
});
