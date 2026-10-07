package nexora

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.nexora.io"

type Config struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	MaxRetries int
	RetryDelay time.Duration
	HTTPClient *http.Client
}

type RequestOptions struct {
	IdempotencyKey string
	Timeout        time.Duration
	Headers        http.Header
}

type Client struct {
	baseURL    string
	apiKey     string
	http       *http.Client
	maxRetries int
	retryDelay time.Duration

	Wallets      *WalletsResource
	Transfers    *TransfersResource
	FX           *FXResource
	Schedules    *SchedulesResource
	Webhooks     *WebhooksResource
	Fees         *FeesResource
	Keys         *KeysResource
	Fiat         *FiatResource
	PaymentLinks *PaymentLinksResource
	Refunds      *RefundsResource
}

func NewClient(config Config) (*Client, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("nexora: APIKey is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.MaxRetries < 0 {
		config.MaxRetries = 0
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = 500 * time.Millisecond
	}
	transport := config.HTTPClient
	if transport == nil {
		transport = &http.Client{Timeout: config.Timeout}
	}
	c := &Client{
		baseURL:    strings.TrimRight(strings.TrimSuffix(config.BaseURL, "/v1"), "/") + "/v1",
		apiKey:     config.APIKey,
		http:       transport,
		maxRetries: config.MaxRetries,
		retryDelay: config.RetryDelay,
	}
	c.Wallets = &WalletsResource{client: c}
	c.Transfers = &TransfersResource{client: c}
	c.FX = &FXResource{client: c}
	c.Schedules = &SchedulesResource{client: c}
	c.Webhooks = &WebhooksResource{client: c}
	c.Fees = &FeesResource{client: c}
	c.Keys = &KeysResource{client: c}
	c.Fiat = &FiatResource{client: c}
	c.PaymentLinks = &PaymentLinksResource{client: c}
	c.Refunds = &RefundsResource{client: c}
	return c, nil
}

func (c *Client) Health(ctx context.Context, options ...RequestOptions) (HealthResponse, error) {
	var response HealthResponse
	err := c.request(ctx, http.MethodGet, "/../health", nil, nil, &response, options...)
	return response, err
}

func (c *Client) request(ctx context.Context, method, path string, body any, query url.Values, output any, options ...RequestOptions) error {
	option := RequestOptions{}
	if len(options) > 0 {
		option = options[0]
	}
	if option.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, option.Timeout)
		defer cancel()
	}
	bodyBytes, err := json.Marshal(body)
	if body == nil {
		bodyBytes = nil
	} else if err != nil {
		return fmt.Errorf("nexora: encode request: %w", err)
	}
	key := option.IdempotencyKey
	if key == "" {
		key = option.Headers.Get("Idempotency-Key")
	}
	if key == "" {
		key = option.Headers.Get("X-Idempotency-Key")
	}
	if key == "" && method == http.MethodPost && idempotencyRequired(path) {
		key, err = newIdempotencyKey()
		if err != nil {
			return fmt.Errorf("nexora: create idempotency key: %w", err)
		}
	}
	canRetry := method == http.MethodGet || method == http.MethodHead || key != ""
	endpoint := c.baseURL + path
	if path == "/../health" {
		endpoint = strings.TrimSuffix(c.baseURL, "/v1") + "/health"
	}
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	for attempt := 0; ; attempt++ {
		var requestBody io.Reader
		if bodyBytes != nil {
			requestBody = bytes.NewReader(bodyBytes)
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
		if err != nil {
			return fmt.Errorf("nexora: create request: %w", err)
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
		if bodyBytes != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		for name, values := range option.Headers {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
		if key != "" && request.Header.Get("Idempotency-Key") == "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, requestErr := c.http.Do(request)
		if requestErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if canRetry && attempt < c.maxRetries {
				if err := c.wait(ctx, c.retryDelay*time.Duration(1<<attempt)); err != nil {
					return err
				}
				continue
			}
			return &NexoraError{Code: "NETWORK_ERROR", Message: requestErr.Error()}
		}
		responseBody, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			if canRetry && attempt < c.maxRetries {
				continue
			}
			return &NexoraError{Code: "NETWORK_ERROR", Message: readErr.Error()}
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			delay, hasRetryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
			if canRetry && retryableStatus(response.StatusCode) && attempt < c.maxRetries {
				if !hasRetryAfter {
					delay = c.retryDelay * time.Duration(1<<attempt)
				}
				if err := c.wait(ctx, delay); err != nil {
					return err
				}
				continue
			}
			retryAfter := 0
			if hasRetryAfter {
				retryAfter = int(delay.Seconds())
			}
			return parseError(response.StatusCode, responseBody, retryAfter)
		}
		if response.StatusCode == http.StatusNoContent || len(responseBody) == 0 {
			return nil
		}
		if text, ok := output.(*string); ok && !strings.Contains(response.Header.Get("Content-Type"), "json") {
			*text = string(responseBody)
			return nil
		}
		if output == nil {
			return nil
		}
		if err := json.Unmarshal(responseBody, output); err != nil {
			return fmt.Errorf("nexora: decode response: %w", err)
		}
		return nil
	}
}

func (c *Client) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func idempotencyRequired(path string) bool {
	if path == "/wallets" || path == "/transfers" || path == "/transfers/batch" || path == "/withdrawals" || path == "/fx/convert" || path == "/schedules" || path == "/claimable-balances" || path == "/payment-links" || path == "/refunds" {
		return true
	}
	return strings.HasPrefix(path, "/wallets/") && (strings.HasSuffix(path, "/deposit/fiat") || strings.HasSuffix(path, "/withdraw/fiat") || strings.HasSuffix(path, "/trustlines")) || strings.HasPrefix(path, "/claimable-balances/") && strings.HasSuffix(path, "/claim")
}

func newIdempotencyKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		if delay := at.Sub(now); delay > 0 {
			return delay, true
		}
		return 0, true
	}
	return 0, false
}
