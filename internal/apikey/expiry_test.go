package apikey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/go-chi/chi/v5"
)

var errNotFound = errors.New("not found")

type mockKeyRepo struct {
	keys map[string]*domain.APIKey
}

func newMockKeyRepo() *mockKeyRepo {
	return &mockKeyRepo{keys: make(map[string]*domain.APIKey)}
}

func (m *mockKeyRepo) Create(ctx context.Context, k *domain.APIKey) error {
	m.keys[k.ID] = k
	return nil
}

func (m *mockKeyRepo) GetByHash(ctx context.Context, hash string) (*domain.APIKey, error) {
	for _, k := range m.keys {
		if k.KeyHash == hash {
			return k, nil
		}
	}
	return nil, errNotFound
}

func (m *mockKeyRepo) GetByID(ctx context.Context, id, tenantID string, mode domain.Mode) (*domain.APIKey, error) {
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID || k.Mode != mode {
		return nil, errNotFound
	}
	return k, nil
}

func (m *mockKeyRepo) ListByTenant(ctx context.Context, tenantID string, mode domain.Mode) ([]*domain.APIKey, error) {
	var list []*domain.APIKey
	for _, k := range m.keys {
		if k.TenantID == tenantID && k.Mode == mode {
			list = append(list, k)
		}
	}
	return list, nil
}

func (m *mockKeyRepo) Revoke(ctx context.Context, id string, tenantID string, mode domain.Mode) error {
	k, err := m.GetByID(ctx, id, tenantID, mode)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	k.RevokedAt = &now
	return nil
}

func (m *mockKeyRepo) UpdateLastUsed(ctx context.Context, id string) error {
	k, ok := m.keys[id]
	if !ok {
		return errNotFound
	}
	now := time.Now().UTC()
	k.LastUsedAt = &now
	return nil
}

func (m *mockKeyRepo) UpdateExpiry(ctx context.Context, id, tenantID string, mode domain.Mode, expiresAt *time.Time, reminderDays int) error {
	k, err := m.GetByID(ctx, id, tenantID, mode)
	if err != nil {
		return err
	}
	k.ExpiresAt = expiresAt
	k.RotationReminderDays = reminderDays
	return nil
}

func (m *mockKeyRepo) ListExpiringKeys(ctx context.Context, limit int) ([]*domain.APIKey, error) {
	var list []*domain.APIKey
	for _, k := range m.keys {
		if k.RevokedAt == nil && k.ExpiresAt != nil {
			list = append(list, k)
		}
	}
	return list, nil
}

func (m *mockKeyRepo) RecordRotationReminder(ctx context.Context, id string, at time.Time) error {
	k, ok := m.keys[id]
	if !ok {
		return errNotFound
	}
	k.LastRotationRemindedAt = &at
	return nil
}

func TestDomainAPIKey_ExpiryAndRotation(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	future := now.Add(30 * 24 * time.Hour)
	past := now.Add(-1 * time.Hour)

	kActive := &domain.APIKey{
		ID:                   "k1",
		ExpiresAt:            &future,
		RotationReminderDays: 7,
	}
	if kActive.IsExpired(now) {
		t.Fatal("expected key not to be expired")
	}
	if kActive.NeedsRotationReminder(now) {
		t.Fatal("expected key not to need rotation reminder yet (30 days left)")
	}

	// Within rotation reminder window (5 days left vs 7 day reminder)
	reminderTime := future.Add(-5 * 24 * time.Hour)
	if !kActive.NeedsRotationReminder(reminderTime) {
		t.Fatal("expected key to need rotation reminder at 5 days before expiration")
	}

	// Already reminded within this window
	alreadyRemindedAt := reminderTime.Add(-1 * time.Hour)
	kActive.LastRotationRemindedAt = &alreadyRemindedAt
	if kActive.NeedsRotationReminder(reminderTime) {
		t.Fatal("expected key not to need duplicate rotation reminder")
	}

	// Expired key
	kExpired := &domain.APIKey{
		ID:        "k2",
		ExpiresAt: &past,
	}
	if !kExpired.IsExpired(now) {
		t.Fatal("expected past key to be expired")
	}
	if kExpired.NeedsRotationReminder(now) {
		t.Fatal("expired key should not need rotation reminder (already expired)")
	}
}

type mockWebhookDispatcher struct {
	dispatched []domain.EventType
}

func (m *mockWebhookDispatcher) Dispatch(ctx context.Context, eventType domain.EventType, payload interface{}) error {
	m.dispatched = append(m.dispatched, eventType)
	return nil
}

