package crypto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	vaultTransitProvider = "vault-transit"
	vaultMinRotation     = time.Hour
)

// VaultConfig configures HashiCorp Vault's Transit secrets engine.
type VaultConfig struct {
	Address    string // e.g. https://vault.internal:8200
	Token      string
	Namespace  string // Vault Enterprise namespace, optional
	Mount      string // Transit mount path, default "transit"
	Key        string // Transit key name
	HTTPClient *http.Client
}

// VaultTransit wraps data keys with a Vault Transit key. Transit prefixes
// each ciphertext with its key version ("vault:v3:..."), decrypts with that
// version and can rewrap server-side.
type VaultTransit struct {
	cfg    VaultConfig
	base   string
	client *http.Client
}

// NewVaultTransit validates the configuration and builds the client.
func NewVaultTransit(cfg VaultConfig) (*VaultTransit, error) {
	if cfg.Address == "" || cfg.Token == "" || cfg.Key == "" {
		return nil, fmt.Errorf("%w: vault needs address, token and transit key", ErrInvalidKMSConfig)
	}
	if _, err := url.ParseRequestURI(cfg.Address); err != nil {
		return nil, fmt.Errorf("%w: vault address: %v", ErrInvalidKMSConfig, err)
	}
	mount := strings.Trim(cfg.Mount, "/")
	if mount == "" {
		mount = "transit"
	}
	return &VaultTransit{
		cfg:    cfg,
		base:   strings.TrimRight(cfg.Address, "/") + "/v1/" + mount,
		client: httpClient(cfg.HTTPClient),
	}, nil
}

func (v *VaultTransit) Name() string { return vaultTransitProvider }

func (v *VaultTransit) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	err := v.call(ctx, "/encrypt/"+url.PathEscape(v.cfg.Key), struct {
		Plaintext []byte `json:"plaintext"`
	}{dek}, &out)
	return []byte(out.Data.Ciphertext), err
}

func (v *VaultTransit) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext []byte `json:"plaintext"`
		} `json:"data"`
	}
	err := v.call(ctx, "/decrypt/"+url.PathEscape(v.cfg.Key), map[string]string{"ciphertext": string(wrapped)}, &out)
	return out.Data.Plaintext, err
}

// Rewrap uses Transit's rewrap endpoint, so the data key never leaves Vault.
func (v *VaultTransit) Rewrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	err := v.call(ctx, "/rewrap/"+url.PathEscape(v.cfg.Key), map[string]string{"ciphertext": string(wrapped)}, &out)
	return []byte(out.Data.Ciphertext), err
}

// EnsureRotation sets the key's auto_rotate_period (Vault 1.10+).
func (v *VaultTransit) EnsureRotation(ctx context.Context, policy RotationPolicy) error {
	if policy.Period < vaultMinRotation {
		return fmt.Errorf("%w: vault transit rotation period must be at least 1h", ErrInvalidKMSConfig)
	}
	return v.call(ctx, "/keys/"+url.PathEscape(v.cfg.Key)+"/config", map[string]string{
		"auto_rotate_period": fmt.Sprintf("%ds", int64(policy.Period.Seconds())),
	}, nil)
}

func (v *VaultTransit) call(ctx context.Context, path string, in, out interface{}) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.base+path, bytes.NewReader(body))
	if err != nil {
		Zeroize(body)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", v.cfg.Token)
	if v.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.cfg.Namespace)
	}
	return doKMS(v.client, req, body, out, vaultTransitProvider+path, func(raw []byte) string {
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		return strings.Join(e.Errors, "; ")
	})
}
