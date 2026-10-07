package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
)

// mockConfigRepo is an in-memory ConfigRepository keyed by tenant so the
// tenant-isolation assertions below are meaningful.
type mockConfigRepo struct {
	mu         sync.Mutex
	configs    map[string]*domain.TenantWebhookConfig
	deliveries map[string]*domain.TenantWebhookDelivery
	secrets    map[string]map[string]*domain.WebhookSigningSecret
	values     map[string]map[string]string
}

func newMockConfigRepo() *mockConfigRepo {
	return &mockConfigRepo{
		configs:    make(map[string]*domain.TenantWebhookConfig),
		deliveries: make(map[string]*domain.TenantWebhookDelivery),
		secrets:    make(map[string]map[string]*domain.WebhookSigningSecret),
		values:     make(map[string]map[string]string),
	}
}

func (m *mockConfigRepo) ListSigningSecrets(_ context.Context, tenantID string) ([]*domain.WebhookSigningSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*domain.WebhookSigningSecret
	for _, secret := range m.secrets[tenantID] {
		copied := *secret
		if copied.Status == "overlapping" && copied.RetiredAt != nil && !time.Now().Before(*copied.RetiredAt) {
			copied.Status = "retired"
			secret.Status = "retired"
		}
		out = append(out, &copied)
	}
	return out, nil
}

func (m *mockConfigRepo) ImportLegacySigningSecret(_ context.Context, tenantID, keyID, encryptedSecret string, now time.Time) (*domain.WebhookSigningSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, ok := m.configs[tenantID]
	if !ok {
		return nil, domain.ErrWebhookConfigNotFound
	}
	if config.SigningKeyID != "" {
		if metadata := m.secrets[tenantID][config.SigningKeyID]; metadata != nil {
			copied := *metadata
			return &copied, nil
		}
	}
	if config.Secret == "" {
		return nil, nil
	}
	if m.secrets[tenantID] == nil {
		m.secrets[tenantID] = make(map[string]*domain.WebhookSigningSecret)
		m.values[tenantID] = make(map[string]string)
	}
	metadata := &domain.WebhookSigningSecret{KeyID: keyID, CreatedAt: now, ActivatedAt: now, Status: "active"}
	m.secrets[tenantID][keyID] = metadata
	m.values[tenantID][keyID] = encryptedSecret
	config.Secret = encryptedSecret
	config.SigningKeyID = keyID
	for _, delivery := range m.deliveries {
		if delivery.TenantID == tenantID && delivery.SigningKeyID == "" {
			delivery.SigningKeyID = keyID
		}
	}
	return metadata, nil
}

func (m *mockConfigRepo) RotateSigningSecret(_ context.Context, tenantID, keyID, encryptedSecret, legacyKeyID, legacyEncryptedSecret string, overlap time.Duration, now time.Time) (*domain.WebhookSigningSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.secrets[tenantID] {
		if existing.Status == "active" {
			if overlap > 0 {
				existing.Status = "overlapping"
				retireAt := now.Add(overlap)
				existing.RetiredAt = &retireAt
			} else {
				existing.Status = "retired"
				retireAt := now
				existing.RetiredAt = &retireAt
			}
		}
	}
	if m.secrets[tenantID] == nil {
		m.secrets[tenantID] = make(map[string]*domain.WebhookSigningSecret)
		m.values[tenantID] = make(map[string]string)
	}
	if legacyKeyID != "" && legacyEncryptedSecret != "" {
		legacyStatus := "retired"
		retiredAt := now
		if overlap > 0 {
			legacyStatus = "overlapping"
			retiredAt = now.Add(overlap)
		}
		m.secrets[tenantID][legacyKeyID] = &domain.WebhookSigningSecret{
			KeyID: legacyKeyID, CreatedAt: now, ActivatedAt: now, RetiredAt: &retiredAt, Status: legacyStatus,
		}
		m.values[tenantID][legacyKeyID] = legacyEncryptedSecret
	}
	metadata := &domain.WebhookSigningSecret{KeyID: keyID, CreatedAt: now, ActivatedAt: now, Status: "active"}
	m.secrets[tenantID][keyID] = metadata
	m.values[tenantID][keyID] = encryptedSecret
	config := m.configs[tenantID]
	if config == nil {
		config = &domain.TenantWebhookConfig{TenantID: tenantID, CreatedAt: now}
		m.configs[tenantID] = config
	}
	config.Secret = encryptedSecret
	config.SigningKeyID = keyID
	return metadata, nil
}

