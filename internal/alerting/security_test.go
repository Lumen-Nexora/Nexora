package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPagerDutyClientSendsToSecurityAndCompliance(t *testing.T) {
	var mu sync.Mutex
	var events []pagerDutyEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		var event pagerDutyEvent
		require.NoError(t, json.NewDecoder(r.Body).Decode(&event))
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := NewPagerDutyClient(PagerDutyConfig{
		SecurityRoutingKey:   "security-key",
		ComplianceRoutingKey: "compliance-key",
		Service:              "nexora-test",
		Endpoint:             server.URL,
	})

	err := client.Send(context.Background(), SecurityAlert{
		Summary:  "micro-variance skimming detected",
		Severity: SeverityCritical,
		DedupKey: "fraud:route-1",
		Details:  map[string]interface{}{"route_id": "route-1"},
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 2)
	keys := []string{events[0].RoutingKey, events[1].RoutingKey}
	assert.ElementsMatch(t, []string{"security-key", "compliance-key"}, keys)
	assert.Equal(t, "trigger", events[0].EventAction)
	require.NotNil(t, events[0].Payload)
	assert.Equal(t, "micro-variance skimming detected", events[0].Payload.Summary)
	assert.Equal(t, SeverityCritical, events[0].Payload.Severity)
	assert.Equal(t, "fraud:route-1", events[0].DedupKey)
	assert.Equal(t, "route-1", events[0].Payload.CustomDetails["route_id"])
}

func TestPagerDutyClientWithoutRoutingKeysSkips(t *testing.T) {
	client := NewPagerDutyClient(PagerDutyConfig{})
	assert.False(t, client.Configured())
	require.NoError(t, client.Send(context.Background(), SecurityAlert{Summary: "test"}))
}

func TestPagerDutyClientReturnsErrorOnFailureStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewPagerDutyClient(PagerDutyConfig{
		SecurityRoutingKey: "security-key",
		Endpoint:           server.URL,
		HTTPClient:         &http.Client{Timeout: time.Second},
	})
	err := client.Send(context.Background(), SecurityAlert{Summary: "test"})
	require.Error(t, err)
}
