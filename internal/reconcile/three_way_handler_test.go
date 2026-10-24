package reconcile

import (
	"context"
	"errors"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/fiat/report"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/stellar/go/protocols/horizon"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestThreeWayReportHTTPIntegration(t *testing.T) {
	repo := &threeWayRepo{txs: []*domain.Transaction{threeWayTransfer("bank-1", "hash-1")}}
	chain := &threeWayChain{records: map[string]horizon.Transaction{"hash-1": {Hash: "hash-1", Successful: true}}}
	handler := &threeWayHandler{threeWay: NewThreeWayService(repo, stellar.NewModeAwareClients(chain, chain)), readSettlement: report.ReadSettlementReport}
	csv := "reference,amount,currency,status,settled_at\nbank-1,100.01,NGN,settled,2026-10-22T00:00:00Z\n"
	run := func(ctx context.Context, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/reconciliation/three-way?date=2026-10-22&provider=bank", strings.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := run(threeWayContext(), csv)
	if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "PK") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v body %s", w.Code, w.Header(), w.Body.String())
	}
	if w := run(context.Background(), csv); w.Code != 401 {
		t.Fatalf("unscoped status %d", w.Code)
	}
	if w := run(tenant.WithID(context.Background(), "tenant-1"), csv); w.Code != 400 {
		t.Fatalf("missing mode status %d", w.Code)
	}
	if w := run(threeWayContext(), "malformed"); w.Code != 400 {
		t.Fatalf("bad report status %d", w.Code)
	}
	chain.err = errors.New("private provider error")
	if w := run(threeWayContext(), csv); w.Code != 500 || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("outage response %d %s", w.Code, w.Body.String())
	}
}