func (m *mockConfigRepo) GetSigningSecret(_ context.Context, tenantID, keyID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	secret, ok := m.values[tenantID][keyID]
	if !ok {
		return "", domain.ErrWebhookConfigNotFound
	}
	return secret, nil
}

func (m *mockConfigRepo) GetConfig(_ context.Context, tenantID string) (*domain.TenantWebhookConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, ok := m.configs[tenantID]
	if !ok {
		return nil, domain.ErrWebhookConfigNotFound
	}
	copied := *config
	copied.Events = append([]string{}, config.Events...)
	return &copied, nil
}

func (m *mockConfigRepo) UpsertConfig(_ context.Context, config *domain.TenantWebhookConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *config
	copied.Events = append([]string{}, config.Events...)
	m.configs[config.TenantID] = &copied
	return nil
}

func (m *mockConfigRepo) ListEnabledConfigs(_ context.Context) ([]*domain.TenantWebhookConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*domain.TenantWebhookConfig
	for _, config := range m.configs {
		if config.Enabled {
			copied := *config
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (m *mockConfigRepo) ListConfigs(_ context.Context) ([]*domain.TenantWebhookConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	configs := make([]*domain.TenantWebhookConfig, 0, len(m.configs))
	for _, config := range m.configs {
		copied := *config
		copied.Events = append([]string{}, config.Events...)
		configs = append(configs, &copied)
	}
	return configs, nil
}

func (m *mockConfigRepo) CreateConfigDelivery(_ context.Context, delivery *domain.TenantWebhookDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *delivery
	m.deliveries[delivery.ID] = &copied
	return nil
}

func (m *mockConfigRepo) UpdateConfigDelivery(_ context.Context, delivery *domain.TenantWebhookDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *delivery
	m.deliveries[delivery.ID] = &copied
	return nil
}

func (m *mockConfigRepo) GetConfigDelivery(_ context.Context, id, tenantID string) (*domain.TenantWebhookDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delivery, ok := m.deliveries[id]
	if !ok || delivery.TenantID != tenantID {
		return nil, domain.ErrWebhookDeliveryNotFound
	}
	copied := *delivery
	return &copied, nil
}

func (m *mockConfigRepo) ListConfigDeliveries(_ context.Context, tenantID string, limit, offset int) ([]*domain.TenantWebhookDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []*domain.TenantWebhookDelivery
	for _, delivery := range m.deliveries {
		if delivery.TenantID == tenantID {
			all = append(all, delivery)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	if offset >= len(all) {
		return nil, nil
	}
	all = all[offset:]
	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}
	return all, nil
}

func (m *mockConfigRepo) UpdateConfigLastDelivered(_ context.Context, tenantID string, deliveredAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if config, ok := m.configs[tenantID]; ok {
		at := deliveredAt
		config.LastDeliveredAt = &at
	}
	return nil
}

// newConfigTestService returns a service wired to the config repo, with SSRF
// destination checks relaxed so it can target loopback httptest servers.
func newConfigTestService(t *testing.T, configRepo ConfigRepository) *service {
	t.Helper()
	svc := NewConfigService(nil, configRepo, nil).(*service)
	svc.allowPrivateNetworks = true
	return svc
}

func tenantCtx(tenantID string) context.Context {
	return tenant.WithID(context.Background(), tenantID)
}

func boolPtr(v bool) *bool            { return &v }
func strPtr(v string) *string         { return &v }
func eventsPtr(v ...string) *[]string { return &v }

// unreachableURL is a syntactically valid, loopback-resolving URL that is never
// dialled. It lets the config-only tests exercise URL validation and storage
// without depending on external DNS or a live server.
func unreachableURL(path string) string {
	return "http://127.0.0.1:1" + path
}

func TestUpdateConfig_CreatesAndRevealsSecretOnce(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	result, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{
		Enabled: boolPtr(true),
		URL:     strPtr(unreachableURL("/hook")),
		Events:  eventsPtr("transfer.settled"),
	})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if result.Secret == "" {
		t.Fatal("expected the generated secret to be returned on creation")
	}
	if result.Config.SigningAlgorithm != defaultSigningAlgorithm {
		t.Fatalf("signing_algorithm = %q, want %q", result.Config.SigningAlgorithm, defaultSigningAlgorithm)
	}
	if !result.Config.Enabled {
		t.Fatal("expected config to be enabled")
	}

	// A subsequent unrelated update must not re-reveal the secret.
	second, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{URL: strPtr(unreachableURL("/hook2"))})
	if err != nil {
		t.Fatalf("second UpdateConfig() error: %v", err)
	}
	if second.Secret != "" {
		t.Fatal("secret must only be revealed at creation or rotation")
	}
	if second.Config.Secret != "" {
		t.Fatal("the returned config must never carry the secret")
	}
	if !second.Config.SecretConfigured {
		t.Fatal("secret_configured must report that a secret exists without revealing it")
	}
	if second.Config.URL != unreachableURL("/hook2") {
		t.Fatalf("URL = %q, want %s", second.Config.URL, unreachableURL("/hook2"))
	}

	// ...but it is still stored, so signatures keep working.
	stored, err := repo.GetConfig(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("GetConfig() error: %v", err)
	}
	if stored.Secret == result.Secret || !strings.HasPrefix(stored.Secret, "v1::") {
		t.Fatal("the stored secret must be encrypted at rest")
	}
	decrypted, err := svc.decryptSecret(stored.Secret)
	if err != nil || decrypted != result.Secret {
		t.Fatal("the encrypted secret must remain usable for signing")
	}

	// Reading the config back must not expose the secret either.
	readBack, err := svc.GetConfig(ctx)
	if err != nil {
		t.Fatalf("svc.GetConfig() error: %v", err)
	}
	if readBack.Secret != "" {
		t.Fatal("GetConfig() must not return the secret")
	}
	if !readBack.SecretConfigured {
		t.Fatal("GetConfig() should report secret_configured")
	}
}

func TestUpdateConfig_SecretRotation(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	first, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{URL: strPtr(unreachableURL("/a"))})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}

	rotated, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{RotateSecret: true})
	if err != nil {
		t.Fatalf("rotate error: %v", err)
	}
	if rotated.Secret == "" {
		t.Fatal("expected a new secret on rotation")
	}
	if rotated.Secret == first.Secret {
		t.Fatal("expected the rotated secret to differ from the original")
	}
}

