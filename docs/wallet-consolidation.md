# Multi-wallet consolidation

Consolidation moves the balances of many of a tenant's custodial wallets (up to 500 per request) into one of its wallets. It is built for large sets: the work is split into sequential batches of 10–20 wallets per Stellar transaction and runs in the worker, so a request for 100+ wallets is neither one oversized transaction nor one long HTTP call.

## Preview first

`POST /v1/wallets/consolidations` with `dry_run: true` returns exactly what would be submitted right now, and nothing is recorded or sent:

- `batches`: the wallets in each transaction, its operation count, its fee and which wallet pays it
- `wallets`: per source wallet, the assets and amounts that will move, or a `skip_reason`
- `total_fee` / `total_fee_stroops`, `reserve_recovered`, and `net_xlm` (recovered reserves minus fees; negative means the consolidation costs more than it frees)
- `resulting_balances`: the destination's balance of each asset before and after

A dry run does not need an `idempotency_key`.

## Run it

Send the same body without `dry_run`, with an `idempotency_key` (up to 255 characters). The response is `202` with the operation in `pending`; poll `GET /v1/wallets/consolidations/{id}` until it is `completed`, `partially_completed` or `failed`. Each batch in `batches` carries its transaction hash or error. A failed batch does not stop the others; its wallets keep their balances and can be consolidated again under a new key.

The key makes the request idempotent permanently, not just for a retry window. Sending the same key with the same body (source order does not matter) returns the original operation with `200` and never moves funds twice. Sending the same key with a different body is refused with `409 CONFLICT`. Only one worker can run an operation, and a redelivered task for a finished operation does nothing.

## Options

- `batch_size` (10–20, default 20): wallets per transaction. Stellar allows 20 signatures per transaction, and each source signs for itself. A batch also closes early at 100 operations, which matters when merging wallets with many trustlines.
- `merge_sources` (default `false`): close each source after emptying it. Every non-native balance is paid to the destination, every trustline is removed and the account is merged, so its reserves (1 XLM plus 0.5 XLM per trustline) are recovered at the destination. A source is only merged when nothing would be lost: no open offers or liabilities, data entries, extra signers, sponsorships or pool shares, and the destination must hold a trustline with room for every asset. The tenant's primary settlement wallet and master address are never merged.

Without `merge_sources`, each source keeps its minimum balance and selling liabilities and everything else moves.

## Safety checks

All source wallets and the destination must belong to the calling tenant; any other wallet is reported as not found. They must be custodial, listed once, and the destination cannot also be a source. The destination must exist on the ledger. A source is skipped (see `skip_reason`) if it has a transfer `pending`, `submitted`, `compliance_hold` or `approval_pending`, is unfunded, has nothing to move, or cannot be merged safely. If nothing at all can move, the request is refused with `409 CONSOLIDATION_NOTHING_TO_MOVE`.

The fee for each batch is paid by the source with the most XLM above its reserve, out of what it was going to send anyway, so no wallet is pushed below its minimum balance. Execution re-plans against the ledger at run time, so balances that changed after the request are handled correctly.

Every consolidation writes `wallet.consolidation.requested` and `wallet.consolidation.finished` events to the tenant audit log, with the acting principal. All consolidation routes require the Owner or Admin role.
