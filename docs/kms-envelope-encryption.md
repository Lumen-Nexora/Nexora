# KMS envelope encryption for custodial seeds

Nexora can keep custodial wallet seeds under a key that never leaves AWS KMS, Google Cloud KMS or HashiCorp Vault. Without a KMS, seeds are encrypted with `MASTER_ENCRYPTION_KEY` as before.

## How it works

Each seed is sealed with **envelope encryption**:

1. A fresh random 256-bit data encryption key (DEK) is generated for that seed.
2. The seed is encrypted with the DEK (AES-256-GCM).
3. The DEK is wrapped (encrypted) by the provider's key encryption key (KEK), and the plaintext DEK is overwritten with zeros.
4. Only `v2::<provider>::<wrapped DEK>::<encrypted seed>` is stored.

A seed is decrypted only at the moment a transaction is signed. The signer asks the KMS to unwrap the DEK, decrypts the seed into a short-lived buffer, decodes it into a raw ed25519 key, produces the one signature, and zeroes the DEK, the seed and the private key before returning, including when signing fails. The seed is never turned into a Go string, which could not be erased. One KMS call is made per signature.

Go's garbage collector can copy memory, and the KMS's JSON responses pass through HTTP buffers, so zeroizing is best effort. It still shortens how long key material sits in memory from the life of the process to the length of one signature.

## Configure a provider

Set `KMS_PROVIDER` and the provider's variables. The worker and the API both read them, and startup fails fast if the configuration is incomplete.

| Provider | `KMS_PROVIDER` | Variables | Needed permissions |
| --- | --- | --- | --- |
| AWS KMS | `aws` | `AWS_REGION`, `AWS_KMS_KEY_ID` (key ID, ARN or alias), `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN`, optional `AWS_KMS_ENDPOINT` | `kms:Encrypt`, `kms:Decrypt`, `kms:ReEncrypt*`; `kms:EnableKeyRotation` for rotation |
| Google Cloud KMS | `gcp` | `GCP_KMS_KEY_NAME` (`projects/…/cryptoKeys/…`), optional `GCP_ACCESS_TOKEN`; otherwise the GCE/GKE metadata server supplies tokens | `cloudkms.cryptoKeyVersions.useToEncrypt` / `useToDecrypt`; `cloudkms.cryptoKeys.update` for rotation |
| Vault Transit | `vault` | `VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_TRANSIT_KEY`, optional `VAULT_TRANSIT_MOUNT` (default `transit`) and `VAULT_NAMESPACE` | `update` on `transit/encrypt/<key>`, `transit/decrypt/<key>`, `transit/rewrap/<key>`, and `transit/keys/<key>/config` for rotation |

All three are called over their HTTP APIs directly; Nexora adds no SDK dependencies. AWS requests are signed with Signature Version 4. The AWS client uses static credentials only; it does not yet support instance profiles or IRSA.

## Key rotation

Set `KMS_ROTATION_PERIOD` (for example `2160h` for 90 days) to have the provider rotate the KEK automatically. The policy is applied at startup:

- AWS: `EnableKeyRotation` with `RotationPeriodInDays` (90–2560 days)
- Google Cloud: the CryptoKey's `rotationPeriod` and `nextRotationTime` (at least 24h)
- Vault: the Transit key's `auto_rotate_period` (at least 1h)

Rotation never breaks existing seeds, because every provider records the KEK version in its ciphertext and decrypts with it. To move a seed onto the newest KEK version, `crypto.RewrapSecret` re-wraps only its DEK (server-side with AWS `ReEncrypt` and Vault `rewrap`; unwrap-and-wrap with Cloud KMS), leaving the encrypted seed unchanged.

## Enabling it on an existing deployment

Keep `MASTER_ENCRYPTION_KEY`. After the KMS is enabled, new wallets get v2 envelopes, and seeds sealed before that keep working through the master key. `crypto.RewrapSecret` also migrates a v1 seed to v2 for operators who want every seed behind the KMS. Do not switch `KMS_PROVIDER` to a different provider while v2 seeds exist; they can only be opened by the provider that sealed them.
