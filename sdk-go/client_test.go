package nexora

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewClientRequiresAPIKey(t *testing.T) {
	if _, err := NewClient(Config{}); err == nil {
		t.Fatal("NewClient() accepted an empty API key")
	}
}

func TestClientErrorEnvelope(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"invalid amount"},"validation_errors":[{"field":"amount"}]}`))
	}))
	defer server.Close()

	var result map[string]any
	err := client.request(context.Background(), http.MethodGet, "/test", nil, nil, &result)
	var nexoraErr *NexoraError
	if !errors.As(err, &nexoraErr) {
		t.Fatalf("request error = %T, want *NexoraError", err)
	}
	if nexoraErr.Code != "BAD_REQUEST" || nexoraErr.Message != "invalid amount" || nexoraErr.HTTPStatus != 400 {
		t.Fatalf("unexpected error fields: %+v", nexoraErr)
	}
	if nexoraErr.Details == nil {
		t.Fatal("validation_errors were not retained in Details")
	}
}

func TestResourceSuccessAndError(t *testing.T) {
	errorMode := false
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if errorMode {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"BAD_REQUEST","message":"test failure"}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/keys") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	checks := []struct {
		name string
		call func() error
	}{
		{"wallets", func() error { _, err := client.Wallets.Create(context.Background()); return err }},
		{"transfers", func() error {
			_, err := client.Transfers.Create(context.Background(), CreateTransferRequest{})
			return err
		}},
		{"fx", func() error { _, err := client.FX.GetRates(context.Background(), "USD", "USDC"); return err }},
		{"schedules", func() error { _, err := client.Schedules.List(context.Background()); return err }},
		{"webhooks", func() error { _, err := client.Webhooks.List(context.Background()); return err }},
		{"fees", func() error { _, err := client.Fees.Get(context.Background()); return err }},
		{"keys", func() error { _, err := client.Keys.List(context.Background()); return err }},
		{"fiat", func() error {
			_, err := client.Fiat.Deposit(context.Background(), "wallet-1", FiatDepositRequest{})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			errorMode = false
			if err := check.call(); err != nil {
				t.Fatalf("successful response: %v", err)
			}
			errorMode = true
			err := check.call()
			var nexoraErr *NexoraError
			if !errors.As(err, &nexoraErr) || nexoraErr.Code != "BAD_REQUEST" {
				t.Fatalf("error response = %v, want typed BAD_REQUEST", err)
			}
		})
	}
}

type conformanceCase struct {
	Name             string `json:"name"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	IdempotencyKey   string `json:"idempotency_key"`
	MaxRetries       int    `json:"max_retries"`
	Statuses         []int  `json:"statuses"`
	RetryAfter       string `json:"retry_after"`
	KeyMode          string `json:"key_mode"`
	ExpectedRequests int    `json:"expected_requests"`
	ExpectedError    bool   `json:"expected_error"`
}

func TestSharedConformanceCases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "sdk", "conformance", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Cases []conformanceCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &suite); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range suite.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			var calls int
			var keys []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				status := testCase.Statuses[min(calls-1, len(testCase.Statuses)-1)]
				if calls == 1 && testCase.RetryAfter != "" {
					w.Header().Set("Retry-After", testCase.RetryAfter)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status >= 400 {
					_, _ = w.Write([]byte(`{"error":{"code":"TEMPORARY","message":"retry"}}`))
				} else {
					_, _ = w.Write([]byte(`{"ok":true}`))
				}
			}))
			defer server.Close()
			client, err := NewClient(Config{APIKey: "test-key", BaseURL: server.URL, MaxRetries: testCase.MaxRetries, RetryDelay: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			options := RequestOptions{IdempotencyKey: testCase.IdempotencyKey}
			var response map[string]any
			err = client.request(context.Background(), testCase.Method, testCase.Path, nil, nil, &response, options)
			if testCase.ExpectedError && err == nil {
				t.Fatal("expected request error")
			}
			if !testCase.ExpectedError && err != nil {
				t.Fatalf("request error: %v", err)
			}
			if calls != testCase.ExpectedRequests {
				t.Fatalf("request count = %d, want %d", calls, testCase.ExpectedRequests)
			}
			for _, key := range keys {
				switch testCase.KeyMode {
				case "generated":
					if key == "" || key != keys[0] {
						t.Fatalf("generated key not present/stable: %v", keys)
					}
				case "caller":
					if key != testCase.IdempotencyKey {
						t.Fatalf("key = %q, want caller key %q", key, testCase.IdempotencyKey)
					}
				case "none":
					if key != "" {
						t.Fatalf("unexpected idempotency key %q", key)
					}
				}
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got, ok := parseRetryAfter("2", time.Now()); !ok || got != 2*time.Second {
		t.Fatalf("parseRetryAfter() = (%s, %t), want (2s, true)", got, ok)
	}
}

func TestQueryEscapingAndCallerKey(t *testing.T) {
	var gotPath, gotKey string
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.RequestURI(), r.Header.Get("Idempotency-Key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	_, err := client.Transfers.Create(context.Background(), CreateTransferRequest{}, RequestOptions{IdempotencyKey: "my-key"})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "my-key" || !strings.HasPrefix(gotPath, "/v1/transfers") {
		t.Fatalf("request path/key = %q/%q", gotPath, gotKey)
	}
}

func TestCustomHTTPClientReuse(t *testing.T) {
	httpClient := &http.Client{}
	client, err := NewClient(Config{APIKey: "test-key", HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	if client.http != httpClient {
		t.Fatal("configured HTTP client was not reused")
	}
	if !reflect.DeepEqual(client.http, httpClient) {
		t.Fatal("unexpected HTTP client configuration")
	}
}

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := NewClient(Config{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}
