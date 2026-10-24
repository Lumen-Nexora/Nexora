package compliance

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeGeoResolver struct {
	infos map[string]GeoInfo
	err   error
}

func (f *fakeGeoResolver) Lookup(ip net.IP) (GeoInfo, error) {
	if f.err != nil {
		return GeoInfo{}, f.err
	}
	return f.infos[ip.String()], nil
}

func newGeoTestScreener(resolver IPResolver) *GeoScreener {
	cfg := DefaultGeoScreenConfig()
	return NewGeoScreener(resolver, cfg)
}

func TestGeoScreenerRejectsSanctionedJurisdiction(t *testing.T) {
	resolver := &fakeGeoResolver{infos: map[string]GeoInfo{
		"203.0.113.10": {CountryCode: "IR", CountryName: "Iran"},
	}}
	screener := newGeoTestScreener(resolver)

	decision, err := screener.Screen(context.Background(), net.ParseIP("203.0.113.10"))
	require.NoError(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, GeoReasonSanctioned, decision.Reason)
	assert.Equal(t, "IR", decision.CountryCode)
}

func TestGeoScreenerRejectsAnonymizers(t *testing.T) {
	cases := []struct {
		name string
		info GeoInfo
		flag string
	}{
		{"vpn", GeoInfo{CountryCode: "US", IsAnonymousVPN: true}, "anonymous_vpn"},
		{"tor", GeoInfo{CountryCode: "US", IsTorExitNode: true}, "tor_exit_node"},
		{"hosting", GeoInfo{CountryCode: "US", IsHostingProvider: true}, "hosting_provider"},
		{"proxy", GeoInfo{CountryCode: "US", IsPublicProxy: true}, "public_proxy"},
		{"residential proxy", GeoInfo{CountryCode: "US", IsResidentialProxy: true}, "residential_proxy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &fakeGeoResolver{infos: map[string]GeoInfo{"198.51.100.7": tc.info}}
			decision, err := newGeoTestScreener(resolver).Screen(context.Background(), net.ParseIP("198.51.100.7"))
			require.NoError(t, err)
			assert.False(t, decision.Allowed)
			assert.Equal(t, GeoReasonAnonymizer, decision.Reason)
			assert.Contains(t, decision.Flags, tc.flag)
		})
	}
}

func TestGeoScreenerAllowsPermittedCountry(t *testing.T) {
	resolver := &fakeGeoResolver{infos: map[string]GeoInfo{
		"198.51.100.20": {CountryCode: "DE", CountryName: "Germany"},
	}}
	decision, err := newGeoTestScreener(resolver).Screen(context.Background(), net.ParseIP("198.51.100.20"))
	require.NoError(t, err)
	assert.True(t, decision.Allowed)
	assert.Empty(t, decision.Reason)
}

func TestGeoScreenerAllowsPrivateAddresses(t *testing.T) {
	screener := newGeoTestScreener(&fakeGeoResolver{})
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.5", "::1"} {
		decision, err := screener.Screen(context.Background(), net.ParseIP(raw))
		require.NoError(t, err)
		assert.True(t, decision.Allowed, raw)
	}
}

func TestGeoScreenerFailsClosedOnLookupError(t *testing.T) {
	screener := newGeoTestScreener(&fakeGeoResolver{err: errors.New("database unavailable")})
	decision, err := screener.Screen(context.Background(), net.ParseIP("198.51.100.30"))
	require.Error(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, GeoReasonLookup, decision.Reason)
}

func TestGeoScreenerCanFailOpen(t *testing.T) {
	cfg := DefaultGeoScreenConfig()
	cfg.FailClosed = false
	screener := NewGeoScreener(&fakeGeoResolver{err: errors.New("database unavailable")}, cfg)

	decision, err := screener.Screen(context.Background(), net.ParseIP("198.51.100.30"))
	require.Error(t, err)
	assert.True(t, decision.Allowed)
}

func TestGeoScreenerCustomBlocklist(t *testing.T) {
	cfg := DefaultGeoScreenConfig()
	cfg.BlockedCountries = []string{"xx"}
	resolver := &fakeGeoResolver{infos: map[string]GeoInfo{
		"198.51.100.40": {CountryCode: "XX"},
	}}
	decision, err := NewGeoScreener(resolver, cfg).Screen(context.Background(), net.ParseIP("198.51.100.40"))
	require.NoError(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, GeoReasonSanctioned, decision.Reason)
}

func TestOpenMaxMindResolverRequiresDatabase(t *testing.T) {
	_, err := OpenMaxMindResolver("")
	require.Error(t, err)

	_, err = OpenMaxMindResolver("does-not-exist.mmdb")
	require.Error(t, err)
}
