# Error Reference

Every `/v1` error uses this JSON envelope. The HTTP response status equals `error.status`; `error.request_id` equals the `X-Request-ID` response header. A supplied non-empty request ID is echoed, otherwise the server generates one. Endpoint examples that omit `status` or `request_id` are abbreviated; actual responses always include both fields.

```json
{
  "error": {
    "code": "ERROR_CODE",
    "message": "Human-readable description of what went wrong.",
    "status": 400,
    "request_id": "<effective-request-id>"
  }
}
```

Errors are grouped by domain. Each entry includes the HTTP status code, the `code` field value, a description, resolution steps, and an example response body.

---

## Auth Errors

### `401` — `missing or invalid authorization header`

| Field | Value |
|---|---|
| **HTTP Status** | `401 Unauthorized` |
| **Code** | `UNAUTHORIZED` |
| **Description** | The `Authorization` header is missing, doesn't use the `Bearer` scheme, or is malformed. |
| **Resolution** | Include a valid `Authorization: Bearer <api_key>` header on every request. |

**Example response:**
```json
{"error":{"code":"UNAUTHORIZED","message":"missing or invalid authorization header","status":401,"request_id":"<effective-request-id>"}}
```

---

### `401` — `invalid api key or authentication token`

| Field | Value |
|---|---|
| **HTTP Status** | `401 Unauthorized` |
| **Code** | `UNAUTHORIZED` |
| **Description** | The bearer credential does not match a valid API key or authentication token. |
| **Resolution** | Verify the bearer credential and use an active API key or access token. |

**Example response:**
```json
{"error":{"code":"UNAUTHORIZED","message":"invalid api key or authentication token","status":401,"request_id":"<effective-request-id>"}}
```

---

### `401` — `revoked api key`

| Field | Value |
|---|---|
| **HTTP Status** | `401 Unauthorized` |
| **Code** | `API_KEY_REVOKED` |
| **Description** | The API key has been revoked via `DELETE /v1/keys/:id` and can no longer be used. |
| **Resolution** | Create a new API key via `POST /v1/keys`. |

**Example response:**
```json
{"error":{"code":"API_KEY_REVOKED","message":"revoked api key","status":401,"request_id":"<effective-request-id>"}}
```

---

### `401` — `tenant environment is required`

| Field | Value |
|---|---|
| **HTTP Status** | `401 Unauthorized` |
| **Code** | `UNAUTHORIZED` |
| **Description** | The tenant or environment could not be resolved from the authenticated request context. |
| **Resolution** | Ensure you are authenticated and your session has a valid tenant context. Re-authenticate if the issue persists. |

**Example response:**
```json
{"error":{"code":"UNAUTHORIZED","message":"tenant environment is required","status":401,"request_id":"<effective-request-id>"}}
```

---

### `403` — `INSUFFICIENT_SCOPE` | `API key does not have the required scope: <scope>`

| Field | Value |
|---|---|
| **HTTP Status** | `403 Forbidden` |
| **Code** | `INSUFFICIENT_SCOPE` |
| **Description** | The API key is valid but was not granted the scope this operation needs. Reads (`GET`/`HEAD`) need `<resource>:read`; anything that changes state needs `<resource>:write`. The message names the missing scope. The code is stable, so match on it rather than the message. The denial is recorded in the audit log as `api_key.scope_denied`. |
| **Resolution** | Create a key with the missing scope, or use one that has it. See [API key scopes](api-key-scopes.md). A scoped key creating another key gets the same error if it asks for a scope it doesn't hold itself. |

**Example response:**
```json
{
  "error": {
    "code": "INSUFFICIENT_SCOPE",
    "message": "API key does not have the required scope: transfers:write"
  }
}
```

---

## Wallet Errors

### `404` — `NOT_FOUND` | `wallet not found`

| Field | Value |
|---|---|
| **HTTP Status** | `404 Not Found` |
| **Code** | `NOT_FOUND` |
| **Description** | The specified wallet ID does not exist or does not belong to your tenant. |
| **Resolution** | Verify the wallet ID. List available wallets or create a new one via `POST /v1/wallets`. |

**Example response:**
```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "wallet not found"
  }
}
```

---

## Transfer Errors

### `404` — `NOT_FOUND` | `transaction not found`

| Field | Value |
|---|---|
| **HTTP Status** | `404 Not Found` |
| **Code** | `NOT_FOUND` |
| **Description** | The specified transaction or transfer ID does not exist or belongs to another tenant. |
| **Resolution** | Verify the ID and ensure it was created under your tenant. |

**Example response:**
```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "transaction not found"
  }
}
```

---

### `400` — `BAD_REQUEST` | `source and destination wallets must differ`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | The `from_wallet_id` and `to_wallet_id` are the same. Self-transfers are not supported. |
| **Resolution** | Use two different wallets for the transfer. Create a second wallet if needed via `POST /v1/wallets`. |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "source and destination wallets must differ"
  }
}
```

---

### `400` — `BAD_REQUEST` | `insufficient balance`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | The source wallet does not have enough funds to cover the transfer amount plus fees. |
| **Resolution** | Check the wallet balance via `GET /v1/wallets/:id/balances` and fund the wallet (on testnet via Friendbot). |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "insufficient balance"
  }
}
```

