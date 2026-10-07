# Webhook signature verification

Endpoint-scoped deliveries include two headers:

- `X-Nexora-Signature` — `sha256=<hex HMAC-SHA256>`
- `X-Nexora-Timestamp` — Unix seconds at delivery time

Tenant-config deliveries use an HMAC of the raw body (without a timestamp
prefix) and include `X-Nexora-Key-ID`, which identifies the secret version used
to sign that delivery. Keep each rotated secret available while its metadata
status is `overlapping`; the inspection endpoint returns version metadata only,
while `POST /v1/webhooks/secret/rotate` discloses the new plaintext once.
Rotation accepts an overlap window from 0 to 86,400 seconds (300 seconds by
default) and requires an `Idempotency-Key` header.

## Verification algorithm

1. Reject the delivery if `|now - X-Nexora-Timestamp| >= 300` (5 minutes) — this
   stops a captured payload from being replayed later.
2. Build `signed_payload = X-Nexora-Timestamp + "." + raw_body` — the **raw**
   request body, exactly as received, not a re-serialized/re-parsed copy
   (re-serializing can reorder keys or change whitespace, which changes the
   bytes and breaks the signature).
3. Compute `expected = HMAC-SHA256(signed_payload, webhook_secret)`, hex
   encoded, prefixed `sha256=`.
4. Compare `expected` to `X-Nexora-Signature` using a **constant-time**
   comparison — never `==`/`.equal?`/`===` on the raw strings. A naive
   comparison's runtime leaks how many leading bytes matched, which is
   enough for an attacker to forge a valid signature one byte at a time.

## Reference implementations

One file per language, each self-contained (standard library only, no
external dependencies): [`verify.ts`](./verify.ts), [`verify.py`](./verify.py),
[`verify.go`](./verify.go), [`verify.rb`](./verify.rb), [`verify.php`](./verify.php).

## Test vectors

[`test-vectors.json`](./test-vectors.json) has 10 vectors — normal payload,
empty body, Unicode body, a 10KB body, a stale timestamp, both edges of the
5-minute tolerance window, a future timestamp within tolerance, and a couple
of small-body edge cases. `expectedSignature` in each vector was computed
directly with Node's `crypto` module (the same algorithm as every reference
implementation above); all five reference implementations were run against
every vector to confirm they agree.

Vectors carry a fixed `referenceNow` rather than real wall-clock time, so
they stay reproducible — when testing your own implementation, treat
`referenceNow` as "now" (or shift every vector's `timestamp` by the same
offset as your actual current time) rather than comparing against your
system clock directly.

## Interactive verifier

The dashboard's **Webhooks → Verify Signature** tool runs this exact
algorithm against pasted headers/body/secret, and the same algorithm backs
`POST /v1/webhooks/verify` (rate limited to 60 requests/minute per IP) for
programmatic checks.
