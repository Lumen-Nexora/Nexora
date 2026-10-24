# Rolling transfer velocity limits

Tenant-scoped transfers are limited independently for each asset over rolling
1-hour, 24-hour, 7-day, and 30-day windows. Each window enforces both a transfer
count and a summed transfer amount:

| Window | Count | Volume |
| --- | ---: | ---: |
| 1 hour | 5 | 1,000 |
| 24 hours | 20 | 5,000 |
| 7 days | 100 | 20,000 |
| 30 days | 400 | 50,000 |

The thresholds are multiplied by account type (`individual` 1, `organization`
2) and risk category (`low` 1.5, `medium` 1, `high` 0.5). Fractional count
thresholds are rounded down. Amounts use eight decimal places internally, and
Redis server timestamps are stored as Unix microseconds.

The Redis Lua admission script checks all eight thresholds and records an
in-flight reservation atomically. Reservations count against every window
immediately. Confirmed settlement moves a reservation into the committed
sliding-window event set; cancellation and definitively failed settlement
remove it. A queue enqueue failure deliberately retains the reservation because
the durable transaction remains pending and may be retried by reconciliation.

## Operational limitations

Postgres transaction persistence and Redis reservation cannot participate in a
single distributed transaction. The implementation reserves before persisting
and fails closed if Redis is unavailable. If the database result is ambiguous,
the reservation is kept rather than risk admitting an additional transfer.
Likewise, failed Redis finalization or rollback is logged and leaves the
reservation counted, which is conservative but may temporarily consume tenant
capacity.

An in-flight reservation is intentionally not given an automatic lease: an
expired lease could admit another transfer while an earlier settlement is
still ambiguous. Consequently, a process crash after reserving but before
reaching a terminal transaction state may leave a reservation that needs
operational reconciliation. Committed events are pruned after 30 days.
Transfers without a tenant context (for example, platform/system flows) are
outside this per-tenant limiter; existing compliance and sanctions screening
continues to apply as before.
