package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/server/idempotency"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
)

type mockRepo struct {
	mu           sync.Mutex
	records      map[string]*idempotency.Record
	completeErr  error
	completeCall int
}

func newMockRepo() *mockRepo {
	return &mockRepo{records: map[string]*idempotency.Record{}}
}

func (m *mockRepo) record(orgID, key string) *idempotency.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.records[orgID+":"+idempotency.DeterministicKey(key)]; ok {
		cp := *rec
		return &cp
	}
	return nil
}

func (m *mockRepo) Acquire(_ context.Context, orgID string, mode domain.Mode, key, requestHash string, now, leaseExpiresAt, recordExpiresAt time.Time, allowRecovery bool) (idempotency.Acquisition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := orgID + ":" + key
	if rec, ok := m.records[k]; ok {
		if !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt) {
			// Retention lapsed: the generation is replaced below.
			delete(m.records, k)
		} else {
			cp := *rec
			if cp.Status == idempotency.StatusProcessing {
				if cp.LeaseExpiresAt.IsZero() || !cp.LeaseExpiresAt.After(now) {
					if allowRecovery {
						cp.LeaseToken = uuid.New().String()
						cp.LeaseExpiresAt = leaseExpiresAt
						rec.LeaseToken = cp.LeaseToken
						rec.LeaseExpiresAt = cp.LeaseExpiresAt
					}
					return idempotency.Acquisition{State: idempotency.LeaseExpired, Record: cp}, nil
				}
				return idempotency.Acquisition{State: idempotency.InProgress, Record: cp}, nil
			}
			if cp.RequestHash != requestHash {
				return idempotency.Acquisition{State: idempotency.BodyMismatch, Record: cp}, nil
			}
			return idempotency.Acquisition{State: idempotency.Replay, Record: cp}, nil
		}
	}

	rec := &idempotency.Record{
		ID:             uuid.New().String(),
		OrgID:          orgID,
		Mode:           mode,
		Key:            key,
		RequestHash:    requestHash,
		Status:         idempotency.StatusProcessing,
		LeaseToken:     uuid.New().String(),
		LeaseExpiresAt: leaseExpiresAt,
		ExpiresAt:      recordExpiresAt,
	}
	m.records[k] = rec
	cp := *rec
	return idempotency.Acquisition{State: idempotency.Acquired, Record: cp}, nil
}

func (m *mockRepo) Complete(_ context.Context, recordID, leaseToken string, response idempotency.Response, recordExpiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completeCall++
	if m.completeErr != nil {
		return m.completeErr
	}
	for _, rec := range m.records {
		if rec.ID != recordID {
			continue
		}
		if rec.LeaseToken != leaseToken {
			return errors.New("lease lost")
		}
		rec.Status = idempotency.StatusComplete
		rec.ResponseStatus = response.Status
		rec.ResponseHeaders = response.Headers
		rec.ResponseBody = response.Body
		rec.ExpiresAt = recordExpiresAt
		rec.LeaseToken = ""
		rec.LeaseExpiresAt = time.Time{}
		return nil
	}
	return errors.New("record not found")
}

// DeleteExpired removes all records whose retention window has elapsed, up to
// batchSize rows. Returns the count of deleted records.
func (m *mockRepo) DeleteExpired(_ context.Context, batchSize int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var deleted int64
	for k, rec := range m.records {
		if !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt) {
			delete(m.records, k)
			deleted++
			if int(deleted) >= batchSize {
				break
			}
		}
	}
	return deleted, nil
}

func (m *mockRepo) Lookup(_ context.Context, orgID string, mode domain.Mode, key string) (idempotency.LookupResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := orgID + ":" + key
	if rec, ok := m.records[k]; ok {
		if !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt) {
			return idempotency.LookupResult{Found: false}, nil
		}
		if rec.Mode != "" && mode != "" && rec.Mode != mode {
			return idempotency.LookupResult{Found: false}, nil
		}
		cp := *rec
		return idempotency.LookupResult{Found: true, Record: cp}, nil
	}
	return idempotency.LookupResult{Found: false}, nil
}

func newRequest(t *testing.T, key, body string) *http.Request {
	t.Helper()
	ctx := tenant.WithID(context.Background(), "org-1")
	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", bytes.NewBufferString(body)).WithContext(ctx)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return req
}

