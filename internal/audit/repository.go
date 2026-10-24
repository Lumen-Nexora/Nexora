package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/dbtx"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TamperProofStore is the storage contract for the hash-chained audit log.
// Append links each new record onto the current chain head inside one
// transaction; ScanChain replays the stored chain in append order.
type TamperProofStore interface {
	ChainSource
	Append(ctx context.Context, event *domain.AuditEvent) error
}

// auditChainAdvisoryKey serializes concurrent appends so exactly one writer
// reads the chain head at a time. The key is arbitrary; it only needs to be
// stable across deployments.
const auditChainAdvisoryKey = "nexora:audit-hash-chain"

// TamperProofRepo persists audit events with cryptographic hash chaining
// against tenant_audit_events (see db/migrations/20261022000000).
//
// Append holds a transaction-scoped advisory lock, reads the stored chain
// head, computes SHA-256(prev_row_hash + row_data + timestamp), and inserts
// the row together with both hashes. A record's hash therefore commits to
// the full history that precedes it: retroactive modification, insertion,
// or deletion of any row invalidates the stored digest of every later row.
//
// The repository is constructed with the primary pool so chain scans never
// read a lagging replica and flag replication lag as tampering.
type TamperProofRepo struct {
	db dbtx.DB
}

func NewTamperProofRepo(db dbtx.DB) *TamperProofRepo {
	return &TamperProofRepo{db: db}
}

// Append links event onto the current chain and inserts it. It joins the
// caller's transaction when one is already open so the audit write stays
// atomic with the operation being audited, and otherwise wraps the
// read-compute-insert sequence in its own transaction.
func (r *TamperProofRepo) Append(ctx context.Context, event *domain.AuditEvent) error {
	if dbtx.HasTx(ctx) {
		return r.append(ctx, event)
	}
	return dbtx.RunInTx(ctx, r.db, func(txCtx context.Context) error {
		return r.append(txCtx, event)
	})
}

func (r *TamperProofRepo) append(ctx context.Context, event *domain.AuditEvent) error {
	db := dbtx.TxFromContext(ctx, r.db)

	if _, err := db.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, auditChainAdvisoryKey,
	); err != nil {
		return fmt.Errorf("lock audit hash chain: %w", err)
	}

	prev, err := r.chainHead(ctx, db)
	if err != nil {
		return err
	}

	// created_at is stored at microsecond precision in postgres; truncating
	// keeps the recomputed timestamp byte-identical to the one hashed here.
	event.CreatedAt = event.CreatedAt.UTC().Truncate(time.Microsecond)
	if event.ID == "" {
		event.ID = uuid.New().String()
	}

	event.PrevRowHash = HashHex(prev)
	event.RowHash = HashHex(ComputeRowHash(prev, event))

	if _, err := db.Exec(ctx,
		`INSERT INTO tenant_audit_events (
			id, tenant_id, mode, actor_type, actor_id, action,
			resource_type, resource_id, metadata, ip_address, user_agent,
			created_at, prev_row_hash, row_hash
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		event.ID, event.TenantID, string(event.Mode), event.ActorType, event.ActorID, event.Action,
		event.ResourceType, event.ResourceID, metadataJSON(event), event.IPAddress, event.UserAgent,
		event.CreatedAt, event.PrevRowHash, event.RowHash,
	); err != nil {
		return fmt.Errorf("insert tamper-proof audit event: %w", err)
	}
	return nil
}

// chainHead returns the latest stored row hash in append order, or the
// genesis hash when the chain is empty (or has not started yet — rows written
// before the hash columns existed are excluded).
func (r *TamperProofRepo) chainHead(ctx context.Context, db dbtx.DB) ([32]byte, error) {
	var headHex *string
	err := db.QueryRow(ctx,
		`SELECT encode(row_hash, 'hex') FROM tenant_audit_events
		 WHERE row_hash IS NOT NULL
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
	).Scan(&headHex)
	switch {
	case err == nil:
		if headHex == nil {
			return GenesisRowHash(), nil
		}
		return ParseHashHex(*headHex)
	case errors.Is(err, pgx.ErrNoRows):
		// An empty chain — or one that has not started hashing yet — begins
		// at the genesis hash.
		return GenesisRowHash(), nil
	default:
		return [32]byte{}, fmt.Errorf("read audit chain head: %w", err)
	}
}

// ScanChain replays every hashed record in append order (created_at, then
// id), rebuilding full audit events. Rows without hashes — records written
// before the hash columns existed — are excluded from the chain.
func (r *TamperProofRepo) ScanChain(ctx context.Context, fn func(ChainRow) error) error {
	rows, err := r.db.Query(ctx,
		`SELECT id, tenant_id, mode, actor_type, actor_id, action,
		        resource_type, resource_id, metadata, ip_address, user_agent,
		        created_at, encode(prev_row_hash, 'hex'), encode(row_hash, 'hex')
		 FROM tenant_audit_events
		 WHERE row_hash IS NOT NULL
		 ORDER BY created_at ASC, id ASC`,
	)
	if err != nil {
		return fmt.Errorf("query audit chain: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		e := &domain.AuditEvent{}
		var metaBytes []byte
		var modeStr string
		var prevHex, rowHex string
		if err := rows.Scan(
			&e.ID, &e.TenantID, &modeStr, &e.ActorType, &e.ActorID, &e.Action,
			&e.ResourceType, &e.ResourceID, &metaBytes, &e.IPAddress, &e.UserAgent,
			&e.CreatedAt, &prevHex, &rowHex,
		); err != nil {
			return fmt.Errorf("scan audit chain row: %w", err)
		}
		e.Mode = domain.Mode(modeStr)
		if len(metaBytes) > 0 {
			if err := json.Unmarshal(metaBytes, &e.Metadata); err != nil {
				return fmt.Errorf("unmarshal audit metadata: %w", err)
			}
		}

		prev, err := ParseHashHex(prevHex)
		if err != nil {
			return fmt.Errorf("audit event %s prev_row_hash: %w", e.ID, err)
		}
		rowHash, err := ParseHashHex(rowHex)
		if err != nil {
			return fmt.Errorf("audit event %s row_hash: %w", e.ID, err)
		}

		if err := fn(ChainRow{Event: e, PrevHash: prev, RowHash: rowHash}); err != nil {
			return err
		}
	}
	return rows.Err()
}