---

### `400` — `BAD_REQUEST` | `invalid or unsupported asset`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | The specified asset code is not recognised or is not supported by the platform (e.g., a non-Stellar asset or an asset without a configured issuer). |
| **Resolution** | Use a supported asset (currently `USDC` and `XLM`). Check the asset code and issuer configuration. |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "invalid or unsupported asset"
  }
}
```

---

### `400` — `BAD_REQUEST` | `slippage tolerance exceeded`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | The FX quote has expired (30-second TTL). The stored rate is no longer valid and the conversion was rejected to protect against slippage. |
| **Resolution** | Request a new quote via `POST /v1/fx/quote` and retry the conversion immediately. |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "slippage tolerance exceeded"
  }
}
```

---

### `400` — `BAD_REQUEST` | `fee schedule not found`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | No fee schedule has been configured for your tenant. Transfers and conversions cannot proceed without a valid fee schedule. |
| **Resolution** | Contact support to have a fee schedule configured for your tenant. |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "fee schedule not found"
  }
}
```

---

### `500` — `INTERNAL_ERROR` | `stellar transaction submission failed`

| Field | Value |
|---|---|
| **HTTP Status** | `500 Internal Server Error` |
| **Code** | `INTERNAL_ERROR` |
| **Description** | The Stellar network rejected the submitted transaction. The response body will contain `"an unexpected error occurred"` (generic message). The exact Stellar error is logged server-side. |
| **Resolution** | Check the Stellar account sequence number, balance, and trustlines. Ensure the destination has a trustline for the asset. Retry the transfer. |

**Example response:**
```json
{
  "error": {
    "code": "INTERNAL_ERROR",
    "message": "an unexpected error occurred"
  }
}
```

---

## Webhook Errors

### `404` — `NOT_FOUND` | `webhook endpoint not found`

| Field | Value |
|---|---|
| **HTTP Status** | `404 Not Found` |
| **Code** | `NOT_FOUND` |
| **Description** | The specified webhook endpoint ID does not exist or belongs to another tenant. |
| **Resolution** | Verify the webhook ID. List your endpoints via `GET /v1/webhooks`. |

**Example response:**
```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "webhook endpoint not found"
  }
}
```

---

### `404` — `NOT_FOUND` | `webhook delivery not found`

| Field | Value |
|---|---|
| **HTTP Status** | `404 Not Found` |
| **Code** | `NOT_FOUND` |
| **Description** | The specified webhook delivery log entry does not exist. |
| **Resolution** | Verify the delivery ID. Check delivery logs for this endpoint via `GET /v1/webhooks/:id/deliveries`. |

**Example response:**
```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "webhook delivery not found"
  }
}
```

---

## FX Errors

### `400` — `BAD_REQUEST` | `invalid request body`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | The request body is missing, malformed, or contains invalid JSON. This can also occur when required fields are missing or validation constraints are not met. |
| **Resolution** | Check the request body for valid JSON syntax and ensure all required fields are present and correctly typed. |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "invalid request body"
  }
}
```

---

