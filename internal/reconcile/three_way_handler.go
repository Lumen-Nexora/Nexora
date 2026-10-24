package reconcile

import (
	"io"
	"net/http"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
)

type SettlementReportReader func(io.Reader, time.Time) ([]domain.BankSettlement, error)

type threeWayHandler struct {
	threeWay       *ThreeWayService
	readSettlement SettlementReportReader
	audit          interface {
		Log(*http.Request, string, string, string, map[string]interface{})
	}
}

func (h *threeWayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if tenant.IDFromContext(r.Context()) == "" {
		api.Error(w, http.StatusUnauthorized, "TENANT_REQUIRED", "tenant context is required")
		return
	}
	if _, ok := tenant.ModeFromContext(r.Context()); !ok {
		api.BadRequest(w, "mode context is required")
		return
	}
	if h.threeWay == nil || h.readSettlement == nil {
		api.Error(w, http.StatusServiceUnavailable, "RECONCILIATION_UNAVAILABLE", "three-way reconciliation is unavailable")
		return
	}
	day, err := time.Parse("2006-01-02", r.URL.Query().Get("date"))
	provider := r.URL.Query().Get("provider")
	if err != nil || provider == "" || len(provider) > 64 {
		api.BadRequest(w, "date (YYYY-MM-DD) and provider are required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	bank, err := h.readSettlement(r.Body, day)
	if err != nil {
		api.BadRequest(w, err.Error())
		return
	}
	report, err := h.threeWay.Run(r.Context(), provider, day, bank)
	if err != nil {
		api.InternalError(w, err)
		return
	}
	data, err := report.Workbook()
	if err != nil {
		api.InternalError(w, err)
		return
	}
	if h.audit != nil {
		h.audit.Log(r, "reconciliation.three_way", "reconciliation_report", "", map[string]interface{}{"date": day.Format("2006-01-02"), "provider": provider, "rows": len(report.Rows)})
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="reconciliation-`+day.Format("2006-01-02")+`.xlsx"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