func TestRotateSigningSecret_OverlapExpires(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")
	first, _, err := svc.RotateSigningSecret(ctx, 0)
	if err != nil {
		t.Fatalf("initial rotation: %v", err)
	}
	second, _, err := svc.RotateSigningSecret(ctx, time.Minute)
	if err != nil {
		t.Fatalf("rotation with overlap: %v", err)
	}
	third, _, err := svc.RotateSigningSecret(ctx, 2*time.Minute)
	if err != nil {
		t.Fatalf("subsequent rotation with overlap: %v", err)
	}
	secrets, err := svc.ListSigningSecrets(ctx)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(secrets) != 3 {
		t.Fatalf("secret versions = %d, want 3", len(secrets))
	}
	for _, secret := range secrets {
		if secret.KeyID != third.KeyID && secret.Status != "overlapping" {
			t.Fatalf("unexpired previous status = %q, want overlapping", secret.Status)
		}
		if secret.KeyID == third.KeyID && secret.Status != "active" {
			t.Fatalf("latest status = %q, want active", secret.Status)
		}
	}
	repo.mu.Lock()
	retiredAt := time.Now().Add(-time.Second)
	repo.secrets["tenant-1"][first.KeyID].RetiredAt = &retiredAt
	repo.mu.Unlock()
	secrets, err = svc.ListSigningSecrets(ctx)
	if err != nil {
		t.Fatalf("list expired secrets: %v", err)
	}
	for _, secret := range secrets {
		if secret.KeyID == first.KeyID && secret.Status != "retired" {
			t.Fatalf("expired previous status = %q, want retired", secret.Status)
		}
		if secret.KeyID == second.KeyID && secret.Status != "overlapping" {
			t.Fatalf("unexpired second version status = %q, want overlapping", secret.Status)
		}
	}
}

