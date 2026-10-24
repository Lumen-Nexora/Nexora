package fees

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
)

type Handler struct {
	svc        Service
	accounting NetworkFeeAccountingService
}

func NewHandler(svc Service) *Handler {
	return &Handler{svc: svc}
}

// WithAccounting attaches the network-fee accounting engine that powers the
// tenant consumption endpoint and the admin COGS report.
func (h *Handler) WithAccounting(accounting NetworkFeeAccountingService) *Handler {
	h.accounting = accounting
	return h
}

func (h *Handler) Routes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/", h.getSchedule)
		r.Post("/preview", h.previewFee)
		r.Get("/network-consumption", h.networkConsumption)
	}
}

func (h *Handler) AdminRoutes() func(r chi.Router) {
	return func(r chi.Router) {
		r.Get("/collected", h.listCollected)
		r.Put("/schedule", h.putSchedule)
		r.Get("/network-cogs", h.networkCOGS)
	}
}

type feeScheduleResponse struct {
	TransferFeeBps   int    `json:"transfer_fee_bps"`
	ConversionFeeBps int    `json:"conversion_fee_bps"`
	MinFeeAmount     string `json:"min_fee_amount"`
	MaxFeeAmount     string `json:"max_fee_amount,omitempty"`
	Asset            string `json:"asset"`
}

func (h *Handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	tenantID := tenant.IDFromContext(r.Context())

	schedule, err := h.svc.GetSchedule(r.Context(), tenantID)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	resp := feeScheduleResponse{
		TransferFeeBps:   schedule.TransferFeeBps,
		ConversionFeeBps: schedule.ConversionFeeBps,
		MinFeeAmount:     schedule.MinFeeAmount.StringFixed(7),
		Asset:            schedule.Asset,
	}
	if schedule.MaxFeeAmount != nil {
		resp.MaxFeeAmount = schedule.MaxFeeAmount.StringFixed(7)
	}

	api.JSON(w, http.StatusOK, resp)
}

type setScheduleReq struct {
	TransferFeeBps   int    `json:"transfer_fee_bps" validate:"min=0"`
	ConversionFeeBps int    `json:"conversion_fee_bps" validate:"min=0"`
	MinFeeAmount     string `json:"min_fee_amount"`
	MaxFeeAmount     string `json:"max_fee_amount,omitempty"`
	Asset            string `json:"asset"`
	TenantID         string `json:"tenant_id,omitempty"`
}

func (h *Handler) putSchedule(w http.ResponseWriter, r *http.Request) {
	var req setScheduleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	var minFee decimal.Decimal
	if req.MinFeeAmount != "" {
		m, err := decimal.NewFromString(req.MinFeeAmount)
		if err != nil || m.LessThan(decimal.Zero) {
			api.BadRequest(w, "min_fee_amount must be a positive number")
			return
		}
		minFee = m
	}

	var maxFee *decimal.Decimal
	if req.MaxFeeAmount != "" {
		m, err := decimal.NewFromString(req.MaxFeeAmount)
		if err != nil || m.LessThan(decimal.Zero) {
			api.BadRequest(w, "max_fee_amount must be a positive number")
			return
		}
		maxFee = &m
	}

	asset := req.Asset
	if asset == "" {
		asset = "*"
	}

	var tenantID *string
	if req.TenantID != "" {
		tenantID = &req.TenantID
	}

	schedule := &domain.FeeSchedule{
		TenantID:         tenantID,
		Asset:            asset,
		TransferFeeBps:   req.TransferFeeBps,
		ConversionFeeBps: req.ConversionFeeBps,
		MinFeeAmount:     minFee,
		MaxFeeAmount:     maxFee,
	}

	if err := h.svc.SetSchedule(r.Context(), schedule); err != nil {
		api.InternalError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type previewReq struct {
	Type   string `json:"type" validate:"required,oneof=transfer conversion"`
	Asset  string `json:"asset" validate:"required"`
	Amount string `json:"amount" validate:"required"`
}

type previewResp struct {
	GrossAmount string `json:"gross_amount"`
	FeeAmount   string `json:"fee_amount"`
	NetAmount   string `json:"net_amount"`
	FeeBps      int    `json:"fee_bps"`
}

func (h *Handler) previewFee(w http.ResponseWriter, r *http.Request) {
	var req previewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		api.BadRequest(w, "invalid request body")
		return
	}

	if err := api.Validate(req); err != nil {
		api.BadRequest(w, err.Error())
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		api.BadRequest(w, "amount must be a positive number")
		return
	}

	tenantID := tenant.IDFromContext(r.Context())

	var fee *TransferFee
	if req.Type == "transfer" {
		fee, err = h.svc.CalculateTransferFee(r.Context(), tenantID, req.Asset, amount)
	} else {
		fee, err = h.svc.CalculateConversionFee(r.Context(), tenantID, req.Asset, amount)
	}

	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, previewResp{
		GrossAmount: amount.StringFixed(7),
		FeeAmount:   fee.FeeAmount.StringFixed(7),
		NetAmount:   fee.NetAmount.StringFixed(7),
		FeeBps:      fee.FeeBps,
	})
}

