# Custodial deposit sweep

The sweep engine periodically moves customer deposits out of a tenant's custodial wallets into the tenant's master address, leaving every wallet exactly what it needs to stay open. It runs in the worker, scans hundreds of wallets per run, and packs payments from many wallets into as few Stellar transactions as possible.

## Enable it

Sweeping is off unless both of these are set:

1. **Operator thresholds.** `CUSTODIAL_SWEEP_THRESHOLDS` lists the assets to sweep and the balance above which each is swept, as `ASSET[:ISSUER]=AMOUNT` entries: `XLM=25,USDC=100`. An entry without an issuer matches any issuer, but a payment can only land on a trustline the master address already holds, so nothing the tenant does not accept is ever sent. Leave the variable empty to disable the sweep entirely. `CUSTODIAL_SWEEP_INTERVAL` (default `@every 15m`) sets the schedule and `CUSTODIAL_SWEEP_CONCURRENCY` (default `8`) bounds concurrent Horizon account loads.
2. **Tenant opt-in.** A tenant sets `auto_sweep_enabled: true` with `PUT /v1/wallets/custody-settings`, alongside the `master_address` that receives the funds. Settings are per live/test mode.

## What gets swept

For each wallet, the engine loads the account from Horizon and computes:

- **XLM:** the balance minus the minimum balance minus native selling liabilities. The minimum balance is `(2 + subentries + sponsoring − sponsored) × 0.5 XLM`, which is 1 XLM for the account plus 0.5 XLM for each trustline, offer, signer or data entry. The excess is swept once it exceeds the XLM threshold.
- **Other assets:** the balance minus selling liabilities, swept once it exceeds the asset's threshold and the master address has the trustline headroom to receive it.

These wallets are skipped: the tenant's primary settlement wallet, the master address itself, wallets with a transfer `pending`, `submitted`, `compliance_hold` or `approval_pending`, wallets with a pending or completed account closure, unfunded accounts, and frozen trustlines.

## Batching and fees

Payments from up to 20 wallets (Stellar's signature limit) and up to 100 operations go into one transaction, with each payment sourced from its own wallet and each wallet signing for itself. The wallet in the batch with the most spare XLM is the transaction source and pays the fee (100 stroops per operation). If it is also sweeping XLM, the fee comes out of its sweep, so no wallet ever drops below its reserve. A batch where no wallet has spare XLM for the fee is skipped and reported.

## Records and the wallet.swept event

Every submitted transaction is recorded in `custodial_sweeps` with its hash, fee and per-wallet, per-asset items. Failures are recorded with the reason and the run moves on to the next batch. After a successful transaction, one `wallet.swept` webhook is emitted per swept wallet to the tenant's endpoints:

```json
{
  "sweep_id": "7c1e9a4e-…",
  "wallet_id": "3f1c2b7e-…",
  "public_key": "GBXC…",
  "destination_address": "GA5Z…",
  "transaction_hash": "abc123…",
  "mode": "live",
  "items": [
    {"asset_code": "USDC", "issuer": "GA5Z…", "amount": "250.0000000", "balance_before": "250.0000000", "retained": "0.0000000"},
    {"asset_code": "XLM", "amount": "40.9999700", "balance_before": "42.5000000", "retained": "1.5000000"}
  ],
  "fee_paid": "0.0000300",
  "swept_at": "2026-10-22T12:00:00Z"
}
```

`retained` is the wallet's balance of the asset after the sweep; `fee_paid` is non-zero only for the wallet that paid the batch fee.

## Operational notes

A run is registered with a 30-minute uniqueness lock so a long scan is never overlapped by the next tick. A transfer created between the pending-transfer check and submission can fail for lack of funds; it is not lost, and the next run sees the new balances. The sweep never creates trustlines on the master address; add them there first for every asset you want collected.

The sweep is skipped while any row exists in `sweep_halts`; the zero-sum ledger invariant checker (#117) inserts one when tenant balances plus accumulated fees stop matching the pooled reserves, and removes it once the books balance again. If the halt state cannot be read, the run is skipped and retried.
