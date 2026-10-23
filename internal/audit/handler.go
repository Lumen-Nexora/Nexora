package audit

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
)

type Handler struct {
	svc Service
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/", h.List)
		r.Get("/export", h.Export)
		r.Get("/proofs/{tx_id}", h.Proof)
		r.Get("/statements/balance-sheet", h.BalanceSheet)
		r.Get("/statements/income-statement", h.IncomeStatement)
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	filter := parseFilter(r)
	events, total, err := h.svc.List(r.Context(), filter)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"events": events,
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	filter := parseFilter(r)
	format := r.URL.Query().Get("format")
	if format == "" {
		if strings.Contains(r.Header.Get("Accept"), "text/csv") {
			format = "csv"
		} else {
			format = "json"
		}
	}

	data, contentType, err := h.svc.Export(r.Context(), filter, format)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	w.Header().Set("Content-Type", contentType)
	if format == "csv" {
		w.Header().Set("Content-Disposition", "attachment; filename=\"audit-log.csv\"")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) Proof(w http.ResponseWriter, r *http.Request) {
	txID := chi.URLParam(r, "tx_id")
	if txID == "" {
		api.BadRequest(w, "tx_id is required")
		return
	}

	proof, err := h.svc.Proof(r.Context(), txID)
	if err != nil {
		if errors.Is(err, ErrTransactionProofNotFound) || errors.Is(err, domain.ErrTransactionNotFound) {
			api.NotFound(w, err.Error())
			return
		}
		api.InternalError(w, err)
		return
	}
	api.JSON(w, http.StatusOK, proof)
}

// BalanceSheet generates a multi-currency balance sheet from the ledger.
// Query params: as_of (RFC3339, default now), currency (ISO code for consolidated USD reporting, default USD),
// format (json|csv|pdf, default json).
func (h *Handler) BalanceSheet(w http.ResponseWriter, r *http.Request) {
	asOf, err := parseAsOf(r)
	if err != nil {
		api.BadRequest(w, err.Error())
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	base := r.URL.Query().Get("currency")
	if base == "" {
		base = "USD"
	}

	stmt, err := h.svc.BalanceSheet(r.Context(), asOf, base)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	data, contentType, err := renderStatement(stmt, format)
	if err != nil {
		api.BadRequest(w, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// IncomeStatement generates an income statement for a period with fee revenue,
// network costs, and FX conversion gains/losses.
// Query params: from, to (RFC3339, required), currency (default USD), format (json|csv|pdf).
func (h *Handler) IncomeStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := time.Parse(time.RFC3339, q.Get("from"))
	if err != nil {
		api.BadRequest(w, "from must be RFC3339")
		return
	}
	to, err := time.Parse(time.RFC3339, q.Get("to"))
	if err != nil {
		api.BadRequest(w, "to must be RFC3339")
		return
	}
	if !to.After(from) {
		api.BadRequest(w, "to must be after from")
		return
	}
	format := q.Get("format")
	if format == "" {
		format = "json"
	}
	base := q.Get("currency")
	if base == "" {
		base = "USD"
	}

	stmt, err := h.svc.IncomeStatement(r.Context(), from, to, base)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	data, contentType, err := renderStatement(stmt, format)
	if err != nil {
		api.BadRequest(w, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func parseAsOf(r *http.Request) (time.Time, error) {
	s := r.URL.Query().Get("as_of")
	if s == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errors.New("as_of must be RFC3339")
	}
	return t, nil
}

func parseFilter(r *http.Request) domain.AuditFilter {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	filter := domain.AuditFilter{
		Action:       q.Get("action"),
		ActorID:      q.Get("actor"),
		ResourceType: q.Get("resource_type"),
		ResourceID:   q.Get("resource_id"),
		Limit:        limit,
		Offset:       offset,
	}

	if fromStr := q.Get("from"); fromStr != "" {
		if t, err := time.Parse(time.RFC3339, fromStr); err == nil {
			filter.From = &t
		}
	}
	if toStr := q.Get("to"); toStr != "" {
		if t, err := time.Parse(time.RFC3339, toStr); err == nil {
			filter.To = &t
		}
	}

	return filter
}
