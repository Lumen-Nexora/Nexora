package compliance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/oschwald/maxminddb-golang"
)

// GeoInfo is the subset of a MaxMind lookup that matters for jurisdiction and
// anonymizer screening. Anonymous-IP databases populate the Trait fields while
// GeoLite2-City populates Country.
type GeoInfo struct {
	CountryCode string
	CountryName string
	Subdivision string
	City        string

	IsAnonymousVPN     bool
	IsTorExitNode      bool
	IsHostingProvider  bool
	IsAnonymousProxy   bool
	IsPublicProxy      bool
	IsResidentialProxy bool
}

// IPResolver resolves an IP address to jurisdiction and anonymizer metadata.
// MaxMindResolver is the production implementation; tests use fakes.
type IPResolver interface {
	Lookup(ip net.IP) (GeoInfo, error)
}

// MaxMindResolver reads a local GeoLite2-City and/or GeoIP2-Anonymous-IP
// database. Both databases share the same file format, so either (or a merged
// database) can be opened here.
type MaxMindResolver struct {
	reader *maxminddb.Reader
}

func OpenMaxMindResolver(path string) (*MaxMindResolver, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("geo: MaxMind database path is empty")
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("geo: open MaxMind database %s: %w", path, err)
	}
	return &MaxMindResolver{reader: reader}, nil
}

func (r *MaxMindResolver) Lookup(ip net.IP) (GeoInfo, error) {
	if r == nil || r.reader == nil {
		return GeoInfo{}, errors.New("geo: MaxMind database is not open")
	}
	if ip == nil {
		return GeoInfo{}, errors.New("geo: IP address is required")
	}

	var record struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
			Name    string `maxminddb:"name"`
		} `maxminddb:"country"`
		Subdivisions []struct {
			ISOCode string `maxminddb:"iso_code"`
			Name    string `maxminddb:"name"`
		} `maxminddb:"subdivisions"`
		City struct {
			Name string `maxminddb:"name"`
		} `maxminddb:"city"`
		Traits struct {
			IsAnonymousVPN     bool `maxminddb:"is_anonymous_vpn"`
			IsTorExitNode      bool `maxminddb:"is_tor_exit_node"`
			IsHostingProvider  bool `maxminddb:"is_hosting_provider"`
			IsAnonymousProxy   bool `maxminddb:"is_anonymous_proxy"`
			IsPublicProxy      bool `maxminddb:"is_public_proxy"`
			IsResidentialProxy bool `maxminddb:"is_residential_proxy"`
		} `maxminddb:"traits"`
	}
	if err := r.reader.Lookup(ip, &record); err != nil {
		return GeoInfo{}, fmt.Errorf("geo: lookup %s: %w", ip.String(), err)
	}

	info := GeoInfo{
		CountryCode:        strings.ToUpper(strings.TrimSpace(record.Country.ISOCode)),
		CountryName:        record.Country.Name,
		City:               record.City.Name,
		IsAnonymousVPN:     record.Traits.IsAnonymousVPN,
		IsTorExitNode:      record.Traits.IsTorExitNode,
		IsHostingProvider:  record.Traits.IsHostingProvider,
		IsAnonymousProxy:   record.Traits.IsAnonymousProxy,
		IsPublicProxy:      record.Traits.IsPublicProxy,
		IsResidentialProxy: record.Traits.IsResidentialProxy,
	}
	if len(record.Subdivisions) > 0 {
		info.Subdivision = strings.ToUpper(strings.TrimSpace(record.Subdivisions[0].ISOCode))
	}
	return info, nil
}

func (r *MaxMindResolver) Close() error {
	if r == nil || r.reader == nil {
		return nil
	}
	return r.reader.Close()
}

// DefaultBlockedCountries returns the OFAC comprehensively sanctioned
// jurisdictions plus the occupied regions of Ukraine where MaxMind exposes a
// country code. Operators can override the list via configuration.
func DefaultBlockedCountries() []string {
	return []string{
		"CU", // Cuba
		"IR", // Iran
		"KP", // North Korea
		"SY", // Syria
		"RU", // Russia
		"BY", // Belarus
		"MM", // Myanmar
		"SD", // Sudan
		"SS", // South Sudan
		"VE", // Venezuela
	}
}

