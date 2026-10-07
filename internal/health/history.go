package health

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

const (
	SampleInterval   = time.Minute
	HistoryRetention = 30 * 24 * time.Hour
	HistoryMaxLimit  = 500
)

// DependencyCheck probes one platform dependency. Probe results are reduced to
// status and latency; their error text is never stored or returned publicly.
type DependencyCheck func(context.Context) error

// HistoryRecorder stores sanitized snapshots and expires old observations.
type HistoryRecorder interface {
	RecordDependencyHealth(context.Context, []domain.DependencyHealthCheck) error
	PruneDependencyHealth(context.Context, time.Time) error
}

type Sampler struct {
	checks    map[string]DependencyCheck
	recorder  HistoryRecorder
	startOnce sync.Once
	pruneMu   sync.Mutex
	lastPrune time.Time
}

func NewSampler(checks map[string]DependencyCheck, recorder HistoryRecorder) *Sampler {
	copied := make(map[string]DependencyCheck, len(checks))
	for name, check := range checks {
		copied[name] = check
	}
	return &Sampler{checks: copied, recorder: recorder}
}

// Start records an initial snapshot and then samples once per minute until ctx
// is cancelled. Calling Start more than once does not create duplicate loops.
func (s *Sampler) Start(ctx context.Context) {
	if s == nil || s.recorder == nil {
		return
	}
	s.startOnce.Do(func() {
		go func() {
			s.run(ctx)
			ticker := time.NewTicker(SampleInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.run(ctx)
				}
			}
		}()
	})
}

// SampleNow performs one bounded snapshot and is exposed for deterministic tests.
func (s *Sampler) SampleNow(ctx context.Context) error {
	if s == nil || s.recorder == nil {
		return errors.New("dependency health history is not configured")
	}

	type result struct {
		name string
		ok   bool
		ms   int64
	}
	results := make(chan result, len(s.checks))
	for name, check := range s.checks {
		go func(name string, check DependencyCheck) {
			started := time.Now()
			checkCtx, cancel := context.WithTimeout(ctx, dependencyTimeout(name))
			defer cancel()
			ok := check != nil && check(checkCtx) == nil
			results <- result{name: name, ok: ok, ms: time.Since(started).Milliseconds()}
		}(name, check)
	}

	checkedAt := time.Now().UTC()
	checks := make([]domain.DependencyHealthCheck, 0, len(s.checks))
	for range s.checks {
		result := <-results
		status := "unhealthy"
		if result.ok {
			status = "healthy"
		}
		checks = append(checks, domain.DependencyHealthCheck{
			Dependency: result.name,
			Status:     status,
			LatencyMS:  result.ms,
			CheckedAt:  checkedAt,
		})
	}
	if err := s.recorder.RecordDependencyHealth(ctx, checks); err != nil {
		return err
	}

	s.pruneMu.Lock()
	prune := s.lastPrune.IsZero() || time.Since(s.lastPrune) >= 24*time.Hour
	s.pruneMu.Unlock()
	if prune {
		if err := s.recorder.PruneDependencyHealth(ctx, checkedAt.Add(-HistoryRetention)); err != nil {
			return err
		}
		s.pruneMu.Lock()
		s.lastPrune = checkedAt
		s.pruneMu.Unlock()
	}
	return nil
}

func (s *Sampler) run(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.SampleNow(checkCtx); err != nil {
		slog.Warn("dependency health snapshot could not be persisted")
	}
}

func dependencyTimeout(name string) time.Duration {
	switch name {
	case "postgres":
		return 500 * time.Millisecond
	case "redis":
		return 200 * time.Millisecond
	case "horizon":
		return time.Second
	default:
		return 200 * time.Millisecond
	}
}
