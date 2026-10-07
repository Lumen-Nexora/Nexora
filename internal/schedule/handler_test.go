package schedule

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
)

type handlerService struct {
	input   CreateInput
	created int
}

func (s *handlerService) Create(_ context.Context, in CreateInput) (*domain.Schedule, error) {
	s.input = in
	s.created++
	return &domain.Schedule{
		ID: "schedule-1", FromWallet: in.FromWalletID, ToWallet: in.ToWalletID,
		Asset: in.Asset, Amount: in.Amount, Frequency: in.Frequency,
		Timezone: in.Timezone, MissedRunPolicy: in.MissedRunPolicy,
		NextRunAt: in.StartAt, Status: domain.ScheduleStatusActive,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *handlerService) List(context.Context) ([]*domain.Schedule, error) { return nil, nil }
func (s *handlerService) Update(context.Context, string, UpdateInput) (*domain.Schedule, error) {
	return nil, nil
}
func (s *handlerService) Cancel(context.Context, string) error { return nil }
func (s *handlerService) ListRuns(context.Context, string, int, int) ([]*domain.ScheduleRun, error) {
	return nil, nil
}

type handlerAudit struct {
	actions []string
}

func (a *handlerAudit) Log(_ *http.Request, action, _, _ string, _ map[string]interface{}) {
	a.actions = append(a.actions, action)
}

func newCreateScheduleRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/schedules/", strings.NewReader(body))
}

func TestHandlerCreate_DefaultsOptionalRecurrenceFieldsAndAudits(t *testing.T) {
	svc := &handlerService{}
	audit := &handlerAudit{}
	h := NewHandler(svc).WithAuditLogger(audit)
	r := chi.NewRouter()
	r.Route("/v1/schedules", h.Routes())
	body := `{"from_wallet_id":"3f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","to_wallet_id":"4f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","asset":"USDC","amount":"10","frequency":"weekly","start_date":"2026-10-01T12:00:00Z"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, newCreateScheduleRequest(body))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if svc.created != 1 {
		t.Fatalf("Create called %d times, want 1", svc.created)
	}
	if svc.input.Timezone != "UTC" || svc.input.MissedRunPolicy != domain.MissedRunPolicyRunOnce {
		t.Fatalf("defaults = (%q, %q), want (UTC, run_once)", svc.input.Timezone, svc.input.MissedRunPolicy)
	}
	if len(audit.actions) != 1 || audit.actions[0] != "schedule.created" {
		t.Fatalf("audit actions = %v, want [schedule.created]", audit.actions)
	}
	var response scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Timezone != "UTC" || response.MissedRunPolicy != "run_once" {
		t.Fatalf("response recurrence defaults = (%q, %q)", response.Timezone, response.MissedRunPolicy)
	}
}

func TestHandlerCreate_InvalidTimezoneDoesNotCreateOrAudit(t *testing.T) {
	svc := &handlerService{}
	audit := &handlerAudit{}
	h := NewHandler(svc).WithAuditLogger(audit)
	r := chi.NewRouter()
	r.Route("/v1/schedules", h.Routes())
	body := `{"from_wallet_id":"3f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","to_wallet_id":"4f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","asset":"USDC","amount":"10","frequency":"weekly","timezone":"Mars/Olympus","start_date":"2026-10-01T12:00:00Z","missed_run_policy":"skip"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, newCreateScheduleRequest(body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if svc.created != 0 || len(audit.actions) != 0 {
		t.Fatalf("invalid request side effects: creates=%d audit=%v", svc.created, audit.actions)
	}
}

func TestHandlerCreate_RejectsEndDateBeforeStartDate(t *testing.T) {
	svc := &handlerService{}
	h := NewHandler(svc)
	r := chi.NewRouter()
	r.Route("/v1/schedules", h.Routes())
	body := `{"from_wallet_id":"3f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","to_wallet_id":"4f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","asset":"USDC","amount":"10","frequency":"weekly","start_date":"2026-10-02T12:00:00Z","end_date":"2026-10-01T12:00:00Z"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, newCreateScheduleRequest(body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if svc.created != 0 {
		t.Fatalf("Create called %d times for invalid date range", svc.created)
	}
}

func TestHandlerCreate_UsesProvidedRecurrenceFields(t *testing.T) {
	svc := &handlerService{}
	h := NewHandler(svc)
	r := chi.NewRouter()
	r.Route("/v1/schedules", h.Routes())
	body := `{"from_wallet_id":"3f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","to_wallet_id":"4f1c2b8e-9a4d-4d3f-8f4e-2b6c1a9d0e77","asset":"USDC","amount":"10","frequency":"weekly","timezone":"Africa/Lagos","start_date":"2026-10-01T12:00:00Z","missed_run_policy":"skip"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, newCreateScheduleRequest(body))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if svc.input.Timezone != "Africa/Lagos" || svc.input.MissedRunPolicy != domain.MissedRunPolicySkip {
		t.Fatalf("input recurrence = (%q, %q)", svc.input.Timezone, svc.input.MissedRunPolicy)
	}
	if !svc.input.Amount.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("amount = %s, want 10", svc.input.Amount)
	}
}
