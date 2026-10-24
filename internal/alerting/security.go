package alerting

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// SecurityAlert is a security or compliance event that must reach an on-call
// responder. DedupKey lets PagerDuty collapse repeated findings for the same
// account or route into one incident instead of paging once per observation.
type SecurityAlert struct {
	Summary   string                 `json:"summary"`
	Source    string                 `json:"source"`
	Severity  Severity               `json:"severity"`
	DedupKey  string                 `json:"dedup_key,omitempty"`
	Details   map[string]interface{} `json:"details,omitempty"`
	Timestamp time.Time              `json:"timestamp,omitempty"`
}

// SecurityAlerter is the narrow contract fraud and compliance detectors need.
// PagerDutyClient implements it; tests can substitute a recorder.
type SecurityAlerter interface {
	Send(ctx context.Context, alert SecurityAlert) error
}

// PagerDutyConfig configures the Events API v2 client. Both routing keys are
// optional: a deployment may page only security, only compliance, or neither
// (in which case alerts are logged and skipped).
type PagerDutyConfig struct {
	SecurityRoutingKey   string
	ComplianceRoutingKey string
	Service              string
	Endpoint             string
	HTTPClient           *http.Client
	Timeout              time.Duration
}

// PagerDutyClient dispatches alerts to security and compliance PagerDuty
// services. It is safe for concurrent use.
type PagerDutyClient struct {
	notifiers []*PagerDutyNotifier
	service   string
}

func NewPagerDutyClient(cfg PagerDutyConfig) *PagerDutyClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	notifiers := make([]*PagerDutyNotifier, 0, 2)
	for _, key := range []string{cfg.SecurityRoutingKey, cfg.ComplianceRoutingKey} {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			notifiers = append(notifiers, NewPagerDutyNotifier(trimmed, cfg.Endpoint, httpClient))
		}
	}

	service := strings.TrimSpace(cfg.Service)
	if service == "" {
		service = "nexora"
	}
	return &PagerDutyClient{notifiers: notifiers, service: service}
}

// NewPagerDutyClientFromEnv reads the routing keys from the environment so the
// API and worker processes share one wiring path.
func NewPagerDutyClientFromEnv(service string) *PagerDutyClient {
	return NewPagerDutyClient(PagerDutyConfig{
		SecurityRoutingKey:   os.Getenv("PAGERDUTY_SECURITY_ROUTING_KEY"),
		ComplianceRoutingKey: os.Getenv("PAGERDUTY_COMPLIANCE_ROUTING_KEY"),
		Service:              service,
	})
}

func (c *PagerDutyClient) Configured() bool {
	return c != nil && len(c.notifiers) > 0
}

func (c *PagerDutyClient) Service() string {
	if c == nil {
		return ""
	}
	return c.service
}

// Send triggers one PagerDuty incident per configured routing key. All keys
// are attempted even if one fails, so a broken security integration cannot
// suppress the compliance page (and vice versa).
func (c *PagerDutyClient) Send(ctx context.Context, alert SecurityAlert) error {
	if !c.Configured() {
		log.Warn().
			Str("summary", alert.Summary).
			Str("source", alert.Source).
			Msg("alerting: no PagerDuty routing key configured, security alert skipped")
		return nil
	}

	summary := strings.TrimSpace(alert.Summary)
	if summary == "" {
		return errors.New("alerting: PagerDuty alert summary is required")
	}
	source := strings.TrimSpace(alert.Source)
	if source == "" {
		source = c.service
	}
	severity := alert.Severity
	if severity == "" {
		severity = SeverityError
	}
	timestamp := alert.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	incident := Incident{
		DedupKey:  alert.DedupKey,
		Summary:   summary,
		Source:    source,
		Severity:  severity,
		Details:   alert.Details,
		Timestamp: timestamp,
	}

	var errs []error
	for _, notifier := range c.notifiers {
		if err := notifier.Trigger(ctx, incident); err != nil {
			errs = append(errs, fmt.Errorf("send PagerDuty alert: %w", err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	log.Info().
		Str("summary", summary).
		Str("severity", string(severity)).
		Int("services", len(c.notifiers)).
		Msg("alerting: PagerDuty security alert sent")
	return nil
}
