package anchor

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
)

// Registry is an in-memory index of registered anchors, keyed by the asset
// codes they support, backed by the anchors table. It is populated from the
// database on startup and updated whenever a new anchor is registered.
type Registry struct {
	mu         sync.RWMutex
	byAsset    map[string][]*domain.Anchor
	byID       map[string]*domain.Anchor
	repo       Repository
	httpClient *http.Client
}

func NewRegistry(repo Repository, httpClient *http.Client) *Registry {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Registry{
		byAsset:    make(map[string][]*domain.Anchor),
		byID:       make(map[string]*domain.Anchor),
		repo:       repo,
		httpClient: httpClient,
	}
}

// Load populates the registry from the anchors table. Call once on startup.
func (r *Registry) Load(ctx context.Context) error {
	anchors, err := r.repo.ListAnchors(ctx)
	if err != nil {
		return fmt.Errorf("list anchors: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.byAsset = make(map[string][]*domain.Anchor)
	r.byID = make(map[string]*domain.Anchor)
	for _, a := range anchors {
		r.index(a)
	}
	return nil
}

func (r *Registry) index(a *domain.Anchor) {
	r.byID[a.ID] = a
	for _, asset := range a.SupportedAssets {
		r.byAsset[asset] = append(r.byAsset[asset], a)
	}
}

// Register fetches and parses homeDomain's stellar.toml (SEP-1), persists
// the resulting anchor, and adds it to the in-memory registry. No fields
// beyond the home domain need to be supplied manually.
func (r *Registry) Register(ctx context.Context, homeDomain string) (*domain.Anchor, error) {
	homeDomain = NormalizeHomeDomain(homeDomain)

	info, err := FetchStellarToml(ctx, homeDomain, r.httpClient)
	if err != nil {
		return nil, fmt.Errorf("register anchor %s: %w", homeDomain, err)
	}

	sepVersions := []int{1}
	if info.WebAuthEndpoint != "" {
		sepVersions = append(sepVersions, 10)
	}
	if info.TransferServer != "" {
		sepVersions = append(sepVersions, 6)
	}
	if info.TransferServerSep24 != "" {
		sepVersions = append(sepVersions, 24)
	}

	a := &domain.Anchor{
		ID:                  uuid.New().String(),
		HomeDomain:          homeDomain,
		TransferServer:      info.TransferServer,
		TransferServerSep24: info.TransferServerSep24,
		WebAuthEndpoint:     info.WebAuthEndpoint,
		Sep10SigningKey:     info.SigningKey,
		NetworkPassphrase:   info.NetworkPassphrase,
		SupportedAssets:     info.SupportedAssets,
		SepVersions:         sepVersions,
		RegisteredAt:        time.Now().UTC(),
	}

	if err := r.repo.CreateAnchor(ctx, a); err != nil {
		return nil, fmt.Errorf("persist anchor %s: %w", homeDomain, err)
	}

	r.mu.Lock()
	r.index(a)
	r.mu.Unlock()

	return a, nil
}

// List returns every registered anchor.
func (r *Registry) List() []*domain.Anchor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.Anchor, 0, len(r.byID))
	for _, a := range r.byID {
		out = append(out, a)
	}
	return out
}

// GetByID returns a registered anchor by its Nexora-assigned ID.
func (r *Registry) GetByID(id string) (*domain.Anchor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("anchor %s is not registered", id)
	}
	return a, nil
}

// ResolveForAsset returns a registered anchor that supports assetCode. If
// multiple anchors support the asset, the first one registered is used.
func (r *Registry) ResolveForAsset(assetCode string) (*domain.Anchor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	anchors := r.byAsset[assetCode]
	if len(anchors) == 0 {
		return nil, fmt.Errorf("no registered anchor supports asset %s", assetCode)
	}
	return anchors[0], nil
}