func (h *Handler) listCollected(w http.ResponseWriter, r *http.Request) {
	var start, end *time.Time
	if s := r.URL.Query().Get("start_date"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			api.BadRequest(w, "start_date must be RFC3339 format")
			return
		}
		start = &t
	}
	if e := r.URL.Query().Get("end_date"); e != "" {
		t, err := time.Parse(time.RFC3339, e)
		if err != nil {
			api.BadRequest(w, "end_date must be RFC3339 format")
			return
		}
		end = &t
	}

	var tenantID *string
	if tid := r.URL.Query().Get("tenant_id"); tid != "" {
		tenantID = &tid
	}

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		fmt.Sscanf(o, "%d", &offset)
	}

	collections, err := h.svc.ListCollected(r.Context(), start, end, tenantID, limit, offset)
	if err != nil {
		api.InternalError(w, err)
		return
	}

	type feeCollectionResp struct {
		ID            string `json:"id"`
		TransactionID string `json:"transaction_id"`
		TenantID      string `json:"tenant_id,omitempty"`
		Asset         string `json:"asset"`
		FeeAmount     string `json:"fee_amount"`
		FeeBps        int    `json:"fee_bps"`
		CollectedAt   string `json:"collected_at"`
	}

	var data []feeCollectionResp
	for _, c := range collections {
		resp := feeCollectionResp{
			ID:            c.ID,
			TransactionID: c.TransactionID,
			Asset:         c.Asset,
			FeeAmount:     c.FeeAmount.StringFixed(7),
			FeeBps:        c.FeeBps,
			CollectedAt:   c.CollectedAt.Format(time.RFC3339),
		}
		if c.TenantID != nil {
			resp.TenantID = *c.TenantID
		}
		data = append(data, resp)
	}
	if data == nil {
		data = []feeCollectionResp{}
	}

	api.JSON(w, http.StatusOK, map[string]interface{}{
		"data": data,
	})
}

type networkFeeUsageResponse struct {
	TenantID          string `json:"tenant_id"`
	PeriodStart       string `json:"period_start"`
	PeriodEnd         string `json:"period_end"`
	TransactionCount  int64  `json:"transaction_count"`
	FeeChargedStroops string `json:"fee_charged_stroops"`
	XLMAmount         string `json:"xlm_amount"`
	USDAmount         string `json:"usd_amount"`
	UnvaluedCount     int64  `json:"unvalued_count"`
}

func (h *Handler) networkConsumption(w http.ResponseWriter, r *http.Request) {
	if h.accounting == nil {
		api.Error(w, http.StatusServiceUnavailable, "ACCOUNTING_UNAVAILABLE", "network fee accounting is not configured")
		return
	}

	tenantID := tenant.IDFromContext(r.Context())
	if tenantID == "" {
		api.BadRequest(w, "tenant context is required")
		return
	}

	start, end, ok := parseNetworkFeePeriod(w, r)
	if !ok {
		return
	}

	usage, err := h.accounting.GetNetworkFeeConsumption(r.Context(), tenantID, start, end)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	api.JSON(w, http.StatusOK, networkFeeUsageResponse{
		TenantID:          usage.TenantID,
		PeriodStart:       usage.PeriodStart.Format(time.RFC3339),
		PeriodEnd:         usage.PeriodEnd.Format(time.RFC3339),
		TransactionCount:  usage.TransactionCount,
		FeeChargedStroops: strconv.FormatInt(usage.FeeChargedStroops, 10),
		XLMAmount:         usage.XLMAmount.StringFixed(7),
		USDAmount:         usage.USDAmount.StringFixed(8),
		UnvaluedCount:     usage.UnvaluedCount,
	})
}

type networkFeeCOGSLineResponse struct {
	TenantID          string `json:"tenant_id,omitempty"`
	SubAccount        string `json:"sub_account,omitempty"`
	CostCenter        string `json:"cost_center"`
	Allocation        string `json:"allocation"`
	TransactionCount  int64  `json:"transaction_count"`
	FeeChargedStroops string `json:"fee_charged_stroops"`
	XLMAmount         string `json:"xlm_amount"`
	USDAmount         string `json:"usd_amount"`
	UnvaluedCount     int64  `json:"unvalued_count"`
}

