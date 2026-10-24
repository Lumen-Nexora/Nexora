package compliance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog/log"
)

// Worker refreshes the OFAC SDN list. It is registered against the periodic
// "compliance:sanctions_refresh" task, which asynq's scheduler enqueues once
// a day on the low-priority queue.
type Worker struct {
	repo         Repository
	source       SDNSource
	set          *SanctionsSet
	webhooks     Dispatcher
	extraSources []sanctionsFeed
}
type sanctionsFeed struct {
	name   string
	source SDNSource
	parse  func(io.Reader) ([]*domain.SanctionsEntity, error)
}

func NewWorker(repo Repository, source SDNSource, set *SanctionsSet, webhooks Dispatcher) *Worker {
	return &Worker{repo: repo, source: source, set: set, webhooks: webhooks}
}

// WithOfficialSources adds the UN Security Council and EU Commission FSF
// 1.1 exports to the same daily worker. Failed sources retain their previous
// database snapshot while successful source refreshes remain independent.
func (w *Worker) WithOfficialSources(unURL, euURL string) *Worker {
	w.extraSources = []sanctionsFeed{
		{SourceUN, NewHTTPSDNSource(unURL, nil), ParseUN},
		{SourceEU, NewHTTPSDNSource(euURL, nil), ParseEU},
	}
	return w
}

// HandleRefreshSanctions downloads, parses and persists the SDN list, then
// reloads this process's in-memory set.
//
// A failed refresh leaves the previous list in place: screening against a
// slightly stale list is far better than screening against an empty one,
// which would clear every transfer.
func (w *Worker) HandleRefreshSanctions(ctx context.Context, _ *asynq.Task) error {
	feeds := []sanctionsFeed{{SourceOFAC, w.source, ParseSDN}}
	feeds = append(feeds, w.extraSources...)
	var problems []error
	for _, feed := range feeds {
		started := time.Now().UTC()
		count, err := w.refreshSource(ctx, feed)
		finished := time.Now().UTC()
		update := &domain.SanctionsUpdate{
			ID: uuid.New().String(), Source: feed.name, EntityCount: count,
			StartedAt: started, FinishedAt: finished, DurationMS: finished.Sub(started).Milliseconds(),
		}
		if err != nil {
			update.Status = domain.SanctionsUpdateFailed
			update.Error = err.Error()
			problems = append(problems, fmt.Errorf("%s: %w", feed.name, err))
			log.Error().Err(err).Str("source", feed.name).Msg("compliance: source refresh failed")
			if w.webhooks != nil {
				if hookErr := w.webhooks.Dispatch(ctx, domain.EventSanctionsRefreshFailed, map[string]interface{}{
					"source": feed.name, "error": err.Error(), "failed_at": finished.Format(time.RFC3339),
				}); hookErr != nil {
					log.Error().Err(hookErr).Msg("compliance: source failure webhook failed")
				}
			}
		} else {
			update.Status = domain.SanctionsUpdateSuccess
			log.Info().Str("source", feed.name).Int("entity_count", count).Msg("compliance: source refresh complete")
		}
		if err := w.repo.RecordSanctionsUpdate(ctx, update); err != nil {
			log.Error().Err(err).Str("source", feed.name).Msg("compliance: failed to record sanctions update")
			problems = append(problems, fmt.Errorf("%s audit: %w", feed.name, err))
		}
	}
	// Always reload the complete snapshot from persistence. Updating the
	// in-memory set with the last downloaded source would silently discard
	// every other source, even if earlier updates succeeded.
	if w.set != nil {
		if err := w.set.LoadFromRepository(ctx, w.repo); err != nil {
			problems = append(problems, fmt.Errorf("reload combined sanctions set: %w", err))
		}
	}
	return errors.Join(problems...)

}

func (w *Worker) refresh(ctx context.Context) (int, error) {
	return w.refreshSource(ctx, sanctionsFeed{SourceOFAC, w.source, ParseSDN})
}

func (w *Worker) refreshSource(ctx context.Context, feed sanctionsFeed) (int, error) {
	if feed.source == nil || feed.parse == nil {
		return 0, fmt.Errorf("missing %s feed", feed.name)
	}
	body, err := feed.source.Fetch(ctx)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	entities, err := feed.parse(body)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", feed.name, err)
	}
	if len(entities) == 0 {
		return 0, fmt.Errorf("%s parsed zero entities; refusing replacement", feed.name)
	}
	// Persist only this source's complete fresh snapshot atomically.
	written, err := w.repo.ReplaceSanctionsEntities(ctx, entities, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("persist %s: %w", feed.name, err)
	}
	return written, nil
}
