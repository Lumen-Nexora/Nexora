# Nexora TypeScript SDK

## Cursor Pagination

The SDK provides helpers for cursor-based pagination to simplify fetching large datasets.

### Single Page

```typescript
import { NexoraClient } from '@lumen-nexora/nexora';

const client = new NexoraClient({ /* config */ });
const { nodes, pageInfo } = await client.transfers.list(
  { from: '0x123...' },
  { orderBy: 'timestamp', orderDirection: 'desc' },
  { first: 50 }
);

console.log(nodes); // Array of transfers
console.log(pageInfo.hasNextPage); // boolean
console.log(pageInfo.endCursor); // string | null
```

### Streaming with Async Iteration

```typescript
const client = new NexoraClient({ /* config */ });

for await (const transfer of client.transfers.iterate(
  { from: '0x123...' },
  { orderBy: 'timestamp' }
)) {
  console.log(transfer);
  // Break early to stop further requests
  if (someCondition) break;
}
```

### Fetch All Pages

```typescript
const allTransfers = await client.transfers.listAll(
  { from: '0x123...' },
  { orderBy: 'timestamp' }
);
```

### Aborting Requests

```typescript
const controller = new AbortController();

setTimeout(() => controller.abort(), 5000); // Abort after 5 seconds

try {
  for await (const transfer of client.transfers.iterate(
    { from: '0x123...' },
    undefined,
    undefined,
    controller.signal
  )) {
    console.log(transfer);
  }
} catch (error) {
  if (error.name === 'CursorPaginationError') {
    console.log('Iteration was aborted');
  }
}
```