func TestRotateSigningSecret_ConcurrentOnlyOneActive(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")
	const rotations = 12
	var wg sync.WaitGroup
	errs := make(chan error, rotations)
	for rotation := 0; rotation < rotations; rotation++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.RotateSigningSecret(ctx, time.Minute)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent rotation: %v", err)
		}
	}
	secrets, err := svc.ListSigningSecrets(ctx)
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	active := 0
	for _, secret := range secrets {
		if secret.Status == "active" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active versions = %d, want exactly one", active)
	}
}

func TestListSigningSecrets_ImportsLegacySecretEncrypted(t *testing.T) {
	repo := newMockConfigRepo()
	legacySecret := "whsec_existing-legacy-secret"
	if err := repo.UpsertConfig(context.Background(), &domain.TenantWebhookConfig{
		TenantID: "tenant-legacy", Secret: legacySecret, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed legacy config: %v", err)
	}
	svc := newConfigTestService(t, repo)
	secrets, err := svc.ListSigningSecrets(tenantCtx("tenant-legacy"))
	if err != nil {
		t.Fatalf("list imported secret: %v", err)
	}
	if len(secrets) != 1 || secrets[0].Status != "active" {
		t.Fatalf("imported metadata = %v, want one active version", secrets)
	}
	config, err := repo.GetConfig(context.Background(), "tenant-legacy")
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	if config.SigningKeyID != secrets[0].KeyID || config.Secret == legacySecret || !strings.HasPrefix(config.Secret, "v1::") {
		t.Fatal("legacy secret should be encrypted and linked to its active key ID")
	}
	stored, err := repo.GetSigningSecret(context.Background(), "tenant-legacy", secrets[0].KeyID)
	if err != nil {
		t.Fatalf("read imported key: %v", err)
	}
	plaintext, err := svc.decryptSecret(stored)
	if err != nil || plaintext != legacySecret {
		t.Fatal("imported key should preserve the original secret")
	}
}

func TestMigrateLegacySigningSecrets_EncryptsAllConfigs(t *testing.T) {
	repo := newMockConfigRepo()
	for _, tenantID := range []string{"tenant-a", "tenant-b"} {
		if err := repo.UpsertConfig(context.Background(), &domain.TenantWebhookConfig{
			TenantID: tenantID, Secret: "whsec_legacy_" + tenantID, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed %s: %v", tenantID, err)
		}
	}
	svc := newConfigTestService(t, repo)
	if err := svc.MigrateLegacySigningSecrets(context.Background()); err != nil {
		t.Fatalf("migrate legacy secrets: %v", err)
	}
	for _, tenantID := range []string{"tenant-a", "tenant-b"} {
		config, err := repo.GetConfig(context.Background(), tenantID)
		if err != nil {
			t.Fatalf("read %s: %v", tenantID, err)
		}
		if config.SigningKeyID == "" || !strings.HasPrefix(config.Secret, "v1::") {
			t.Fatalf("%s was not encrypted and versioned: %+v", tenantID, config)
		}
	}
}

func TestUpdateConfig_RejectsUnsafeURL(t *testing.T) {
	svc := newConfigTestService(t, newMockConfigRepo())
	if _, err := svc.UpdateConfig(tenantCtx("tenant-1"), domain.WebhookConfigUpdate{
		URL: strPtr("file:///etc/passwd"),
	}); err == nil {
		t.Fatal("expected a non-HTTP scheme to be rejected")
	}
}

func TestGetConfig_NotFound(t *testing.T) {
	svc := newConfigTestService(t, newMockConfigRepo())
	if _, err := svc.GetConfig(tenantCtx("tenant-1")); err != domain.ErrWebhookConfigNotFound {
		t.Fatalf("GetConfig() = %v, want ErrWebhookConfigNotFound", err)
	}
}

// TestConfig_RequiresTenant guards the isolation boundary: without a tenant on
// the context there is no config to read, so every config operation must fail
// closed rather than fall back to some global config.
func TestConfig_RequiresTenant(t *testing.T) {
	svc := newConfigTestService(t, newMockConfigRepo())
	ctx := context.Background()

	if _, err := svc.GetConfig(ctx); err == nil {
		t.Fatal("GetConfig without tenant should fail")
	}
	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{URL: strPtr(unreachableURL("/"))}); err == nil {
		t.Fatal("UpdateConfig without tenant should fail")
	}
	if _, err := svc.ListConfigDeliveries(ctx, 20, 0); err == nil {
		t.Fatal("ListConfigDeliveries without tenant should fail")
	}
}

func TestUpdateConfig_PauseStopsDeliveryAndScheduledResumeClears(t *testing.T) {
	var received int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Enabled: boolPtr(true), URL: strPtr(ts.URL)}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}

	paused, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Paused: boolPtr(true)})
	if err != nil {
		t.Fatalf("pause error: %v", err)
	}
	if !paused.Config.Paused {
		t.Fatal("expected config to be paused")
	}

	// Fan out an event while paused: it must be recorded, marked paused, and
	// never hit the network.
	if err := svc.DispatchToTenants(ctx, domain.EventTransferSettled, map[string]string{"id": "tx-1"}); err != nil {
		t.Fatalf("DispatchToTenants() error: %v", err)
	}
	deliveries, err := svc.ListConfigDeliveries(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected the paused delivery to be recorded, got %d", len(deliveries))
	}
	if deliveries[0].Status != domain.DeliveryPaused {
		t.Fatalf("status = %q, want %q", deliveries[0].Status, domain.DeliveryPaused)
	}
	mu.Lock()
	got := received
	mu.Unlock()
	if got != 0 {
		t.Fatalf("paused delivery reached the network %d time(s)", got)
	}

	// Unpausing clears any scheduled resume, leaving an unambiguous state.
	resumed, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Paused: boolPtr(false)})
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if resumed.Config.Paused {
		t.Fatal("expected config to be unpaused")
	}
	if resumed.Config.ResumeAt != nil {
		t.Fatal("expected scheduled resume to be cleared on manual resume")
	}
}