func newXRequest(t *testing.T, key, body string) *http.Request {
	t.Helper()
	ctx := tenant.WithID(context.Background(), "org-1")
	req := httptest.NewRequest(http.MethodPost, "/v1/transfers", bytes.NewBufferString(body)).WithContext(ctx)
	if key != "" {
		req.Header.Set("X-Idempotency-Key", key)
	}
	return req
}

func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error response: %v (body=%s)", err, body)
	}
	return resp.Error.Code
}

func TestNewKey(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)

	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"tx-new","status":"pending"}`))
	}))

	rec := httptest.NewRecorder()
	key := uuid.New().String()
	h.ServeHTTP(rec, newRequest(t, key, `{"amount":"100"}`))

	if !called {
		t.Fatal("expected handler to be called for new key")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
}

func TestMissingKeyOptionalPasses(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.OptionalMiddleware(repo)

	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "", `{"amount":"10"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for optional missing key, got %d", rec.Code)
	}
	if !called {
		t.Fatal("handler should run when idempotency key is optional and omitted")
	}
}

func TestMissingKeyRequiredReturns400(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.RequiredMiddleware(repo)

	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "", `{"a":1}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("expected IDEMPOTENCY_KEY_REQUIRED, got %s", code)
	}
	if called {
		t.Fatal("handler should not run when required key is missing")
	}
}

func TestXIdempotencyKeyHeader(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)

	callCount := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"tx-x-key","status":"pending"}`))
	}))

	key := "custom-client-tx-" + uuid.New().String()
	body := `{"from_wallet_id":"a","to_wallet_id":"b","amount":"10"}`

	first := httptest.NewRecorder()
	h.ServeHTTP(first, newXRequest(t, key, body))

	second := httptest.NewRecorder()
	h.ServeHTTP(second, newXRequest(t, key, body))

	if callCount != 1 {
		t.Fatalf("expected handler to run once for duplicate X-Idempotency-Key, ran %d times", callCount)
	}
	if first.Code != second.Code || first.Body.String() != second.Body.String() {
		t.Fatalf("replayed responses differ: first=%s second=%s", first.Body.String(), second.Body.String())
	}
}

func TestSameKeySameBodyReplaysResponseByteForByte(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)

	callCount := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"tx-1","status":"pending"}`))
	}))

	key := uuid.New().String()
	body := `{"from_wallet_id":"a","to_wallet_id":"b","asset":"XLM","amount":"10"}`

	first := httptest.NewRecorder()
	h.ServeHTTP(first, newRequest(t, key, body))

	second := httptest.NewRecorder()
	h.ServeHTTP(second, newRequest(t, key, body))

	if callCount != 1 {
		t.Fatalf("expected handler to run exactly once, ran %d times", callCount)
	}
	if first.Code != second.Code || first.Body.String() != second.Body.String() {
		t.Fatalf("responses differ: first=%d %q second=%d %q", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if second.Code != http.StatusAccepted || second.Body.String() != `{"id":"tx-1","status":"pending"}` {
		t.Fatalf("unexpected replayed response: %d %s", second.Code, second.Body.String())
	}
}

func TestSameKeyDifferentBodyReturns409(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	key := uuid.New().String()
	first := httptest.NewRecorder()
	h.ServeHTTP(first, newRequest(t, key, `{"amount":"10"}`))

	second := httptest.NewRecorder()
	h.ServeHTTP(second, newRequest(t, key, `{"amount":"20"}`))

	// Issue #151: Return 409 Conflict if the same key is used with a different request body
	if second.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", second.Code)
	}
	if code := decodeErrorCode(t, second.Body.Bytes()); code != "IDEMPOTENCY_KEY_REUSED_WITH_DIFFERENT_BODY" {
		t.Fatalf("expected IDEMPOTENCY_KEY_REUSED_WITH_DIFFERENT_BODY, got %s", code)
	}
}

func TestExpiredKeyAllowsNewExecution(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)

	callCount := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"count":` + string(rune('0'+callCount)) + `}`))
	}))

	key := uuid.New().String()
	body := `{"amount":"50"}`

	// First execution
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newRequest(t, key, body))
	if callCount != 1 {
		t.Fatalf("expected 1 execution, got %d", callCount)
	}

	// Manually age the record past 24 hours to simulate TTL expiration
	k := "org-1:" + idempotency.DeterministicKey(key)
	repo.mu.Lock()
	if rec, ok := repo.records[k]; ok {
		rec.ExpiresAt = time.Now().Add(-1 * time.Hour) // expired in the past
	}
	repo.mu.Unlock()

	// Second execution with expired key should run as a new key
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newRequest(t, key, body))

	if callCount != 2 {
		t.Fatalf("expected handler to re-run after key expiration, got %d calls", callCount)
	}
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec2.Code)
	}
}

