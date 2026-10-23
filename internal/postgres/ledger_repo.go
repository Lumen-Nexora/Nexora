package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/shopspring/decimal"

	"nexora/internal/reconcile"
)

// LedgerRepo persists double-entry journal entries. Writes run in a single
// transaction; the deferred database trigger rejects any unbalanced entry at
// commit, and append-only triggers block UPDATE/DELETE on posted rows.
type LedgerRepo struct {
	db *sql.DB
}

func NewLedgerRepo(db *sql.DB) *LedgerRepo {
	return &LedgerRepo{db: db}
}

// PostEntry validates and atomically inserts a journal entry with its postings.
func (r *LedgerRepo) PostEntry(ctx context.Context, e reconcile.JournalEntry, prevHash string) (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	hash := e.ContentHash(prevHash)
	var entryID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO journal_entries (tenant_id, description, currency, posted_at, content_hash)
		 VALUES ($1, $2, $3, COALESCE(NULLIF($4::timestamptz, '0001-01-01'::timestamptz), NOW()), $5)
		 RETURNING id`,
		e.TenantID, e.Description, e.Currency, e.PostedAt, hash,
	).Scan(&entryID)
	if err != nil {
		return "", fmt.Errorf("insert journal entry: %w", err)
	}

	for _, p := range e.Postings {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_postings (journal_entry_id, account_id, direction, amount)
			 VALUES ($1, $2, $3, $4)`,
			entryID, p.AccountID, string(p.Direction), p.Amount.String(),
		)
		if err != nil {
			return "", fmt.Errorf("insert posting: %w", err)
		}
	}

	// Deferred constraint trigger verifies balance at commit.
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit journal entry: %w", err)
	}
	return entryID, nil
}

// AccountBalance returns the running balance of a ledger account using exact
// decimal arithmetic over all postings.
func (r *LedgerRepo) AccountBalance(ctx context.Context, accountID, accountType string) (decimal.Decimal, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT direction, amount::text FROM ledger_postings WHERE account_id = $1`, accountID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("query postings: %w", err)
	}
	defer rows.Close()

	var postings []reconcile.Posting
	for rows.Next() {
		var dir, amt string
		if err := rows.Scan(&dir, &amt); err != nil {
			return decimal.Zero, fmt.Errorf("scan posting: %w", err)
		}
		d, err := decimal.NewFromString(amt)
		if err != nil {
			return decimal.Zero, fmt.Errorf("parse amount: %w", err)
		}
		postings = append(postings, reconcile.Posting{
			AccountID: accountID,
			Direction: reconcile.Direction(dir),
			Amount:    d,
		})
	}
	if err := rows.Err(); err != nil {
		return decimal.Zero, err
	}
	return reconcile.AccountBalance(accountType, postings), nil
}

// TenantZeroSum returns the net debit-minus-credit across all postings for a
// tenant's journal entries. A correct ledger always returns zero.
func (r *LedgerRepo) TenantZeroSum(ctx context.Context, tenantID string) (decimal.Decimal, error) {
	var dr, cr sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(p.amount) FILTER (WHERE p.direction = 'debit'), 0)::text,
		   COALESCE(SUM(p.amount) FILTER (WHERE p.direction = 'credit'), 0)::text
		 FROM ledger_postings p
		 JOIN journal_entries j ON j.id = p.journal_entry_id
		 WHERE j.tenant_id = $1`, tenantID,
	).Scan(&dr, &cr)
	if err != nil {
		return decimal.Zero, fmt.Errorf("zero-sum query: %w", err)
	}
	debits, err := decimal.NewFromString(dr.String)
	if err != nil {
		return decimal.Zero, err
	}
	credits, err := decimal.NewFromString(cr.String)
	if err != nil {
		return decimal.Zero, err
	}
	return debits.Sub(credits), nil
}
