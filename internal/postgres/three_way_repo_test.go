package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/jackc/pgx/v5"
)

func TestThreeWayRepoPostgres(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx := tenant.WithMode(tenant.WithID(context.Background(), "tenant-1"), domain.ModeTest)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// A connection-local table avoids modifying any real tenant data.
	_, err = conn.Exec(ctx, `CREATE TEMP TABLE transactions (id text,tx_hash text,status text,fiat_provider_ref text,local_currency text,local_amount numeric,tenant_id text,mode text,fiat_rail text,created_at timestamptz)`)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	rows := []struct {
		id, tenant, mode, provider, ref string
		at                              time.Time
	}{
		{"today", "tenant-1", "test", "bank", "today", day},
		{"late", "tenant-1", "test", "bank", "late", day.AddDate(0, 0, -5)},
		{"old", "tenant-1", "test", "bank", "old", day.AddDate(0, 0, -5)},
		{"tomorrow", "tenant-1", "test", "bank", "tomorrow", day.AddDate(0, 0, 1)},
		{"foreign", "tenant-2", "test", "bank", "late", day},
		{"live", "tenant-1", "live", "bank", "late", day},
		{"other-bank", "tenant-1", "test", "other", "late", day},
	}
	for _, row := range rows {
		_, err = conn.Exec(ctx, `INSERT INTO transactions VALUES ($1,'hash','confirmed',$2,'NGN',100.0100000,$3,$4,$5,$6)`, row.id, row.ref, row.tenant, row.mode, row.provider, row.at)
		if err != nil {
			t.Fatal(err)
		}
	}
	repo := NewThreeWayRepo(conn)
	found, err := repo.ListFiatReconciliationTransfers(ctx, "bank", day, []string{"late"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].ID != "late" || found[1].ID != "today" || found[0].LocalAmount.String() != "100.01" {
		t.Fatalf("unexpected rows %+v", found)
	}
	found, err = repo.ListFiatReconciliationTransfers(ctx, "bank", day, nil)
	if err != nil || len(found) != 1 || found[0].ID != "today" {
		t.Fatalf("empty bank report rows %+v err %v", found, err)
	}
	if _, err = repo.ListFiatReconciliationTransfers(context.Background(), "bank", day, nil); err == nil {
		t.Fatal("missing tenant/mode accepted")
	}
	_, err = conn.Exec(ctx, `INSERT INTO transactions SELECT 'extra-'||n,'hash','confirmed','extra','NGN',1,'tenant-1','test','bank',$1 FROM generate_series(1,10001) n`, day)
	if err != nil {
		t.Fatal(err)
	}
	if found, err = repo.ListFiatReconciliationTransfers(ctx, "bank", day, nil); err == nil || found != nil {
		t.Fatal("oversized result must not silently truncate")
	}
}
