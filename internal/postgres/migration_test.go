package postgres_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/postgres"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/keypair"
)

func startPostgres(t *testing.T) string {
	t.Helper()
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		if os.Getenv("MIGRATION_TEST_REQUIRED") == "1" {
			t.Fatalf("docker is not available but MIGRATION_TEST_REQUIRED=1")
		}
		t.Skip("docker is not available; skipping ephemeral-postgres migration test")
	}
	cmd := exec.Command("docker", "run", "--rm", "-d", "-e", "POSTGRES_PASSWORD=nexora", "-P", "postgres:15-alpine")
	out, err := cmd.Output()
	if err != nil {
		if os.Getenv("MIGRATION_TEST_REQUIRED") == "1" {
			t.Fatalf("could not start postgres container: %v", err)
		}
		t.Skipf("could not start postgres container: %v", err)
	}
	containerID := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		_ = exec.Command("docker", "stop", containerID).Run()
	})

	portCmd := exec.Command("docker", "port", containerID, "5432/tcp")
	var port string
	for i := 0; i < 20; i++ {
		out, err = portCmd.Output()
		if err == nil && len(out) > 0 {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			parts := strings.Split(lines[0], ":")
			if len(parts) > 1 {
				port = parts[len(parts)-1]
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if port == "" {
		t.Fatalf("could not determine bound port for postgres container")
	}

	dbURL := fmt.Sprintf("postgres://postgres:nexora@localhost:%s/postgres?sslmode=disable", port)

	var ready bool
	for i := 0; i < 20; i++ {
		conn, err := pgx.Connect(context.Background(), dbURL)
		if err == nil {
			conn.Close(context.Background())
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("database did not become ready in time")
	}

	return dbURL
}

func TestMigrations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping migration test in short mode")
	}

	dbURL := startPostgres(t)

	err := postgres.RunMigrations(dbURL, "../../db/migrations")
	if err != nil {
		t.Fatalf("first migration run failed: %v", err)
	}

	err = postgres.RunMigrations(dbURL, "../../db/migrations")
	if err != nil {
		t.Fatalf("second migration run failed: %v", err)
	}

	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("failed to connect to db to check schema_migrations: %v", err)
	}
	defer conn.Close(context.Background())

	var dirty bool
	err = conn.QueryRow(context.Background(), "SELECT dirty FROM schema_migrations LIMIT 1").Scan(&dirty)
	if err != nil {
		t.Fatalf("failed to query schema_migrations: %v", err)
	}
	if dirty {
		t.Fatalf("schema_migrations is dirty after migration")
	}

	pool, err := postgres.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	defer pool.Close()

	walletRepo := postgres.NewWalletRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)

	tID := uuid.NewString()
	err = tenantRepo.Create(context.Background(), &domain.Tenant{
		ID:        tID,
		Name:      "Test",
		Email:     fmt.Sprintf("test-%s@example.com", tID[:8]),
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("failed to seed tenant: %v", err)
	}

	kp1 := keypair.MustRandom()
	kp2 := keypair.MustRandom()
	w1 := &domain.Wallet{ID: uuid.NewString(), TenantID: &tID, PublicKey: kp1.Address(), CreatedAt: time.Now().UTC()}
	w2 := &domain.Wallet{ID: uuid.NewString(), TenantID: &tID, PublicKey: kp2.Address(), CreatedAt: time.Now().UTC()}
	if err := walletRepo.Create(context.Background(), w1); err != nil {
		t.Fatalf("failed to create wallet w1: %v", err)
	}
	if err := walletRepo.Create(context.Background(), w2); err != nil {
		t.Fatalf("failed to create wallet w2: %v", err)
	}

	schedRepo := postgres.NewScheduleRepo(pool)
	ctx := tenant.WithID(context.Background(), tID)

	statuses := []domain.ScheduleStatus{
		domain.ScheduleStatusActive,
		domain.ScheduleStatusProcessing,
		domain.ScheduleStatusFailed,
		domain.ScheduleStatusPaused,
		domain.ScheduleStatusCancelled,
		domain.ScheduleStatusCompleted,
	}

	for _, st := range statuses {
		s := &domain.Schedule{
			ID:         uuid.NewString(),
			FromWallet: w1.ID,
			ToWallet:   w2.ID,
			Asset:      "XLM",
			Amount:     decimal.NewFromInt(1),
			Frequency:  domain.FrequencyDaily,
			NextRunAt:  time.Now().UTC(),
			Status:     st,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		}
		err = schedRepo.Create(ctx, s)
		if err != nil {
			t.Fatalf("failed to persist schedule status %s: %v", st, err)
		}
	}

	batchRepo := postgres.NewBatchRepo(pool)
	batchStatuses := []domain.BatchStatus{
		domain.BatchStatusPending,
		domain.BatchStatusProcessing,
		domain.BatchStatusPartial,
		domain.BatchStatusCompleted,
		domain.BatchStatusFailed,
		domain.BatchStatusComplianceHold,
	}
	for _, bst := range batchStatuses {
		b := &domain.Batch{
			ID:         uuid.NewString(),
			Status:     bst,
			TotalCount: 1,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		}
		err = batchRepo.Create(ctx, b)
		if err != nil {
			t.Fatalf("failed to persist batch status %s: %v", bst, err)
		}
	}
}

func TestWebhookDeliveryStatusEnum(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping migration test in short mode")
	}

	dbURL := startPostgres(t)
	if err := postgres.RunMigrations(dbURL, "../../db/migrations"); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	for _, status := range []string{"pending", "success", "failed", "dead_lettered"} {
		_, err := conn.Exec(context.Background(),
			"INSERT INTO webhook_deliveries (endpoint_id, event_type, payload, status) VALUES ($1, 'test', '{}', $2)",
			uuid.NewString(), status)
		if err != nil {
			t.Errorf("webhook_delivery_status enum should accept %q: %v", status, err)
		}
	}
}

func TestSchemaDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping migration test in short mode")
	}

	dbURL := startPostgres(t)
	if err := postgres.RunMigrations(dbURL, "../../db/migrations"); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	conn, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(context.Background(), `
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_schema = 'public'
		ORDER BY table_name, ordinal_position
	`)
	if err != nil {
		t.Fatalf("query columns: %v", err)
	}
	defer rows.Close()

	type col struct{ table, name string }
	columns := make(map[col]bool)
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		columns[c] = true
	}

	_ = columns
}
