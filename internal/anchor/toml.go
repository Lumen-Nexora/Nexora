package anchor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// tomlDoc mirrors the subset of SEP-1 (stellar.toml) fields Nexora needs to
// register an anchor: authentication, transfer server endpoints and the
// assets the anchor issues/supports.
type tomlDoc struct {
	SigningKey          string         `toml:"SIGNING_KEY"`
	NetworkPassphrase   string         `toml:"NETWORK_PASSPHRASE"`
	WebAuthEndpoint     string         `toml:"WEB_AUTH_ENDPOINT"`
	TransferServer      string         `toml:"TRANSFER_SERVER"`
	TransferServerSep24 string         `toml:"TRANSFER_SERVER_SEP0024"`
	KYCServer           string         `toml:"KYC_SERVER"`
	Currencies          []tomlCurrency `toml:"CURRENCIES"`
}

type tomlCurrency struct {
	Code   string `toml:"code"`
	Issuer string `toml:"issuer"`
	Status string `toml:"status"`
}

// NormalizeHomeDomain strips a scheme and trailing slash from a home domain
// so the same anchor is never registered twice under different spellings.
func NormalizeHomeDomain(homeDomain string) string {
	homeDomain = strings.TrimSpace(homeDomain)
	homeDomain = strings.TrimPrefix(homeDomain, "https://")
	homeDomain = strings.TrimPrefix(homeDomain, "http://")
	homeDomain = strings.TrimSuffix(homeDomain, "/")
	return homeDomain
}

// TomlInfo is the parsed, validated result of fetching an anchor's stellar.toml.
type TomlInfo struct {
	SigningKey          string
	NetworkPassphrase   string
	WebAuthEndpoint     string
	TransferServer      string
	TransferServerSep24 string
	KYCServer           string
	SupportedAssets     []string
}

// FetchStellarToml retrieves and parses the stellar.toml hosted at
// https://<homeDomain>/.well-known/stellar.toml (SEP-1), so an anchor can be
// registered by home domain alone with no manually entered fields.
func FetchStellarToml(ctx context.Context, homeDomain string, httpClient *http.Client) (*TomlInfo, error) {
	homeDomain = NormalizeHomeDomain(homeDomain)
	if homeDomain == "" {
		return nil, fmt.Errorf("home domain is required")
	}

	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	url := "https://" + homeDomain + "/.well-known/stellar.toml"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build stellar.toml request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch stellar.toml from %s: %w", homeDomain, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch stellar.toml from %s: unexpected status %d", homeDomain, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read stellar.toml body: %w", err)
	}

	var doc tomlDoc
	if err := toml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse stellar.toml: %w", err)
	}

	if doc.SigningKey == "" {
		return nil, fmt.Errorf("stellar.toml at %s has no SIGNING_KEY", homeDomain)
	}
	if doc.WebAuthEndpoint == "" {
		return nil, fmt.Errorf("stellar.toml at %s has no WEB_AUTH_ENDPOINT", homeDomain)
	}
	if doc.TransferServer == "" && doc.TransferServerSep24 == "" {
		return nil, fmt.Errorf("stellar.toml at %s declares no SEP-6 or SEP-24 transfer server", homeDomain)
	}

	assets := make([]string, 0, len(doc.Currencies))
	for _, c := range doc.Currencies {
		if c.Code != "" {
			assets = append(assets, c.Code)
		}
	}

	return &TomlInfo{
		SigningKey:          doc.SigningKey,
		NetworkPassphrase:   doc.NetworkPassphrase,
		WebAuthEndpoint:     doc.WebAuthEndpoint,
		TransferServer:      doc.TransferServer,
		TransferServerSep24: doc.TransferServerSep24,
		KYCServer:           doc.KYCServer,
		SupportedAssets:     assets,
	}, nil
}
