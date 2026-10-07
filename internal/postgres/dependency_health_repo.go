package postgres

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

var dependencyNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type DependencyHealthRepository struct {
	db DB
}

func NewDependencyHealthRepository(db DB) *DependencyHealthRepository {
	return &DependencyHealthRepository{db: db}
}

func (r *DependencyHealthRepository) RecordDependencyHealth(ctx context.Context, checks []domain.DependencyHealthCheck) error {
	if len(checks) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		if !dependencyNamePattern.MatchString(check.Dependency) ||
			(check.Status != "healthy" && check.Status != "unhealthy") ||
			check.LatencyMS < 0 || check.CheckedAt.IsZero() {
			return errors.New("invalid dependency health check")
		}
		if _, exists := seen[check.Dependency]; exists {
			return errors.New("duplicate dependency in health snapshot")
		}
		seen[check.Dependency] = struct{}{}
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, check := range checks {
		_, err = tx.Exec(ctx, `
			INSERT INTO dependency_health_checks (dependency, status, latency_ms, checked_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (dependency, checked_at) DO NOTHING
		`, check.Dependency, check.Status, check.LatencyMS, check.CheckedAt.UTC())
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *DependencyHealthRepository) PruneDependencyHealth(ctx context.Context, before time.Time) error {
	_, err := r.db.Exec(ctx, `DELETE FROM dependency_health_checks WHERE checked_at < $1`, before.UTC())
	return err
}

func (r *DependencyHealthRepository) LatestDependencyHealth(ctx context.Context) ([]domain.DependencyHealthCheck, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT ON (dependency) dependency, status, latency_ms, checked_at
		FROM dependency_health_checks
		ORDER BY dependency, checked_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	checks := make([]domain.DependencyHealthCheck, 0)
	for rows.Next() {
		var check domain.DependencyHealthCheck
		if err := rows.Scan(&check.Dependency, &check.Status, &check.LatencyMS, &check.CheckedAt); err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	return checks, rows.Err()
}

func (r *DependencyHealthRepository) ListDependencyHealth(ctx context.Context, dependency string, since *time.Time, limit int) ([]domain.DependencyHealthCheck, error) {
	if dependency != "" && !dependencyNamePattern.MatchString(dependency) {
		return nil, errors.New("invalid dependency name")
	}
	if limit < 1 || limit > 500 {
		return nil, errors.New("invalid dependency health history limit")
	}

	rows, err := r.db.Query(ctx, `
		SELECT dependency, status, latency_ms, checked_at
		FROM dependency_health_checks
		WHERE ($1 = '' OR dependency = $1)
		  AND ($2::timestamptz IS NULL OR checked_at >= $2)
		ORDER BY checked_at DESC, dependency ASC
		LIMIT $3
	`, dependency, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	checks := make([]domain.DependencyHealthCheck, 0)
	for rows.Next() {
		var check domain.DependencyHealthCheck
		if err := rows.Scan(&check.Dependency, &check.Status, &check.LatencyMS, &check.CheckedAt); err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	return checks, rows.Err()
}