### `400` — `BAD_REQUEST` | `wallet id is required`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | The wallet ID path parameter is missing or empty (fiat deposit/withdrawal endpoints). |
| **Resolution** | Include the wallet ID in the URL path: `/v1/wallets/{id}/deposit/fiat`. |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "wallet id is required"
  }
}
```

---

### `400` — `BAD_REQUEST` | `invalid amount`

| Field | Value |
|---|---|
| **HTTP Status** | `400 Bad Request` |
| **Code** | `BAD_REQUEST` |
| **Description** | The amount field is missing, zero, negative, or not a valid decimal string (fiat deposit/withdrawal). |
| **Resolution** | Provide a positive decimal string for the amount field (e.g., `"1000"`). |

**Example response:**
```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "invalid amount"
  }
}
```

---

## Compliance Errors

### `403` — `TRANSFER_BLOCKED_SANCTIONS` | `transfer blocked: destination matches a sanctions list entry`

| Field | Value |
|---|---|
| **HTTP Status** | `403 Forbidden` |
| **Code** | `TRANSFER_BLOCKED_SANCTIONS` |
| **Description** | The destination address appears on the OFAC SDN list. No transaction is created; the attempt is recorded in `compliance_blocks`. |
| **Resolution** | Terminal — retrying with the same destination will always fail. This is distinct from the generic `FORBIDDEN`, which indicates insufficient permissions. Contact compliance if you believe the match is incorrect. |

**Example response:**
```json
{
  "error": {
    "code": "TRANSFER_BLOCKED_SANCTIONS",
    "message": "transfer blocked: destination matches a sanctions list entry"
  }
}
```

### `404` — `NOT_FOUND` | `compliance review not found`

| Field | Value |
|---|---|
| **HTTP Status** | `404 Not Found` |
| **Code** | `NOT_FOUND` |
| **Description** | No compliance review exists with that id for your organization. |
| **Resolution** | List pending reviews with `GET /v1/admin/compliance/reviews` and use an id from that response. |

**Example response:**
```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "compliance review not found"
  }
}
```

### `409` — `REVIEW_ALREADY_DECIDED` | `compliance review has already been decided`

| Field | Value |
|---|---|
| **HTTP Status** | `409 Conflict` |
| **Code** | `REVIEW_ALREADY_DECIDED` |
| **Description** | The review has already been approved or rejected. Decisions are terminal, which is what stops two reviewers from releasing the same payment twice. |
| **Resolution** | Fetch the review with `GET /v1/admin/compliance/reviews/{id}` to see the recorded outcome and reviewer. |

**Example response:**
```json
{
  "error": {
    "code": "REVIEW_ALREADY_DECIDED",
    "message": "compliance review has already been decided"
  }
}
```

---

## Rate Limit Errors

Rate limits return the same JSON envelope as other `/v1` errors and include `Retry-After` where applicable.

| HTTP Status | Code | Description | Resolution |
|---|---|---|---|
| `429` | `RATE_LIMITED` | Request quota exceeded for the current period. | Wait for the rate limit window to reset before retrying. |

---

## Stellar / Horizon Pass-Through Errors

When the Stellar network returns an error during transaction submission, Nexora maps it to a generic `500 INTERNAL_ERROR`. The original Stellar error is logged server-side and may include:

| Stellar Error | Meaning |
|---|---|
| `tx_bad_seq` | Account sequence number mismatch. Retry the transfer. |
| `tx_insufficient_balance` | Source account does not have enough of the asset. |
| `op_no_trust` | Destination account has no trustline for the asset. |
| `op_underfunded` | The fee account does not have enough XLM. |
| `tx_too_late` | Transaction age exceeded the maximum allowed. |

**Resolution for all Stellar errors:** Check the source and destination accounts on the Stellar explorer (testnet or mainnet). Ensure the source has sufficient balance, the destination has a trustline for the asset, and the sequence number is correct. Retry the operation.

---

## Complete Error Code Index

| HTTP Status | Code | Message | Domain |
|---|---|---|---|
| `400` | `BAD_REQUEST` | `source and destination wallets must differ` | Transfers |
| `400` | `BAD_REQUEST` | `insufficient balance` | Transfers |
| `400` | `BAD_REQUEST` | `invalid or unsupported asset` | Transfers / FX |
| `400` | `BAD_REQUEST` | `slippage tolerance exceeded` | FX |
| `400` | `BAD_REQUEST` | `fee schedule not found` | Fees |
| `400` | `BAD_REQUEST` | `invalid request body` | General |
| `400` | `BAD_REQUEST` | `wallet id is required` | Fiat |
| `400` | `BAD_REQUEST` | `invalid amount` | Fiat |
| `403` | `TRANSFER_BLOCKED_SANCTIONS` | `transfer blocked: destination matches a sanctions list entry` | Compliance |
| `409` | `REVIEW_ALREADY_DECIDED` | `compliance review has already been decided` | Compliance |
| `409` | `LAST_ORG_OWNER` | `organization must keep at least one owner` | Organization |
| `401` | `UNAUTHORIZED` | `missing or invalid authorization header` | Auth |
| `401` | `UNAUTHORIZED` | `invalid api key or authentication token` | Auth |
| `401` | `API_KEY_REVOKED` | `revoked api key` | Auth |
| `401` | `UNAUTHORIZED` | `invalid or expired refresh token` | Auth |
| `401` | `UNAUTHORIZED` | `tenant environment is required` | Auth |
| `405` | `METHOD_NOT_ALLOWED` | `method not allowed` | General |
| `404` | `NOT_FOUND` | `wallet not found` | Wallets |
| `404` | `NOT_FOUND` | `transaction not found` | Transfers |
| `404` | `NOT_FOUND` | `webhook endpoint not found` | Webhooks |
| `404` | `NOT_FOUND` | `webhook delivery not found` | Webhooks |
| `404` | `NOT_FOUND` | `compliance review not found` | Compliance |
| `500` | `INTERNAL_ERROR` | `an unexpected error occurred` | General |
| `500` | `INTERNAL_ERROR` | `an unexpected error occurred` (Stellar submission failure) | Stellar |
| `500` | `INTERNAL_ERROR` | `an unexpected error occurred` (decryption failure) | Crypto |
| `500` | `INTERNAL_ERROR` | `an unexpected error occurred` (panic recovery) | General |
| `429` | `RATE_LIMITED` | `rate limit exceeded` | Rate Limits |

---

## Error Response Format

All `/v1` errors follow this schema (including auth, authorization, rate-limit, and middleware failures):

```json
{
  "error": {
    "code": "ERROR_CODE",
    "message": "Human-readable description",
    "status": 400,
    "request_id": "<effective-request-id>"
  }
}
```

The `request_id` value matches the `X-Request-ID` response header and can be used to correlate an error with server logs. `/health`, `/health/ready`, and `/health/live` retain their documented probe response formats; they are not versioned API routes.
