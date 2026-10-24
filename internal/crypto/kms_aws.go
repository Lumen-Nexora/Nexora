package crypto

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	awsKMSProvider = "aws-kms"
	// AWS KMS accepts automatic rotation periods of 90 to 2560 days.
	awsMinRotationDays = 90
	awsMaxRotationDays = 2560
	maxKMSResponse     = 1 << 20
)

// AWSKMSConfig configures AWS KMS. Credentials are static (typically from
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN).
type AWSKMSConfig struct {
	Region          string
	KeyID           string // key ID, key ARN, alias name or alias ARN
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Endpoint overrides https://kms.<region>.amazonaws.com (VPC endpoints,
	// FIPS endpoints, local mocks).
	Endpoint   string
	HTTPClient *http.Client
}

// AWSKMS wraps data keys with an AWS KMS symmetric key through the KMS JSON
// API, signed with Signature Version 4.
type AWSKMS struct {
	cfg      AWSKMSConfig
	endpoint string
	client   *http.Client
	now      func() time.Time
}

// NewAWSKMS validates the configuration and builds the client.
func NewAWSKMS(cfg AWSKMSConfig) (*AWSKMS, error) {
	if cfg.Region == "" || cfg.KeyID == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("%w: aws kms needs region, key id, access key id and secret access key", ErrInvalidKMSConfig)
	}
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = "https://kms." + cfg.Region + ".amazonaws.com"
	}
	return &AWSKMS{cfg: cfg, endpoint: endpoint, client: httpClient(cfg.HTTPClient), now: time.Now}, nil
}

func (a *AWSKMS) Name() string { return awsKMSProvider }

func (a *AWSKMS) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	var out struct{ CiphertextBlob []byte }
	err := a.call(ctx, "Encrypt", struct {
		KeyId     string
		Plaintext []byte
	}{a.cfg.KeyID, dek}, &out)
	return out.CiphertextBlob, err
}

func (a *AWSKMS) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var out struct{ Plaintext []byte }
	err := a.call(ctx, "Decrypt", struct {
		CiphertextBlob []byte
		KeyId          string
	}{wrapped, a.cfg.KeyID}, &out)
	return out.Plaintext, err
}

// Rewrap uses ReEncrypt, so the data key is never exposed to Nexora.
func (a *AWSKMS) Rewrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var out struct{ CiphertextBlob []byte }
	err := a.call(ctx, "ReEncrypt", struct {
		CiphertextBlob   []byte
		DestinationKeyId string
	}{wrapped, a.cfg.KeyID}, &out)
	return out.CiphertextBlob, err
}

// EnsureRotation enables automatic key rotation with the given period.
func (a *AWSKMS) EnsureRotation(ctx context.Context, policy RotationPolicy) error {
	days := int(math.Ceil(policy.Period.Hours() / 24))
	if days < awsMinRotationDays || days > awsMaxRotationDays {
		return fmt.Errorf("%w: aws kms rotation period must be %d-%d days, got %d", ErrInvalidKMSConfig, awsMinRotationDays, awsMaxRotationDays, days)
	}
	return a.call(ctx, "EnableKeyRotation", struct {
		KeyId                string
		RotationPeriodInDays int
	}{a.cfg.KeyID, days}, nil)
}

func (a *AWSKMS) call(ctx context.Context, action string, in, out interface{}) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		Zeroize(body)
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+action)
	signAWSv4(req, body, a.cfg.AccessKeyID, a.cfg.SecretAccessKey, a.cfg.SessionToken, a.cfg.Region, "kms", a.now())
	return doKMS(a.client, req, body, out, awsKMSProvider+" "+action, func(raw []byte) string {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Msg     string `json:"Message"`
		}
		_ = json.Unmarshal(raw, &e)
		return strings.TrimSpace(e.Type + " " + e.Message + e.Msg)
	})
}

// signAWSv4 adds Signature Version 4 headers to req. body must be the exact
// request body.
func signAWSv4(req *http.Request, body []byte, accessKey, secretKey, sessionToken, region, service string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	if sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", sessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		headers[strings.ToLower(name)] = strings.TrimSpace(strings.Join(values, ","))
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	payloadHash := sha256.Sum256(body)
	canonicalRequest := strings.Join([]string{
		req.Method, path, canonicalQuery(req.URL.Query()), canonicalHeaders.String(), signedHeaders,
		hex.EncodeToString(payloadHash[:]),
	}, "\n")

	scope := date + "/" + region + "/" + service + "/aws4_request"
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(requestHash[:])

	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func canonicalQuery(values map[string][]string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape percent-encodes everything except RFC 3986 unreserved characters.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: kmsCallTimeout}
}

// doKMS sends a provider request and decodes the JSON response into out.
// Request and response bodies may carry key material, so both buffers are
// zeroized before returning. errMessage extracts a provider error message
// from a non-2xx body; error bodies never contain key material.
func doKMS(client *http.Client, req *http.Request, body []byte, out interface{}, op string, errMessage func([]byte) string) error {
	defer Zeroize(body)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrKMSRequest, op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxKMSResponse))
	defer Zeroize(raw)
	if err != nil {
		return fmt.Errorf("%w: %s: read response: %v", ErrKMSRequest, op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: %s: HTTP %d %s", ErrKMSRequest, op, resp.StatusCode, errMessage(raw))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %s: decode response: %v", ErrKMSRequest, op, err)
	}
	return nil
}