func TestUpdateConfig_ScheduledResumeIsRecorded(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	when := time.Now().UTC().Add(30 * time.Minute)
	result, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{ResumeAt: &when})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if !result.Config.Paused {
		t.Fatal("scheduling a resume should leave the tenant paused")
	}
	if result.Config.ResumeAt == nil || !result.Config.ResumeAt.Equal(when) {
		t.Fatalf("resume_at = %v, want %v", result.Config.ResumeAt, when)
	}
}

func TestDeliverConfig_ElapsedScheduledResumeSends(t *testing.T) {
	var received int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Enabled: boolPtr(true), URL: strPtr(ts.URL)}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}

	// Pause with a resume time in the future, so the tenant is genuinely paused
	// right now and the dispatch below must record without sending.
	future := time.Now().UTC().Add(time.Hour)
	paused, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{ResumeAt: &future})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if !paused.Config.Paused {
		t.Fatal("expected paused state to be recorded first")
	}

	if err := svc.DispatchToTenants(ctx, domain.EventTransferSettled, map[string]string{"id": "tx-1"}); err != nil {
		t.Fatalf("DispatchToTenants() error: %v", err)
	}
	deliveries, err := svc.ListConfigDeliveries(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery while paused, got %d", len(deliveries))
	}
	if deliveries[0].Status != domain.DeliveryPaused {
		t.Fatalf("status = %q, want %q", deliveries[0].Status, domain.DeliveryPaused)
	}
	mu.Lock()
	if received != 0 {
		mu.Unlock()
		t.Fatalf("a paused tenant reached the network %d time(s)", received)
	}
	mu.Unlock()

	// Let the scheduled resume elapse. Setting ResumeAt keeps Paused true, so the
	// only thing that can lift it is the delivery path noticing the deadline.
	past := time.Now().UTC().Add(-time.Minute)
	stillPaused, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{ResumeAt: &past})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if !stillPaused.Config.Paused {
		t.Fatal("config should still be paused until the delivery path lifts it")
	}

	if err := svc.DeliverConfig(ctx, deliveries[0].ID, "tenant-1"); err != nil {
		t.Fatalf("DeliverConfig() error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if received != 1 {
		t.Fatalf("expected the elapsed resume to lift the pause and send once, got %d", received)
	}

	// The pause must be cleared persistently, not just for this delivery.
	after, err := repo.GetConfig(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("GetConfig() error: %v", err)
	}
	if after.Paused {
		t.Fatal("expected the pause to be lifted once the resume elapsed")
	}
	if after.ResumeAt != nil {
		t.Fatalf("resume_at = %v, want it cleared", after.ResumeAt)
	}

	updated, err := repo.GetConfigDelivery(context.Background(), deliveries[0].ID, "tenant-1")
	if err != nil {
		t.Fatalf("GetConfigDelivery() error: %v", err)
	}
	if updated.Status != domain.DeliverySuccess {
		t.Fatalf("status = %q, want %q", updated.Status, domain.DeliverySuccess)
	}
}

