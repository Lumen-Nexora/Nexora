package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/audit"
	"github.com/go-chi/chi/v5"
)

// LedgerSource supplies ledger entries and month-end closing rates for a
// reporting period. It is implemented by the storage layer.
type LedgerSource interface {
	LedgerEntries(r *http.Request, month string) ([]audit.LedgerEntry, error)
	ClosingRates(r *http.Request, month string) (audit.RateTable, error)
}

// ReportingHandler exposes the financial statement endpoints.
type ReportingHandler struct {
	gen    *audit.Generator
	source LedgerSource
}

func NewReportingHandler(source LedgerSource) *ReportingHandler {
	return &ReportingHandler{gen: audit.NewGenerator(), source: source}
}

// Routes mounts the reporting endpoints.
func (h *ReportingHandler) Routes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/statements/{month}", h.Statements)
	}
}

// Statements generates the balance sheet and income statement for the month
// given in the path (YYYY-MM). The format query parameter selects json (default),
// csv, or pdf.
func (h *ReportingHandler) Statements(w http.ResponseWriter, r *http.Request) {
	month := chi.URLParam(r, "month")
	period, err := audit.ParsePeriod(month)
	if err != nil {
		BadRequest(w, err.Error())
		return
	}

	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "json"
	}

	entries, err := h.source.LedgerEntries(r, month)
	if err != nil {
		InternalError(w, err)
		return
	}
	rates, err := h.source.ClosingRates(r, month)
	if err != nil {
		InternalError(w, err)
		return
	}

	stmts, err := h.gen.Generate(period, entries, rates)
	if err != nil {
		if errors.Is(err, audit.ErrStatementMissingRate) {
			UnprocessableEntity(w, "MISSING_CLOSING_RATE", err.Error())
			return
		}
		if errors.Is(err, audit.ErrStatementUnbalanced) || errors.Is(err, audit.ErrStatementInvalidEntry) {
			UnprocessableEntity(w, "LEDGER_INTEGRITY", err.Error())
			return
		}
		BadRequest(w, err.Error())
		return
	}

	data, contentType, err := stmts.Export(format)
	if err != nil {
		BadRequest(w, err.Error())
		return
	}

	w.Header().Set("Content-Type", contentType)
	if format != "json" {
		w.Header().Set("Content-Disposition", "attachment; filename=\"statements-"+month+"."+format+"\"")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
