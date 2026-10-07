package audit

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/postgres"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

type Service interface {
	Record(ctx context.Context, event *domain.AuditEvent) error
	Log(r *http.Request, action, resourceType, resourceID string, metadata map[string]interface{})
	List(ctx context.Context, filter domain.AuditFilter) ([]*domain.AuditEvent, int, error)
	Export(ctx context.Context, filter domain.AuditFilter, format string) ([]byte, string, error)
}

type service struct {
	repo *postgres.AuditRepo
}

func NewService(repo *postgres.AuditRepo) Service {
	return &service{repo: repo}
}

func (s *service) Record(ctx context.Context, event *domain.AuditEvent) error {
	if event.TenantID == "" {
		event.TenantID = tenant.IDFromContext(ctx)
	}
	if event.TenantID == "" {
		return nil // unauthenticated system operation
	}
	if event.Mode == "" {
		event.Mode = tenant.ModeOrDefault(ctx, domain.ModeLive)
	}
	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if event.ActorType == "" {
		if uID := tenant.UserIDFromContext(ctx); uID != "" {
			event.ActorType = "user"
			event.ActorID = uID
		} else if kID := tenant.APIKeyIDFromContext(ctx); kID != "" {
			event.ActorType = "api_key"
			event.ActorID = kID
		} else {
			event.ActorType = "system"
			event.ActorID = "system"
		}
	}
	return s.repo.Create(ctx, event)
}

func (s *service) Log(r *http.Request, action, resourceType, resourceID string, metadata map[string]interface{}) {
	ctx := r.Context()
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return
	}
	mode, _ := tenant.ModeFromContext(ctx)
	if !mode.Valid() {
		mode = domain.ModeLive
	}

	actorType := "system"
	actorID := "system"
	if uID := tenant.UserIDFromContext(ctx); uID != "" {
		actorType = "user"
		actorID = uID
	} else if kID := tenant.APIKeyIDFromContext(ctx); kID != "" {
		actorType = "api_key"
		actorID = kID
	}

	ip := clientIP(r)
	ua := r.UserAgent()

	event := &domain.AuditEvent{
		ID:           uuid.New().String(),
		TenantID:     tenantID,
		Mode:         mode,
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Metadata:     metadata,
		IPAddress:    &ip,
		UserAgent:    &ua,
		CreatedAt:    time.Now().UTC(),
	}

	if err := s.repo.Create(ctx, event); err != nil {
		log.Error().Err(err).Str("tenant_id", tenantID).Str("action", action).Msg("failed to persist audit log")
	}
}

func (s *service) List(ctx context.Context, filter domain.AuditFilter) ([]*domain.AuditEvent, int, error) {
	tenantID := tenant.IDFromContext(ctx)
	if tenantID == "" {
		return nil, 0, fmt.Errorf("tenant ID required")
	}
	mode, _ := tenant.ModeFromContext(ctx)
	if !mode.Valid() {
		mode = domain.ModeLive
	}
	return s.repo.List(ctx, tenantID, mode, filter)
}

func (s *service) Export(ctx context.Context, filter domain.AuditFilter, format string) ([]byte, string, error) {
	// For export, allow up to 1000 records
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 1000
	}
	events, _, err := s.List(ctx, filter)
	if err != nil {
		return nil, "", err
	}

	if strings.ToLower(format) == "csv" {
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write([]string{"id", "created_at", "actor_type", "actor_id", "action", "resource_type", "resource_id", "metadata", "ip_address", "user_agent"})
		for _, e := range events {
			metaBytes, _ := json.Marshal(e.Metadata)
			ip := ""
			if e.IPAddress != nil {
				ip = *e.IPAddress
			}
			ua := ""
			if e.UserAgent != nil {
				ua = *e.UserAgent
			}
			_ = w.Write([]string{
				e.ID,
				e.CreatedAt.Format(time.RFC3339),
				e.ActorType,
				e.ActorID,
				e.Action,
				e.ResourceType,
				e.ResourceID,
				string(metaBytes),
				ip,
				ua,
			})
		}
		w.Flush()
		return buf.Bytes(), "text/csv", nil
	}

	data, err := json.Marshal(map[string]interface{}{
		"events": events,
		"count":  len(events),
	})
	if err != nil {
		return nil, "", err
	}
	return data, "application/json", nil
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