func TestDeliverConfig_PausedRecordsWithoutSending(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Enabled: boolPtr(true), URL: strPtr(unreachableURL("/hook"))}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Paused: boolPtr(true)}); err != nil {
		t.Fatalf("pause error: %v", err)
	}

	if err := svc.DispatchToTenants(ctx, domain.EventTransferSettled, map[string]string{"id": "tx-1"}); err != nil {
		t.Fatalf("DispatchToTenants() error: %v", err)
	}
	deliveries, err := svc.ListConfigDeliveries(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveries))
	}
	if err := svc.DeliverConfig(ctx, deliveries[0].ID, "tenant-1"); err != nil {
		t.Fatalf("DeliverConfig() error: %v", err)
	}
	updated, err := repo.GetConfigDelivery(context.Background(), deliveries[0].ID, "tenant-1")
	if err != nil {
		t.Fatalf("GetConfigDelivery() error: %v", err)
	}
	if updated.Status != domain.DeliveryPaused {
		t.Fatalf("status = %q, want %q", updated.Status, domain.DeliveryPaused)
	}
}

func TestDispatchToTenants_SendsSignedPayloadAndRecordsSuccess(t *testing.T) {
	var gotSig, gotTimestamp, gotEvent, gotTenant, gotKeyID string
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Nexora-Signature")
		gotTimestamp = r.Header.Get("X-Nexora-Timestamp")
		gotEvent = r.Header.Get("X-Nexora-Event")
		gotTenant = r.Header.Get("X-Nexora-Tenant-ID")
		gotKeyID = r.Header.Get("X-Nexora-Key-ID")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	created, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{
		Enabled: boolPtr(true),
		URL:     strPtr(ts.URL),
		Events:  eventsPtr(string(domain.EventTransferSettled)),
	})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}

	if err := svc.DispatchToTenants(ctx, domain.EventTransferSettled, map[string]string{"id": "tx-1"}); err != nil {
		t.Fatalf("DispatchToTenants() error: %v", err)
	}
	deliveries, err := svc.ListConfigDeliveries(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveries))
	}
	if _, _, err := svc.RotateSigningSecret(ctx, time.Minute); err != nil {
		t.Fatalf("rotate before retry: %v", err)
	}
	// No queue is wired in tests, so drive the delivery directly.
	if err := svc.DeliverConfig(ctx, deliveries[0].ID, "tenant-1"); err != nil {
		t.Fatalf("DeliverConfig() error: %v", err)
	}

	// The one-time secret from creation is what signs deliveries; the config
	// value returned to the caller is redacted.
	if want := signBody(created.Secret, deliveries[0].Payload); gotSig != want {
		t.Fatalf("signature = %q, want %q", gotSig, want)
	}
	if gotKeyID != deliveries[0].SigningKeyID {
		t.Fatalf("key ID header = %q, want pinned delivery key %q", gotKeyID, deliveries[0].SigningKeyID)
	}
	if gotTimestamp == "" {
		t.Fatal("timestamp header is empty")
	}
	if gotEvent != string(domain.EventTransferSettled) {
		t.Fatalf("event header = %q", gotEvent)
	}
	if gotTenant != "tenant-1" {
		t.Fatalf("tenant header = %q", gotTenant)
	}
	if string(gotBody) != `{"id":"tx-1"}` {
		t.Fatalf("body = %s", gotBody)
	}

	updated, err := repo.GetConfigDelivery(context.Background(), deliveries[0].ID, "tenant-1")
	if err != nil {
		t.Fatalf("GetConfigDelivery() error: %v", err)
	}
	if updated.Status != domain.DeliverySuccess {
		t.Fatalf("status = %q, want success", updated.Status)
	}
	if updated.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1", updated.AttemptCount)
	}
	if updated.ResponseCode == nil || *updated.ResponseCode != http.StatusAccepted {
		t.Fatalf("response_code = %v, want 202", updated.ResponseCode)
	}

	config, err := repo.GetConfig(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("GetConfig() error: %v", err)
	}
	if config.LastDeliveredAt == nil {
		t.Fatal("expected last_delivered_at to be set after a successful delivery")
	}
}

