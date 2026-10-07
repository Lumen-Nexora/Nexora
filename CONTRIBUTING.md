# Contributing to Nexora

Nexora uses [GrantFox](https://grantfox.xyz) to fund and coordinate open-source contributions. Contributors pick a funded issue, implement it, and submit a PR. One issue per contributor at a time.

---

## Quick Setup

### Prerequisites

- Go 1.22+
- PostgreSQL 15+
- Redis 7+

### Get running

```bash
git clone https://github.com/Lumen-Nexora/Nexora.git
cd Nexora
go mod tidy
cp .env.example .env
# Fill in DATABASE_URL, REDIS_URL, MASTER_ENCRYPTION_KEY, STELLAR_* values
make migrate
```

```bash
# Terminal 1 — API server
make run-api       # http://localhost:3000

# Terminal 2 — Background worker (required for transfer settlement)
make run-worker
```

Generate a `MASTER_ENCRYPTION_KEY`:

```bash
openssl rand -hex 32
```

Fund a testnet wallet:

```bash
curl "https://friendbot.stellar.org?addr=<PUBLIC_KEY>"
```

---

## Picking Up an Issue

1. Find an open, unassigned issue in [GitHub Issues](https://github.com/Lumen-Nexora/Nexora/issues)
2. Comment to claim it — wait for assignment before starting
3. Fork and branch off `main`: `feat/issue-<number>-short-description`
4. Stay current with `git rebase origin/main`

Read the full issue body before writing code. Every issue has an acceptance criteria checklist — that determines whether the PR merges.

---

## Codebase Orientation

Nexora is structured in `internal/` packages, each owning a slice of the domain:

```
internal/
├── domain/       ← core types only — no business logic, no DB
├── wallet/       ← service + repository interface + HTTP handler
├── transfer/     ← same pattern
├── settlement/   ← reads from queue, calls Stellar, updates DB
├── postgres/     ← repository implementations (pgx/v5)
├── stellar/      ← Horizon client + signer interface
└── server/       ← router, middleware, auth
```

Before touching a package, read its files end to end. The package boundaries matter — don't reach across them. Business logic lives in the service layer, not in handlers or repositories.

---

## Working with AI Agents

AI coding assistants (Claude Code, Cursor, Copilot, etc.) are welcome. The issues are written with enough context to serve as effective prompts. You are responsible for everything in the PR — unreviewed AI output that doesn't compile, breaks tests, or ignores acceptance criteria will be closed.

### Effective patterns

**1. Read before write**

Ask the agent to read the files it will touch before making any changes:

```
Read these files first:
- internal/settlement/engine.go
- internal/domain/transaction.go
- internal/domain/errors.go
- internal/stellar/client.go

Then explain what the settlement flow does and what changes this issue requires.
Don't write any code yet.

[paste issue body]
```

This is especially important in Go — type mismatches and interface violations won't be obvious until compilation.

**2. Paste the full issue, not a summary**

The issues in this repo are written to be directly usable as prompts. Paste the complete body including the acceptance criteria checklist.

**3. Show it the pattern to follow**

Nexora has a consistent service/handler/repository pattern. Point the agent at the closest existing example:

```
The wallet package in internal/wallet/ is the reference implementation.
Follow the same service interface + repository interface + handler structure
when building the new package.
```

**4. Ask it to confirm Go interfaces compile**

```
After writing the code, verify that all interface implementations are complete
and that there are no missing methods. List every interface in the new package
and which struct satisfies it.
```

**5. Review the diff line by line**

Agents commonly:

- Forget to wire up new packages in `cmd/api/main.go` or `cmd/worker/main.go`
- Implement an interface method with the wrong signature
- Import packages not in `go.mod`
- Miss error wrapping with `fmt.Errorf("...: %w", err)` (required for error chain)

Check compilation: `go build ./...` before running tests.

### What not to do

- Don't commit code that doesn't compile
- Don't let the agent add files the issue didn't ask for
- Don't use the agent to write the PR description

---

## Running Tests

```bash
make test           # go test ./... -race -count=1
make test-cover     # with HTML coverage report
make lint           # golangci-lint (install: https://golangci-lint.run/usage/install/)
make build          # compile both binaries
```

All tests must pass before submitting a PR. Fix lint warnings — CI enforces it.

For the web dashboard and TypeScript SDK, run their checks from the repository
root with:

```bash
cd apps/web && npm ci && npm run typecheck && npm run lint && npm run format:check && npm run build
cd sdk && npm ci --ignore-scripts && npm run typecheck && npm run format:check && npm run build
```

`npm run lint` and `npm run format:check` are non-mutating. Use `npm run lint:fix`
or `npm run format` in `apps/web` to apply local fixes. `make typecheck`,
`make format-check`, and `make clean` run the corresponding checks or cleanup
across both TypeScript packages.

---

## Code Style

- **Error handling**: always wrap with `fmt.Errorf("context: %w", err)`; never swallow errors silently
- **Decimal arithmetic**: use `shopspring/decimal` for all monetary values — no `float64`
- **Context propagation**: every function that touches the DB or network must accept `context.Context` as its first argument
- **Interface-first**: define the interface in the package that owns the domain (`wallet.Service`, `wallet.Repository`); implementations live in `postgres/` or elsewhere
- **Comments**: only if the _why_ is genuinely non-obvious; don't describe what the code does
- **No extra files**: don't add docs, notes, or scripts the issue didn't require

---

## Opening a PR

**Title**: `feat: <short description> (closes #<number>)`

**Body should include**:

- What you built
- Any architectural decisions made that weren't obvious from the issue
- How a reviewer can manually test the change (exact curl commands welcome)

Always include `Closes #<issue-number>`.

**Checklist before submitting**:

- [ ] All acceptance criteria in the issue are satisfied
- [ ] `go build ./...` compiles without errors
- [ ] `make test` passes
- [ ] `make lint` passes
- [ ] New packages are wired into `cmd/api/main.go` or `cmd/worker/main.go` as appropriate
- [ ] No files added beyond what the issue required

---

## Questions

Comment on the issue thread. Don't open a new issue to ask about an existing one.
