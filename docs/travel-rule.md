# Travel Rule (IVMS 101)

This implementation adds a fail-closed Travel Rule gate for transfers at or
above `TRAVEL_RULE_THRESHOLD_USD` (default: USD 1,000). The USD amount is
calculated server-side through the configured FX provider; the API caller
cannot submit a trusted USD value.

## Request

Include `travel_rule` in a transfer request when the threshold is met:

`json
{
  "from_wallet_id": "…",
  "to_wallet_id": "…",
  "asset": "USDC",
  "amount": "1200",
  "travel_rule": {
    "recipient_vasp_id": "counterparty-example",
    "ivms101": {
      "originator": {
        "originatorPersons": [{
          "naturalPerson": {
            "name": {
              "nameIdentifier": [{
                "primaryIdentifier": "DOE",
                "secondaryIdentifier": "JANE",
                "nameIdentifierType": "LEGL"
              }]
            }
          }
        }],
        "accountNumber": ["originator-account-ref"]
      },
      "beneficiary": {
        "beneficiaryPersons": [{
          "naturalPerson": {
            "name": {
              "nameIdentifier": [{
                "primaryIdentifier": "SMITH",
                "secondaryIdentifier": "ALEX",
                "nameIdentifierType": "LEGL"
              }]
            }
          }
        }],
        "accountNumber": ["beneficiary-account-ref"]
      }
    }
  }
}
`

The server validates the required IVMS 101 core fields. Unknown optional fields
are preserved in the encrypted JSON so partner-specific extensions are not
silently discarded. Plaintext PII is never logged or stored in the exchange
table.

## Trusted VASP directory

Set `TRAVEL_RULE_ENABLED=true` and provide
`TRAVEL_RULE_ORIGINATING_VASP_ID` plus
`TRAVEL_RULE_VASP_DIRECTORY_JSON` as a JSON array:

- `id`: stable VASP identifier accepted by the API
- `protocol`: `trp` or `openvasp`
- `endpoint`: operator-configured HTTPS endpoint
- `encryption_public_key`: 32-byte X25519 public key, base64url
- `confirmation_public_key`: 32-byte Ed25519 public key, base64url
- `bearer_token` (optional): counterparty bearer token, supplied from secret management
- `client_cert_file` and `client_key_file` (optional pair): client certificate/key paths for mTLS

Endpoints and keys are not accepted from client requests. Encryption uses an
ephemeral X25519 key, HKDF-SHA256 and AES-256-GCM with transfer/message routing
metadata authenticated as AAD. Only the encrypted envelope is persisted.

## Confirmation and release

The recipient must return a signed confirmation binding its VASP ID, transfer
ID, message ID, envelope digest, outcome and issue time. Confirmations are
verified against the configured Ed25519 key and rejected if stale, future-dated,
mismatched, unsigned or replayed with a conflicting ID. An asynchronous callback
is accepted at `POST /v1/travel-rule/confirmations` and needs no tenant session
because its signature is the authentication mechanism.

Transfers remain `compliance_hold` until a valid confirmation is received. A
separate sanctions/manual-review hold is never cleared by a Travel Rule
confirmation. A rejected exchange moves the transfer to `failed`. If sending to
a VASP fails, the encrypted pending record is retained and the transfer remains
held; retry scheduling and operational alerting must be configured by the
deployment.

## Protocol integration note

The sender provides distinct TRP and OpenVASP HTTP envelope profiles at the
configured partner endpoint. These profiles are a transport integration seam,
not a certification that every vendor's API, authentication or mTLS setup is
identical. Before production use, configure the actual counterparty endpoint,
bearer token, client certificate/key for mTLS, and wire profile required by that VASP. Never commit real keys or bearer tokens to source control. Thresholds and
required fields vary by jurisdiction; this code is an engineering control, not
a legal compliance certification.