func TestTracker_CheckExpiringKeys(t *testing.T) {
	repo := newMockKeyRepo()
	webhooks := &mockWebhookDispatcher{}
	tracker := NewTracker(repo, webhooks, nil, time.Hour)

	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	dueExpiry := now.Add(3 * 24 * time.Hour)
	farExpiry := now.Add(60 * 24 * time.Hour)

	key1 := &domain.APIKey{
		ID:                   "key-1",
		TenantID:             "tenant-1",
		Prefix:               "sk_live_12345678",
		Mode:                 domain.ModeLive,
		ExpiresAt:            &dueExpiry,
		RotationReminderDays: 7,
		CreatedAt:            now.Add(-30 * 24 * time.Hour),
	}
	key2 := &domain.APIKey{
		ID:                   "key-2",
		TenantID:             "tenant-1",
		Prefix:               "sk_live_87654321",
		Mode:                 domain.ModeLive,
		ExpiresAt:            &farExpiry,
		RotationReminderDays: 7,
		CreatedAt:            now.Add(-10 * 24 * time.Hour),
	}

	_ = repo.Create(context.Background(), key1)
	_ = repo.Create(context.Background(), key2)

	notified := tracker.CheckExpiringKeys(context.Background(), now)
	if notified != 1 {
		t.Fatalf("expected 1 key notified, got %d", notified)
	}

	if len(webhooks.dispatched) != 1 || webhooks.dispatched[0] != domain.EventAPIKeyRotationReminder {
		t.Fatalf("expected rotation reminder event dispatched, got %v", webhooks.dispatched)
	}

	// Running again right away should not dispatch duplicates
	notifiedSecond := tracker.CheckExpiringKeys(context.Background(), now)
	if notifiedSecond != 0 {
		t.Fatalf("expected 0 duplicate notifications, got %d", notifiedSecond)
	}
}

func TestHandler_ExpiryAndRotation(t *testing.T) {
	repo := newMockKeyRepo()
	h := NewHandler(repo)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := tenant.WithID(req.Context(), "tenant-1")
			ctx = tenant.WithMode(ctx, domain.ModeLive)
			ctx = tenant.WithUser(ctx, "user-1", domain.RoleOwner)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	r.Post("/keys", h.Create)
	r.Get("/keys", h.List)
	r.Patch("/keys/{id}/expiry", h.UpdateExpiry)
	r.Post("/keys/{id}/rotate", h.Rotate)

	// 1. Create key with expiry policy
	future := time.Now().UTC().Add(30 * 24 * time.Hour)
	reminderDays := 14
	createBody, _ := json.Marshal(map[string]interface{}{
		"label":                  "CI Production Deployer",
		"role":                   "admin",
		"mode":                   "live",
		"expires_at":             future,
		"rotation_reminder_days": reminderDays,
	})

	req := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(createBody))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var created map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}

	keyID := created["id"].(string)
	rawKey := created["key"].(string)
	if rawKey == "" {
		t.Fatal("expected raw key to be returned on creation")
	}

	// 2. List keys and verify expiry fields and is_expired
	reqList := httptest.NewRequest(http.MethodGet, "/keys", nil)
	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, reqList)

	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", wList.Code, wList.Body.String())
	}

	var list []map[string]interface{}
	_ = json.Unmarshal(wList.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 key in list, got %d", len(list))
	}
	if list[0]["is_expired"].(bool) != false {
		t.Fatal("expected is_expired to be false")
	}

	// 3. Update expiry policy
	newExpiry := time.Now().UTC().Add(60 * 24 * time.Hour)
	newReminder := 10
	patchBody, _ := json.Marshal(map[string]interface{}{
		"expires_at":             newExpiry,
		"rotation_reminder_days": newReminder,
	})
	reqPatch := httptest.NewRequest(http.MethodPatch, "/keys/"+keyID+"/expiry", bytes.NewReader(patchBody))
	wPatch := httptest.NewRecorder()
	r.ServeHTTP(wPatch, reqPatch)

	if wPatch.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on patch expiry, got %d: %s", wPatch.Code, wPatch.Body.String())
	}

	// 4. Rotate key
	rotateBody, _ := json.Marshal(map[string]interface{}{
		"expires_in_days":        45,
		"rotation_reminder_days": 10,
	})
	reqRotate := httptest.NewRequest(http.MethodPost, "/keys/"+keyID+"/rotate", bytes.NewReader(rotateBody))
	wRotate := httptest.NewRecorder()
	r.ServeHTTP(wRotate, reqRotate)

	if wRotate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on rotate, got %d: %s", wRotate.Code, wRotate.Body.String())
	}

	var rotated map[string]interface{}
	_ = json.Unmarshal(wRotate.Body.Bytes(), &rotated)
	newKeyID := rotated["id"].(string)
	if newKeyID == keyID {
		t.Fatal("expected new key ID to differ from old key ID")
	}
	if rotated["previous_key_id"] != keyID {
		t.Fatalf("expected previous_key_id %s, got %v", keyID, rotated["previous_key_id"])
	}

	// Verify old key is revoked
	oldKey, err := repo.GetByID(context.Background(), keyID, "tenant-1", domain.ModeLive)
	if err != nil {
		t.Fatalf("get old key: %v", err)
	}
	if oldKey.RevokedAt == nil {
		t.Fatal("expected old key to be revoked upon rotation")
	}
}
