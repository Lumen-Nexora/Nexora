# Security Policy

Nexora handles financial transactions, asset transfers, and cryptographic keys across traditional banking and the Stellar/Soroban network. Security and supply-chain integrity are our highest priorities.

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| main    | :white_check_mark: |

---

## Supply Chain & Contributor Security Policy

To protect maintainers, contributors, and operators from malicious upstream pull requests and dependency poisoning:

1. **Automated CI Security Gate**:
   - Every commit and pull request must pass the automated `.github/workflows/security-scan.yml` scanner.
   - Any pull request attempting to add obfuscated JavaScript, unauthorized execution hooks (e.g. `node init`, `curl | sh`), detached background processes, or unauthorized network calls is blocked automatically.
2. **Strict Manifest & Script Controls**:
   - Modification to build, start, or test scripts in `package.json` or `Cargo.toml` is subject to two-person maintainer review.
   - Direct execution hooks (`preinstall`, `postinstall`) in dependencies are disabled in CI via `--ignore-scripts`.
3. **Branch Protection & Commit Attribution**:
   - Direct pushes to `main` are restricted.
   - Code contributions require peer review and clean cryptographic commits.
4. **Environment Isolation**:
   - Sensitive environment variables (`.env`, `.env.local`), IDE settings (`.vscode/`), and credentials are strictly ignored via `.gitignore` and monitored by secret-scanning workflows.

---

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

To report a security issue, please email **derinsolaowolabi@gmail.com** with:

1. A description of the vulnerability
2. Steps to reproduce
3. Potential impact
4. Any suggested fixes (optional)

### What to expect

- **Acknowledgment**: We will acknowledge receipt within 48 hours.
- **Assessment**: We will assess the report and respond with our evaluation within 7 days.
- **Resolution**: Critical issues will be patched as soon as possible. We will coordinate disclosure timing with you.
- **Credit**: We will credit reporters in the release notes (unless you prefer to remain anonymous).

---

## Scope

The following are in scope:

- API authentication & authorization bypass
- Unauthorized access to custodial wallets or funds
- Cryptographic key exposure or envelope encryption flaws
- Soroban smart contract logic errors (escrow, batch transfer, circuit breaker, fee splitter)
- SQL injection, XSS, or other injection attacks
- Stellar transaction manipulation or replay vulnerabilities
- Webhook signature verification bypass
- Compliance & OFAC sanctions screening bypass

The following are out of scope:

- Denial of service attacks
- Social engineering
- Issues in third-party dependencies (report upstream)
- Attacks requiring physical device access

---

## Security Best Practices for Operators

When deploying Nexora:

1. **Encryption keys**: Generate `MASTER_ENCRYPTION_KEY` using `openssl rand -hex 32`. Never reuse keys across environments. Use AWS KMS, GCP KMS, or HashiCorp Vault in production.
2. **Database**: Use SSL/TLS connections (`sslmode=require` or `sslmode=verify-full`).
3. **Redis**: Enable authentication and use TLS in production.
4. **API keys**: Store `sk_live_*` keys securely; they grant full access to tenant resources. Use scoped API keys with rotation reminders.
5. **Webhook secrets**: Verify signatures on every delivery using timestamped HMAC-SHA256 headers. Reject requests older than 5 minutes.
6. **Network**: Run the API behind a reverse proxy with TLS termination. Never expose PostgreSQL or Redis to the public internet.
7. **Compliance**: Enable `COMPLIANCE_ENABLED=true` in production to screen transfers against OFAC sanctions lists and enforce Travel Rule thresholds.

---

## Incident Response & Remediation Protocol

In the event of an identified security incident or malicious contributor payload:
- **Triage**: The affected branch is frozen immediately, and commits are isolated.
- **Purge**: Malicious files are removed, configuration manifests are restored, and dependent PRs are rebased.
- **Credential Rotation**: All CI secrets, registry tokens, API credentials, and custodian keys are rotated from clean, uncompromised devices.
- **Disclosure**: Transparent post-mortem documentation is published with commit SHAs and IoCs (Indicators of Compromise).