// GeoScreenConfig controls which jurisdictions and anonymizers are rejected.
type GeoScreenConfig struct {
	BlockedCountries []string
	BlockVPN         bool
	BlockTor         bool
	BlockHosting     bool
	BlockProxy       bool
	// AllowPrivateIPs keeps loopback, RFC1918, and link-local callers working.
	// It defaults to true because those addresses never reach MaxMind and are
	// used by health checks, sidecars, and local development.
	AllowPrivateIPs bool
	// FailClosed rejects requests when the database cannot be consulted.
	FailClosed bool
}

func DefaultGeoScreenConfig() GeoScreenConfig {
	return GeoScreenConfig{
		BlockedCountries: DefaultBlockedCountries(),
		BlockVPN:         true,
		BlockTor:         true,
		BlockHosting:     true,
		BlockProxy:       true,
		AllowPrivateIPs:  true,
		FailClosed:       true,
	}
}

func (c GeoScreenConfig) countrySet() map[string]struct{} {
	set := make(map[string]struct{}, len(c.BlockedCountries))
	for _, code := range c.BlockedCountries {
		if normalized := strings.ToUpper(strings.TrimSpace(code)); normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	return set
}

// GeoDecision is the result of screening one caller.
type GeoDecision struct {
	Allowed     bool
	CountryCode string
	CountryName string
	Reason      string
	Flags       []string
}

const (
	GeoReasonSanctioned = "sanctioned_jurisdiction"
	GeoReasonAnonymizer = "anonymizer_detected"
	GeoReasonLookup     = "geo_lookup_failed"
)

// GeoScreener enforces jurisdiction and anonymizer policy. It is safe for
// concurrent use; the MaxMind reader supports concurrent lookups.
type GeoScreener struct {
	resolver IPResolver
	cfg      GeoScreenConfig
	blocked  map[string]struct{}
}

func NewGeoScreener(resolver IPResolver, cfg GeoScreenConfig) *GeoScreener {
	return &GeoScreener{
		resolver: resolver,
		cfg:      cfg,
		blocked:  cfg.countrySet(),
	}
}

func (s *GeoScreener) Configured() bool {
	return s != nil && s.resolver != nil
}

// Screen resolves ip and applies the configured policy. A lookup failure
// returns an error; the decision is denied when FailClosed is set so callers
// cannot accidentally treat a failed lookup as a pass.
func (s *GeoScreener) Screen(ctx context.Context, ip net.IP) (GeoDecision, error) {
	_ = ctx
	if s == nil || s.resolver == nil {
		return GeoDecision{Allowed: false, Reason: GeoReasonLookup}, errors.New("geo: screener is not configured")
	}
	if ip == nil {
		if s.cfg.FailClosed {
			return GeoDecision{Allowed: false, Reason: GeoReasonLookup}, errors.New("geo: caller IP is unavailable")
		}
		return GeoDecision{Allowed: true}, nil
	}
	if isPrivateIP(ip) {
		if s.cfg.AllowPrivateIPs {
			return GeoDecision{Allowed: true}, nil
		}
		return GeoDecision{Allowed: false, Reason: GeoReasonSanctioned}, nil
	}

	info, err := s.resolver.Lookup(ip)
	if err != nil {
		return GeoDecision{Allowed: !s.cfg.FailClosed, Reason: GeoReasonLookup}, err
	}

	decision := GeoDecision{
		Allowed:     true,
		CountryCode: info.CountryCode,
		CountryName: info.CountryName,
	}
	if _, blocked := s.blocked[info.CountryCode]; blocked {
		decision.Allowed = false
		decision.Reason = GeoReasonSanctioned
		return decision, nil
	}

	decision.Flags = anonymizerFlags(info, s.cfg)
	if len(decision.Flags) > 0 {
		decision.Allowed = false
		decision.Reason = GeoReasonAnonymizer
	}
	return decision, nil
}

func anonymizerFlags(info GeoInfo, cfg GeoScreenConfig) []string {
	flags := make([]string, 0, 6)
	if cfg.BlockVPN && info.IsAnonymousVPN {
		flags = append(flags, "anonymous_vpn")
	}
	if cfg.BlockTor && info.IsTorExitNode {
		flags = append(flags, "tor_exit_node")
	}
	if cfg.BlockHosting && info.IsHostingProvider {
		flags = append(flags, "hosting_provider")
	}
	if cfg.BlockProxy {
		if info.IsAnonymousProxy {
			flags = append(flags, "anonymous_proxy")
		}
		if info.IsPublicProxy {
			flags = append(flags, "public_proxy")
		}
		if info.IsResidentialProxy {
			flags = append(flags, "residential_proxy")
		}
	}
	return flags
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