func TestDeterministicKeySHA256(t *testing.T) {
	rawString := "order_transfer_ref_99999"
	k1 := idempotency.DeterministicKey(rawString)
	k2 := idempotency.DeterministicKey(rawString)

	if k1 != k2 {
		t.Fatalf("expected deterministic keys to be identical, got %q and %q", k1, k2)
	}

	// Must be a valid UUID
	parsed, err := uuid.Parse(k1)
	if err != nil {
		t.Fatalf("expected valid UUID from deterministic key, got error: %v", err)
	}
	if parsed.Version() != 4 {
		t.Fatalf("expected UUID v4 format, got version %d", parsed.Version())
	}
}

func TestConcurrentRequestInProgressReturns409(t *testing.T) {
	repo := newMockRepo()
	key := uuid.New().String()
	dk := idempotency.DeterministicKey(key)
	repo.records["org-1:"+dk] = &idempotency.Record{
		ID:             uuid.New().String(),
		OrgID:          "org-1",
		Key:            dk,
		Status:         idempotency.StatusProcessing,
		LeaseToken:     uuid.New().String(),
		LeaseExpiresAt: time.Now().Add(time.Minute),
	}

	mw := idempotency.Middleware(repo)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, key, `{"amount":"10"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "REQUEST_IN_PROGRESS" {
		t.Fatalf("expected REQUEST_IN_PROGRESS, got %s", code)
	}
	if called {
		t.Fatal("handler should not run for a request that lost the race")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After to be set while a lease is held")
	}
}

// TestStaleLeaseIsRecoveredWithRecoveryEnabled covers the #192 recovery path:
// a request whose process died leaves a processing row behind. Once its lease
// lapses, a retry that opted into recovery may re-run the handler instead of
// being told forever that the request is in progress.
func TestStaleLeaseIsRecoveredWithRecoveryEnabled(t *testing.T) {
	repo := newMockRepo()
	key := uuid.New().String()
	dk := idempotency.DeterministicKey(key)
	repo.records["org-1:"+dk] = &idempotency.Record{
		ID:             uuid.New().String(),
		OrgID:          "org-1",
		Key:            dk,
		Status:         idempotency.StatusProcessing,
		LeaseToken:     uuid.New().String(),
		LeaseExpiresAt: time.Now().Add(-time.Minute),
	}

	mw := idempotency.MiddlewareWithOptions(repo, idempotency.Options{AllowLeaseRecovery: true})
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, key, `{"amount":"10"}`))

	if !called {
		t.Fatal("expected the handler to re-run after the lease expired")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if stored := repo.record("org-1", key); stored == nil || stored.Status != idempotency.StatusComplete {
		t.Fatal("expected the recovered request to be recorded as complete")
	}
}

// TestResponseIsBufferedUntilDurable asserts the core #192 guarantee: when the
// idempotency record cannot be persisted, the client must not receive the
// handler's success response.
func TestResponseIsBufferedUntilDurable(t *testing.T) {
	repo := newMockRepo()
	repo.completeErr = errors.New("database unavailable")

	mw := idempotency.Middleware(repo)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"tx-dup","status":"pending"}`))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, uuid.New().String(), `{"amount":"10"}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the idempotency record cannot be persisted, got %d", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "IDEMPOTENCY_RECORD_PERSISTENCE_FAILED" {
		t.Fatalf("expected IDEMPOTENCY_RECORD_PERSISTENCE_FAILED, got %s", code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("tx-dup")) {
		t.Fatal("handler response leaked to the client before the record was durable")
	}
}

func TestReplayRestoresHandlerHeaders(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Nexora-Test", "preserved")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tx-h"}`))
	}))

	key := uuid.New().String()
	body := `{"amount":"7"}`
	h.ServeHTTP(httptest.NewRecorder(), newRequest(t, key, body))

	second := httptest.NewRecorder()
	h.ServeHTTP(second, newRequest(t, key, body))

	if second.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", second.Code)
	}
	if second.Header().Get("X-Nexora-Test") != "preserved" {
		t.Fatalf("expected handler header to be replayed, got %q", second.Header().Get("X-Nexora-Test"))
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("expected replayed response to be marked with Idempotency-Replayed")
	}
}