type networkFeeAllocationResponse struct {
	Allocation        string `json:"allocation"`
	TransactionCount  int64  `json:"transaction_count"`
	FeeChargedStroops string `json:"fee_charged_stroops"`
	XLMAmount         string `json:"xlm_amount"`
	USDAmount         string `json:"usd_amount"`
	UnvaluedCount     int64  `json:"unvalued_count"`
}

type networkFeeCOGSResponse struct {
	Month             string                         `json:"month"`
	PeriodStart       string                         `json:"period_start"`
	PeriodEnd         string                         `json:"period_end"`
	TransactionCount  int64                          `json:"transaction_count"`
	FeeChargedStroops string                         `json:"fee_charged_stroops"`
	XLMAmount         string                         `json:"xlm_amount"`
	USDAmount         string                         `json:"usd_amount"`
	UnvaluedCount     int64                          `json:"unvalued_count"`
	Allocations       []networkFeeAllocationResponse `json:"by_allocation"`
	Lines             []networkFeeCOGSLineResponse   `json:"lines"`
}

func (h *Handler) networkCOGS(w http.ResponseWriter, r *http.Request) {
	if h.accounting == nil {
		api.Error(w, http.StatusServiceUnavailable, "ACCOUNTING_UNAVAILABLE", "network fee accounting is not configured")
		return
	}

	month := time.Now().UTC()
	if value := r.URL.Query().Get("month"); value != "" {
		parsed, err := time.Parse("2006-01", value)
		if err != nil {
			api.BadRequest(w, "month must use YYYY-MM format")
			return
		}
		month = parsed
	}

	report, err := h.accounting.GetMonthlyNetworkFeeCOGS(r.Context(), month)
	if err != nil {
		api.HandleDomainError(w, err)
		return
	}

	response := networkFeeCOGSResponse{
		Month:             report.Month,
		PeriodStart:       report.PeriodStart.Format(time.RFC3339),
		PeriodEnd:         report.PeriodEnd.Format(time.RFC3339),
		TransactionCount:  report.TransactionCount,
		FeeChargedStroops: strconv.FormatInt(report.FeeChargedStroops, 10),
		XLMAmount:         report.XLMAmount.StringFixed(7),
		USDAmount:         report.USDAmount.StringFixed(8),
		UnvaluedCount:     report.UnvaluedCount,
		Allocations:       make([]networkFeeAllocationResponse, 0, len(report.Allocations)),
		Lines:             make([]networkFeeCOGSLineResponse, 0, len(report.Lines)),
	}

	for _, allocation := range report.Allocations {
		response.Allocations = append(response.Allocations, networkFeeAllocationResponse{
			Allocation:        string(allocation.Allocation),
			TransactionCount:  allocation.TransactionCount,
			FeeChargedStroops: strconv.FormatInt(allocation.FeeChargedStroops, 10),
			XLMAmount:         allocation.XLMAmount.StringFixed(7),
			USDAmount:         allocation.USDAmount.StringFixed(8),
			UnvaluedCount:     allocation.UnvaluedCount,
		})
	}
	for _, line := range report.Lines {
		item := networkFeeCOGSLineResponse{
			SubAccount:        line.SubAccount,
			CostCenter:        line.CostCenter,
			Allocation:        string(line.Allocation),
			TransactionCount:  line.TransactionCount,
			FeeChargedStroops: strconv.FormatInt(line.FeeChargedStroops, 10),
			XLMAmount:         line.XLMAmount.StringFixed(7),
			USDAmount:         line.USDAmount.StringFixed(8),
			UnvaluedCount:     line.UnvaluedCount,
		}
		if line.TenantID != nil {
			item.TenantID = *line.TenantID
		}
		response.Lines = append(response.Lines, item)
	}

	api.JSON(w, http.StatusOK, response)
}

func parseNetworkFeePeriod(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	var start, end time.Time
	if value := r.URL.Query().Get("start_date"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			api.BadRequest(w, "start_date must be RFC3339 format")
			return time.Time{}, time.Time{}, false
		}
		start = parsed
	}
	if value := r.URL.Query().Get("end_date"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			api.BadRequest(w, "end_date must be RFC3339 format")
			return time.Time{}, time.Time{}, false
		}
		end = parsed
	}
	return start, end, true
}
