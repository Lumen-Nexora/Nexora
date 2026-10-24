package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/compliance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type geoMiddlewareResolver struct {
	info compliance.GeoInfo
}

func (r geoMiddlewareResolver) Lookup(net.IP) (compliance.GeoInfo, error) {
	return r.info, nil
}

type recordingGeoAudit struct {
	action   string
	metadata map[string]interface{}
}

func (r *recordingGeoAudit) Log(_ *http.Request, action, _, _ string, metadata map[string]interface{}) {
	r.action = action
	r.metadata = metadata
}

func TestGeoScreeningBlocksSanctionedCaller(t *testing.T) {
	screener := compliance.NewGeoScreener(
		geoMiddlewareResolver{info: compliance.GeoInfo{CountryCode: "IR", CountryName: "Iran"}},
		compliance.DefaultGeoScreenConfig(),
	)
	audit := &recordingGeoAudit{}
	nextCalled := false

	handler := GeoScreening(GeoScreenOptions{Screener: screener, Audit: audit})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/wallets", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, nextCalled)
	assert.Equal(t, "security.geo_blocked", audit.action)
	require.NotNil(t, audit.metadata)
	assert.Equal(t, "IR", audit.metadata["country_code"])
	assert.Equal(t, "203.0.113.10", audit.metadata["ip"])
}

func TestGeoScreeningAllowsPermittedCaller(t *testing.T) {
	screener := compliance.NewGeoScreener(
		geoMiddlewareResolver{info: compliance.GeoInfo{CountryCode: "DE", CountryName: "Germany"}},
		compliance.DefaultGeoScreenConfig(),
	)
	audit := &recordingGeoAudit{}
	nextCalled := false

	handler := GeoScreening(GeoScreenOptions{Screener: screener, Audit: audit})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/wallets", nil)
	req.RemoteAddr = "198.51.100.20:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, nextCalled)
	assert.Empty(t, audit.action)
}

func TestGeoScreeningAllowsPrivateCaller(t *testing.T) {
	screener := compliance.NewGeoScreener(
		geoMiddlewareResolver{},
		compliance.DefaultGeoScreenConfig(),
	)
	nextCalled := false
	handler := GeoScreening(GeoScreenOptions{Screener: screener})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, nextCalled)
}
