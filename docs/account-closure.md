# Custodial account closure

Closing a custodial wallet permanently destroys its Stellar account. Nexora removes every asset trustline and merges the account into the tenant's master address, which recovers the native XLM held as minimum reserves (0.5 XLM per trustline plus the 1 XLM base reserve). The wallet row stays in place for history, but the account no longer exists on the ledger and cannot be reopened under the same key.

## Configure custody settings first

Set the master address with `PUT /v1/wallets/custody-settings`, providing a `master_address` (a Stellar `G...` account ID) and, optionally, the `primary_settlement_wallet_id`. Settings are stored separately for live and test mode, so configure each mode you close wallets in. The master address must already exist on the ledger; a closure into an unfunded address is refused before any fee is spent. The primary settlement wallet, and any wallet whose key is the master address itself, can never be closed. Changes are recorded in the tenant audit log as `wallet.custody_settings.updated`.

## Close a wallet

Call `POST /v1/wallets/{id}/close` with a `justification` of at least ten characters and a `confirm` field repeating the wallet's public key. The confirmation makes the call impossible to issue by accident or to replay against a different wallet. The operator recorded on the closure is the authenticated user or API key; it is never read from the request body. Every closure, custody settings and closure history route requires the Owner or Admin role.

The closure is refused with `409` when:

| Code | Cause |
| --- | --- |
| `CLOSURE_OUTSTANDING_BALANCE` | A non-native trustline holds a balance or open buying/selling liabilities. Move or sell the asset first. |
| `CLOSURE_PENDING_TRANSFERS` | A transfer into or out of the wallet is `pending`, `submitted`, `compliance_hold` or `approval_pending`. |
| `CLOSURE_PROTECTED_ACCOUNT` | The wallet is the tenant's primary settlement account or master address. |
| `CLOSURE_UNSUPPORTED_ACCOUNT` | The account is a contract wallet, has open offers, data entries, extra signers, liquidity pool shares, sponsors other entries, or needs more than one transaction; or the master address is unfunded. These need manual handling. |
| `CLOSURE_ALREADY_EXISTS` | The wallet already has a pending or completed closure. |

## What happens on-chain

Trustline removal and the merge are one transaction: one `ChangeTrust` with limit `0` per trustline, followed by `AccountMerge` to the master address. Operations apply in order, so every trustline is gone before the merge runs, and if any operation fails nothing is applied. The response is the closure record, with the removed trustlines, the native balance merged (before the transaction fee) and the transaction hash. If the network rejects the transaction, the response is `502` with the record marked `failed` and the reason; correct the cause and close again.

## Compliance record

Before the transaction is submitted, Nexora writes a `wallet.closure.initiated` audit event carrying the operator, justification, destination and trustlines. If that write fails the closure is aborted, so an account is never destroyed without a record. The outcome is then logged as `wallet.closure.completed` (with the transaction hash) or `wallet.closure.failed`. Closure rows in `account_closures` are append-only: a database trigger rejects deletes, any change to the operator, justification, wallet or destination, and any change once a closure is completed or failed. Browse them with `GET /v1/wallets/closures` and `GET /v1/wallets/closures/{closureId}`.

## Limits

Pending transfers are checked when the closure starts. A transfer created between that check and the merge fails on submission because the source account no longer exists; its funds are not lost. At most 99 trustlines can be removed, because a Stellar transaction is limited to 100 operations.
