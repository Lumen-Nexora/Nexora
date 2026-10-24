package crypto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	gcpKMSProvider       = "gcp-kms"
	defaultGCPEndpoint   = "https://cloudkms.googleapis.com"
	defaultGCPMetadata   = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	gcpMinRotationPeriod = 24 * time.Hour
	// tokenRefreshSkew refreshes a metadata token this long before it expires.
	tokenRefreshSkew = time.Minute
)

// GCPKMSConfig configures Google Cloud KMS.
type GCPKMSConfig struct {
	// KeyName is the full CryptoKey resource name:
	// projects/P/locations/L/keyRings/R/cryptoKeys/K.
	KeyName string
	// AccessToken is a static OAuth2 bearer token. When empty, tokens are
	// fetched from the GCE/GKE metadata server (workload identity).
	AccessToken string
	Endpoint    string
	MetadataURL string
	HTTPClient  *http.Client
}

// GCPKMS wraps data keys with a Cloud KMS symmetric CryptoKey over its REST
// API. Cloud KMS records the key version in each ciphertext and decrypts with
// it automatically.
type GCPKMS struct {
	cfg      GCPKMSConfig
	endpoint string
	client   *http.Client
	now      func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewGCPKMS validates the configuration and builds the client.
func NewGCPKMS(cfg GCPKMSConfig) (*GCPKMS, error) {
	if !strings.HasPrefix(cfg.KeyName, "projects/") || !strings.Contains(cfg.KeyName, "/cryptoKeys/") {
		return nil, fmt.Errorf("%w: gcp kms key name must be projects/.../cryptoKeys/...", ErrInvalidKMSConfig)
	}
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = defaultGCPEndpoint
	}
	if cfg.MetadataURL == "" {
		cfg.MetadataURL = defaultGCPMetadata
	}
	return &GCPKMS{cfg: cfg, endpoint: endpoint, client: httpClient(cfg.HTTPClient), now: time.Now}, nil
}

func (g *GCPKMS) Name() string { return gcpKMSProvider }

func (g *GCPKMS) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	var out struct {
		Ciphertext []byte `json:"ciphertext"`
	}
	err := g.call(ctx, http.MethodPost, g.cfg.KeyName+":encrypt", struct {
		Plaintext []byte `json:"plaintext"`
	}{dek}, &out)
	return out.Ciphertext, err
}

func (g *GCPKMS) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var out struct {
		Plaintext []byte `json:"plaintext"`
	}
	err := g.call(ctx, http.MethodPost, g.cfg.KeyName+":decrypt", struct {
		Ciphertext []byte `json:"ciphertext"`
	}{wrapped}, &out)
	return out.Plaintext, err
}

// Rewrap decrypts and re-encrypts under the primary version; Cloud KMS has
// no server-side re-encrypt. The data key is zeroized immediately.
func (g *GCPKMS) Rewrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	dek, err := g.Unwrap(ctx, wrapped)
	if err != nil {
		return nil, err
	}
	defer Zeroize(dek)
	return g.Wrap(ctx, dek)
}

// EnsureRotation sets the CryptoKey's rotation period and schedules the next
// rotation one period from now.
func (g *GCPKMS) EnsureRotation(ctx context.Context, policy RotationPolicy) error {
	if policy.Period < gcpMinRotationPeriod {
		return fmt.Errorf("%w: gcp kms rotation period must be at least 24h", ErrInvalidKMSConfig)
	}
	return g.call(ctx, http.MethodPatch, g.cfg.KeyName+"?updateMask=rotationPeriod,nextRotationTime", map[string]string{
		"rotationPeriod":   fmt.Sprintf("%ds", int64(policy.Period.Seconds())),
		"nextRotationTime": g.now().UTC().Add(policy.Period).Format(time.RFC3339),
	}, nil)
}

func (g *GCPKMS) call(ctx context.Context, method, resource string, in, out interface{}) error {
	token, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, g.endpoint+"/v1/"+resource, bytes.NewReader(body))
	if err != nil {
		Zeroize(body)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return doKMS(g.client, req, body, out, gcpKMSProvider+" "+method, googleErrorMessage)
}

func googleErrorMessage(raw []byte) string {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return strings.TrimSpace(e.Error.Status + " " + e.Error.Message)
}

// accessToken returns the static token, or a cached metadata-server token
// refreshed shortly before it expires.
func (g *GCPKMS) accessToken(ctx context.Context) (string, error) {
	if g.cfg.AccessToken != "" {
		return g.cfg.AccessToken, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && g.now().Before(g.expiry.Add(-tokenRefreshSkew)) {
		return g.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.MetadataURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := doKMS(g.client, req, nil, &out, "gcp metadata token", func([]byte) string { return "" }); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("%w: gcp metadata token: empty token", ErrKMSRequest)
	}
	g.token, g.expiry = out.AccessToken, g.now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return g.token, nil
}
