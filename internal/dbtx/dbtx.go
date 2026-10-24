// Package dbtx holds the low-level database plumbing shared by every layer:
// the narrow pgx interface repositories depend on and the transaction
// helpers used to join or start a transaction. Keeping it leaf-level lets
// service, worker, and adaptor packages share the exact same tx semantics
// without importing the whole postgres package.
package dbtx

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the subset of pgxpool.Pool used by repositories. Keeping the
// dependency narrow allows repositories to route reads independently of
// writes.
type DB interface {
	Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error)
	Query(context.Context, string, ...interface{}) (pgx.Rows, error)
	QueryRow(context.Context, string, ...interface{}) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

type txKey struct{}

func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func TxFromContext(ctx context.Context, defaultDB DB) DB {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return defaultDB
}

// HasTx reports whether ctx already carries an ambient transaction.
func HasTx(ctx context.Context) bool {
	return TxFromContext(ctx, nil) != nil
}

func RunInTx(ctx context.Context, db DB, fn func(context.Context) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}

	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(WithTx(ctx, tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