func TestDispatchToTenants_SkipsDisabledAndUnsubscribed(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)

	// Tenant A is enabled but only wants a different event.
	if _, err := svc.UpdateConfig(tenantCtx("tenant-a"), domain.WebhookConfigUpdate{
		Enabled: boolPtr(true),
		URL:     strPtr(unreachableURL("/a")),
		Events:  eventsPtr(string(domain.EventTransferFailed)),
	}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	// Tenant B is configured but left disabled.
	if _, err := svc.UpdateConfig(tenantCtx("tenant-b"), domain.WebhookConfigUpdate{
		Enabled: boolPtr(false),
		URL:     strPtr(unreachableURL("/b")),
	}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}

	if err := svc.DispatchToTenants(tenantCtx("tenant-a"), domain.EventTransferSettled, map[string]string{"id": "tx-1"}); err != nil {
		t.Fatalf("DispatchToTenants() error: %v", err)
	}
	a, err := svc.ListConfigDeliveries(tenantCtx("tenant-a"), 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(a) != 0 {
		t.Fatalf("unsubscribed tenant received %d deliveries", len(a))
	}
	b, err := svc.ListConfigDeliveries(tenantCtx("tenant-b"), 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("disabled tenant received %d deliveries", len(b))
	}
}

func TestListConfigDeliveries_TenantScoped(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)

	for _, id := range []string{"tenant-a", "tenant-b"} {
		if _, err := svc.UpdateConfig(tenantCtx(id), domain.WebhookConfigUpdate{
			Enabled: boolPtr(true),
			URL:     strPtr(unreachableURL("/" + id)),
		}); err != nil {
			t.Fatalf("UpdateConfig() error: %v", err)
		}
	}

	if err := svc.DispatchToTenants(context.Background(), domain.EventTransferSettled, map[string]string{"id": "tx"}); err != nil {
		t.Fatalf("DispatchToTenants() error: %v", err)
	}

	a, err := svc.ListConfigDeliveries(tenantCtx("tenant-a"), 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(a) != 1 {
		t.Fatalf("tenant-a deliveries = %d, want 1", len(a))
	}
	if a[0].TenantID != "tenant-a" {
		t.Fatalf("tenant-a saw a delivery owned by %q", a[0].TenantID)
	}

	b, err := svc.ListConfigDeliveries(tenantCtx("tenant-b"), 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(b) != 1 {
		t.Fatalf("tenant-b deliveries = %d, want 1", len(b))
	}
}

func TestDeliverConfig_RejectsCrossTenantDeliveryID(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-a")

	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{
		Enabled: boolPtr(true),
		URL:     strPtr(unreachableURL("/a")),
	}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if err := svc.DispatchToTenants(ctx, domain.EventTransferSettled, map[string]string{"id": "tx"}); err != nil {
		t.Fatalf("DispatchToTenants() error: %v", err)
	}
	deliveries, err := svc.ListConfigDeliveries(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveries))
	}

	// Another tenant must not be able to deliver a row it does not own.
	if err := svc.DeliverConfig(context.Background(), deliveries[0].ID, "tenant-b"); err == nil {
		t.Fatal("expected cross-tenant delivery lookup to fail")
	}
}