// TestHandlerCanReadDurableRecordID asserts the transfer service can fence its
// own persistence to the exact idempotency generation that owns the request.
func TestHandlerCanReadDurableRecordID(t *testing.T) {
	repo := newMockRepo()
	var got string
	h := idempotency.Middleware(repo)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = idempotency.RecordIDFromContext(r.Context())
		w.WriteHeader(http.StatusCreated)
	}))
	key := uuid.New().String()
	h.ServeHTTP(httptest.NewRecorder(), newRequest(t, key, `{}`))
	if got == "" {
		t.Fatal("handler did not receive idempotency record ID")
	}
	if stored := repo.record("org-1", key); stored == nil || stored.ID != got {
		t.Fatal("handler record ID does not match durable record")
	}
}

func TestHandlerStillReceivesRequestBody(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)

	var received string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		received = buf.String()
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"amount":"42"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, uuid.New().String(), body))

	if received != body {
		t.Fatalf("expected handler to read original body %q, got %q", body, received)
	}
}

// TestDeleteExpiredPurgesOnceUsedKey asserts the acceptance criterion:
// a key used exactly once and never repeated must be removed by DeleteExpired
// once its TTL has elapsed.
func TestDeleteExpiredPurgesOnceUsedKey(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"tx-once","status":"pending"}`))
	}))

	key := uuid.New().String()
	dk := idempotency.DeterministicKey(key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, key, `{"amount":"10"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}

	// Verify the record is present before expiry.
	repo.mu.Lock()
	_, present := repo.records["org-1:"+dk]
	repo.mu.Unlock()
	if !present {
		t.Fatal("expected idempotency record to exist before expiry")
	}

	// Age the record so it is past its TTL.
	repo.mu.Lock()
	if r, ok := repo.records["org-1:"+dk]; ok {
		r.ExpiresAt = time.Now().Add(-1 * time.Second)
	}
	repo.mu.Unlock()

	// The key is never retried, so no opportunistic delete ever fires.
	// DeleteExpired must remove it.
	n, err := repo.DeleteExpired(context.Background(), 1000)
	if err != nil {
		t.Fatalf("DeleteExpired returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted row, got %d", n)
	}

	repo.mu.Lock()
	_, stillPresent := repo.records["org-1:"+dk]
	repo.mu.Unlock()
	if stillPresent {
		t.Fatal("expected idempotency record to be removed after DeleteExpired")
	}
}

// TestDeleteExpiredLeavesLiveRecordsAlone asserts that DeleteExpired does not
// remove records that are still within their TTL window.
func TestDeleteExpiredLeavesLiveRecordsAlone(t *testing.T) {
	repo := newMockRepo()
	mw := idempotency.Middleware(repo)

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	key := uuid.New().String()
	dk := idempotency.DeterministicKey(key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, key, `{"amount":"5"}`))

	// Record is fresh — DeleteExpired must not touch it.
	n, err := repo.DeleteExpired(context.Background(), 1000)
	if err != nil {
		t.Fatalf("DeleteExpired returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 deleted rows for live record, got %d", n)
	}

	repo.mu.Lock()
	_, present := repo.records["org-1:"+dk]
	repo.mu.Unlock()
	if !present {
		t.Fatal("expected live idempotency record to remain after DeleteExpired")
	}
}

func TestEncryptedResponseIsEncryptedAtRestAndReplayed(t *testing.T) {
	repo := newMockRepo()
	key := uuid.New().String()
	secret := "whsec-one-time-plaintext"
	responseBody := []byte(`{"secret":"` + secret + `"}`)
	mw := idempotency.MiddlewareWithOptions(repo, idempotency.Options{
		Required:              true,
		ResponseEncryptionKey: bytes.Repeat([]byte{7}, 32),
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))

	first := httptest.NewRecorder()
	h.ServeHTTP(first, newRequest(t, key, `{}`))
	if !bytes.Equal(first.Body.Bytes(), responseBody) {
		t.Fatalf("first response = %s, want original plaintext response", first.Body.Bytes())
	}
	stored := repo.record("org-1", key)
	if stored == nil || bytes.Contains(stored.ResponseBody, []byte(secret)) {
		t.Fatal("durable idempotency response must not contain plaintext secret material")
	}

	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, newRequest(t, key, `{}`))
	if replay.Code != http.StatusOK || !bytes.Equal(replay.Body.Bytes(), responseBody) {
		t.Fatalf("replayed response = %d %s, want original response", replay.Code, replay.Body.Bytes())
	}
}
