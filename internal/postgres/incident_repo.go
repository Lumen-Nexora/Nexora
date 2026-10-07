package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/jackc/pgx/v5"
)

type IncidentRepository struct {
	db DB
}

func NewIncidentRepository(db DB) *IncidentRepository {
	return &IncidentRepository{db: db}
}

func (r *IncidentRepository) Create(ctx context.Context, inc *domain.Incident) error {
	query := `
		INSERT INTO incidents (id, title, description, severity, status, created_at, resolved_at)
		VALUES (COALESCE(NULLIF($1, ''), gen_random_uuid()), $2, $3, $4, $5, COALESCE($6, NOW()), $7)
		RETURNING id, title, description, severity, status, created_at, resolved_at
	`
	err := r.db.QueryRow(ctx, query,
		inc.ID,
		inc.Title,
		inc.Description,
		inc.Severity,
		inc.Status,
		nullTime(inc.CreatedAt),
		inc.ResolvedAt,
	).Scan(&inc.ID, &inc.Title, &inc.Description, &inc.Severity, &inc.Status, &inc.CreatedAt, &inc.ResolvedAt)
	return err
}

func (r *IncidentRepository) GetByID(ctx context.Context, id string) (*domain.Incident, error) {
	query := `
		SELECT id, title, description, severity, status, created_at, resolved_at
		FROM incidents
		WHERE id = $1
	`
	inc := &domain.Incident{}
	err := r.db.QueryRow(ctx, query, id).Scan(
		&inc.ID,
		&inc.Title,
		&inc.Description,
		&inc.Severity,
		&inc.Status,
		&inc.CreatedAt,
		&inc.ResolvedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIncidentNotFound
	}
	return inc, err
}

func (r *IncidentRepository) List(ctx context.Context, limit int) ([]domain.Incident, error) {
	if limit <= 0 {
		limit = 50
	}
	return r.list(ctx, `
		SELECT id, title, description, severity, status, created_at, resolved_at
		FROM incidents
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
}

// ListActive returns unresolved incidents, worst severity first, so the public
// status endpoint can derive the platform state without paging the full
// incident history.
func (r *IncidentRepository) ListActive(ctx context.Context) ([]domain.Incident, error) {
	return r.list(ctx, `
		SELECT id, title, description, severity, status, created_at, resolved_at
		FROM incidents
		WHERE status <> 'resolved'
		ORDER BY
			CASE severity WHEN 'critical' THEN 1 WHEN 'major' THEN 2 ELSE 3 END,
			created_at DESC
	`)
}

func (r *IncidentRepository) list(ctx context.Context, query string, args ...interface{}) ([]domain.Incident, error) {
	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var incidents []domain.Incident
	for rows.Next() {
		var inc domain.Incident
		if err := rows.Scan(&inc.ID, &inc.Title, &inc.Description, &inc.Severity, &inc.Status, &inc.CreatedAt, &inc.ResolvedAt); err != nil {
			return nil, err
		}
		incidents = append(incidents, inc)
	}
	return incidents, rows.Err()
}

func (r *IncidentRepository) Update(ctx context.Context, inc *domain.Incident) error {
	query := `
		UPDATE incidents
		SET title = $2, description = $3, severity = $4, status = $5, resolved_at = $6
		WHERE id = $1
		RETURNING id, title, description, severity, status, created_at, resolved_at
	`
	err := r.db.QueryRow(ctx, query,
		inc.ID,
		inc.Title,
		inc.Description,
		inc.Severity,
		inc.Status,
		inc.ResolvedAt,
	).Scan(&inc.ID, &inc.Title, &inc.Description, &inc.Severity, &inc.Status, &inc.CreatedAt, &inc.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrIncidentNotFound
	}
	return err
}

func nullTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}
