package reconcile

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/fiat/report"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/shopspring/decimal"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/protocols/horizon"
)

type threeWayRepo struct {
	txs      []*domain.Transaction
	err      error
	provider string
	refs     []string
}

func (m *threeWayRepo) ListFiatReconciliationTransfers(_ context.Context, p string, _ time.Time, refs []string) ([]*domain.Transaction, error) {
	m.provider = p
	m.refs = refs
	return m.txs, m.err
}

type threeWayChain struct {
	stellar.Client
	records map[string]horizon.Transaction
	err     error
	calls   int
}

func (m *threeWayChain) TransactionDetail(hash string) (horizon.Transaction, error) {
	m.calls++
	if m.err != nil {
		return horizon.Transaction{}, m.err
	}
	if tx, ok := m.records[hash]; ok {
		return tx, nil
	}
	return horizon.Transaction{}, &horizonclient.Error{Response: &http.Response{StatusCode: 404}}
}
func threeWayContext() context.Context {
	return tenant.WithMode(tenant.WithID(context.Background(), "tenant-1"), domain.ModeTest)
}
func threeWayTransfer(ref, hash string) *domain.Transaction {
	tenantID, rail, currency := "tenant-1", "bank", "NGN"
	amount := decimal.RequireFromString("100.01")
	return &domain.Transaction{ID: ref, TenantID: &tenantID, Mode: domain.ModeTest, FiatRail: &rail, FiatProviderRef: &ref, TxHash: hash, LocalAmount: &amount, LocalCurrency: &currency, Status: domain.StatusConfirmed}
}
func TestThreeWayPipeline(t *testing.T) {
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	repo := &threeWayRepo{txs: []*domain.Transaction{threeWayTransfer("matched", "hash-1"), threeWayTransfer("fiat-only", "absent"), threeWayTransfer("chain-only", "hash-3"), threeWayTransfer("wrong-amount", "hash-4")}}
	live := &threeWayChain{err: errors.New("must not call live network")}
	chain := &threeWayChain{records: map[string]horizon.Transaction{"hash-1": {Hash: "hash-1", Successful: true}, "hash-3": {Hash: "hash-3", Successful: true}, "hash-4": {Hash: "hash-4", Successful: true}}}
	bank, err := report.ReadSettlementReport(strings.NewReader("reference,amount,currency,status,settled_at\nmatched,100.01,NGN,settled,2026-10-22T01:00:00Z\nfiat-only,100.01,NGN,settled,2026-10-22T01:00:00Z\nwrong-amount,200,NGN,settled,2026-10-22T01:00:00Z\norphan,100.01,NGN,settled,2026-10-22T01:00:00Z\n"), day)
	if err != nil {
		t.Fatal(err)
	}
	report, err := NewThreeWayService(repo, stellar.NewModeAwareClients(live, chain)).Run(threeWayContext(), "bank", day, bank)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{"matched": "matched", "fiat-only": "fiat_settled_missing_on_chain", "chain-only": "chain_settled_missing_in_fiat", "wrong-amount": "amount_mismatch", "orphan": "missing_internal_transfer"}
	if len(report.Rows) != 5 || live.calls != 0 || repo.provider != "bank" || len(repo.refs) != 4 {
		t.Fatalf("report=%+v live calls=%d", report, live.calls)
	}
	for _, row := range report.Rows {
		if !strings.Contains(strings.Join(row.Findings, ","), expected[row.Reference]) {
			t.Fatalf("unexpected row %+v", row)
		}
		if row.Recommendation == "" {
			t.Fatal("missing recommendation")
		}
	}
}