func TestConfig_IsolatedPerTenant(t *testing.T) {
	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)

	a, err := svc.UpdateConfig(tenantCtx("tenant-a"), domain.WebhookConfigUpdate{
		Enabled: boolPtr(true),
		URL:     strPtr(unreachableURL("/a")),
	})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	b, err := svc.UpdateConfig(tenantCtx("tenant-b"), domain.WebhookConfigUpdate{
		Enabled: boolPtr(true),
		URL:     strPtr(unreachableURL("/b")),
	})
	if err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if a.Secret == b.Secret {
		t.Fatal("each tenant must get a distinct secret")
	}
	// The one-time secrets differ, and neither is readable from the config.
	storedA, err := repo.GetConfig(tenantCtx("tenant-a"), "tenant-a")
	if err != nil {
		t.Fatalf("GetConfig() error: %v", err)
	}
	storedB, err := repo.GetConfig(tenantCtx("tenant-b"), "tenant-b")
	if err != nil {
		t.Fatalf("GetConfig() error: %v", err)
	}
	if storedA.Secret == a.Secret || storedB.Secret == b.Secret {
		t.Fatal("tenant secrets must not be stored in plaintext")
	}
	if storedA.Secret == storedB.Secret {
		t.Fatal("each tenant must get a distinct stored secret")
	}

	// Mutating one tenant's config must not touch the other's.
	if _, err := svc.UpdateConfig(tenantCtx("tenant-a"), domain.WebhookConfigUpdate{
		URL: strPtr(unreachableURL("/a2")),
	}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	gotB, err := svc.GetConfig(tenantCtx("tenant-b"))
	if err != nil {
		t.Fatalf("GetConfig() error: %v", err)
	}
	if gotB.URL != unreachableURL("/b") {
		t.Fatalf("tenant-b URL mutated to %q", gotB.URL)
	}
}

func TestTestDelivery_SendsSampleEventAndRecordsResult(t *testing.T) {
	var gotEvent string
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEvent = r.Header.Get("X-Nexora-Event")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	repo := newMockConfigRepo()
	svc := newConfigTestService(t, repo)
	ctx := tenantCtx("tenant-1")

	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{URL: strPtr(ts.URL)}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}

	delivery, err := svc.TestDelivery(ctx)
	if err != nil {
		t.Fatalf("TestDelivery() error: %v", err)
	}
	if delivery.Status != domain.DeliverySuccess {
		t.Fatalf("status = %q, want success", delivery.Status)
	}
	if gotEvent != "webhook.test" {
		t.Fatalf("event header = %q, want webhook.test", gotEvent)
	}
	if len(body) == 0 {
		t.Fatal("expected a sample payload to be delivered")
	}

	// The test delivery must also show up in the tenant's history.
	history, err := svc.ListConfigDeliveries(ctx, 20, 0)
	if err != nil {
		t.Fatalf("ListConfigDeliveries() error: %v", err)
	}
	if len(history) != 1 || history[0].ID != delivery.ID {
		t.Fatalf("expected the test delivery in history, got %d entries", len(history))
	}
}

func TestTestDelivery_RequiresURL(t *testing.T) {
	svc := newConfigTestService(t, newMockConfigRepo())
	if _, err := svc.UpdateConfig(tenantCtx("tenant-1"), domain.WebhookConfigUpdate{Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if _, err := svc.TestDelivery(tenantCtx("tenant-1")); err == nil {
		t.Fatal("expected a test delivery without a URL to fail")
	}
}

// TestTestDelivery_IgnoresPause documents that a test delivery deliberately
// bypasses the pause switch: the tenant is actively verifying the endpoint.
func TestTestDelivery_IgnoresPause(t *testing.T) {
	var received int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	svc := newConfigTestService(t, newMockConfigRepo())
	ctx := tenantCtx("tenant-1")

	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Enabled: boolPtr(true), URL: strPtr(ts.URL)}); err != nil {
		t.Fatalf("UpdateConfig() error: %v", err)
	}
	if _, err := svc.UpdateConfig(ctx, domain.WebhookConfigUpdate{Paused: boolPtr(true)}); err != nil {
		t.Fatalf("pause error: %v", err)
	}
	if _, err := svc.TestDelivery(ctx); err != nil {
		t.Fatalf("TestDelivery() error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if received != 1 {
		t.Fatalf("test delivery should send while paused, got %d sends", received)
	}
}

func TestSubscribedTo(t *testing.T) {
	cases := []struct {
		name   string
		events []string
		want   bool
	}{
		{"empty means all", nil, true},
		{"listed", []string{"a", "b"}, true},
		{"not listed", []string{"b"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subscribedTo(tc.events, "a"); got != tc.want {
				t.Fatalf("subscribedTo(%v, \"a\") = %v, want %v", tc.events, got, tc.want)
			}
		})
	}
}
