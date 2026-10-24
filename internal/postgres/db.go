package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/dbtx"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the subset of pgxpool.Pool used by repositories. The interface now
// lives in the leaf package internal/dbtx so worker and adaptor packages can
// share the same tx helpers without importing every repository here.
type DB = dbtx.DB

// The tx helpers keep their historical postgres.* names via forwarding:
// WithTx, TxFromContext, RunInTx, and HasTx simply delegate to dbtx.
var WithTx = dbtx.WithTx
var TxFromContext = dbtx.TxFromContext
var RunInTx = dbtx.RunInTx

// HasTx reports whether ctx already carries an ambient transaction.
func HasTx(ctx context.Context) bool { return dbtx.HasTx(ctx) }

func New(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}

	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

func RunMigrations(databaseURL, migrationsPath string) error {
	m, err := migrate.New("file://"+migrationsPath, databaseURL)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}