func TestThreeWayFailuresAndAmbiguity(t *testing.T) {
	day := time.Now().UTC().Truncate(24 * time.Hour)
	tx := threeWayTransfer("duplicate", "hash")
	chain := &threeWayChain{records: map[string]horizon.Transaction{"hash": {Hash: "hash", Successful: true}}}
	repo := &threeWayRepo{txs: []*domain.Transaction{tx, threeWayTransfer("duplicate", "hash")}}
	svc := NewThreeWayService(repo, stellar.NewModeAwareClients(chain, chain))
	b := domain.BankSettlement{Reference: "duplicate", Amount: decimal.NewFromInt(1), Currency: "USD", Status: "settled", SettledAt: day}
	report, err := svc.Run(threeWayContext(), "bank", day, []domain.BankSettlement{b, b})
	if err != nil {
		t.Fatal(err)
	}
	if chain.calls != 1 || !strings.Contains(strings.Join(report.Rows[0].Findings, ","), "duplicate") {
		t.Fatalf("report %+v calls %d", report, chain.calls)
	}
	chain.err = errors.New("Horizon unavailable")
	if report, err := svc.Run(threeWayContext(), "bank", day, []domain.BankSettlement{b}); err == nil || report != nil {
		t.Fatal("outage must not generate false absence report")
	}
	if _, err := svc.Run(context.Background(), "bank", day, nil); err == nil {
		t.Fatal("must require scope")
	}
	foreign := "foreign"
	tx.TenantID = &foreign
	if _, err := svc.Run(threeWayContext(), "bank", day, nil); err == nil {
		t.Fatal("must reject foreign tenant")
	}
	ctx, cancel := context.WithCancel(threeWayContext())
	cancel()
	if _, err := svc.Run(ctx, "bank", day, nil); err == nil {
		t.Fatal("must respect cancellation")
	}
}

func TestThreeWayWorkbook(t *testing.T) {
	report := &ThreeWayReport{TenantID: "tenant-1", Provider: "bank", Day: time.Now(), Rows: []ThreeWayRow{{Reference: `=HYPERLINK("evil")<&`, Findings: []string{"missing_internal_transfer"}, Recommendation: "Investigate"}}}
	data, err := report.Workbook()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 7 {
		t.Fatalf("parts=%d", len(reader.File))
	}
	for _, file := range reader.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		decoder := xml.NewDecoder(bytes.NewReader(body))
		for {
			_, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", file.Name, err)
			}
		}
		if strings.Contains(string(body), "<f>") {
			t.Fatal("untrusted content became a formula")
		}
		if file.Name == "xl/worksheets/sheet3.xml" && !strings.Contains(string(body), "HYPERLINK") {
			t.Fatal("exception missing from workbook")
		}
	}
}

func TestThreeWaySettlementStates(t *testing.T) {
	for _, tc := range []struct {
		name, bankStatus, currency string
		successful                 bool
		status                     domain.TransactionStatus
		finding                    string
	}{
		{"failed ledger", "settled", "NGN", false, domain.StatusConfirmed, "fiat_settled_missing_on_chain"},
		{"pending bank", "pending", "NGN", true, domain.StatusConfirmed, "chain_settled_missing_in_fiat"},
		{"currency differs", "settled", "USD", true, domain.StatusConfirmed, "currency_mismatch"},
		{"internal pending", "settled", "NGN", true, domain.StatusPending, "internal_status_mismatch"},
		{"both pending", "pending", "NGN", false, domain.StatusPending, "unsettled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := threeWayTransfer("ref", "hash")
			tx.Status = tc.status
			repo := &threeWayRepo{txs: []*domain.Transaction{tx}}
			chain := &threeWayChain{records: map[string]horizon.Transaction{"hash": {Hash: "hash", Successful: tc.successful}}}
			bank := []domain.BankSettlement{{Reference: "ref", Amount: *tx.LocalAmount, Currency: tc.currency, Status: tc.bankStatus}}
			result, err := NewThreeWayService(repo, stellar.NewModeAwareClients(chain, chain)).Run(threeWayContext(), "bank", time.Now(), bank)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(result.Rows[0].Findings, ","), tc.finding) {
				t.Fatalf("row %+v", result.Rows[0])
			}
		})
	}
}
