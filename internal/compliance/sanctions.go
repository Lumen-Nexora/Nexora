package compliance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/rs/zerolog/log"
)

// ErrSanctionsSetNotLoaded is returned by SanctionsScreener before the first
// successful load. CompositeScreener turns it into a hold, which is the
// intended startup behaviour: an API that boots before the SDN set is
// available reviews every transfer rather than clearing them.
var ErrSanctionsSetNotLoaded = errors.New("sanctions set has not been loaded")

// DefaultFuzzyThreshold is the maximum edit distance at which a destination
// federation name is treated as a probable sanctions match. Exact address
// matches block; name matches only ever hold, because names collide.
const DefaultFuzzyThreshold = 2

type nameEntry struct {
	normalized string
	display    string
}

// SanctionsSet is the in-memory index screened against on the request path.
// It is rebuilt wholesale on refresh rather than mutated in place — the same
// approach as anchor.Registry — so a reader never observes a half-applied
// list.
type SanctionsSet struct {
	mu              sync.RWMutex
	byAddress       map[string]string
	names           []nameEntry
	fuzzy           *FuzzySanctionsIndex
	entityCount     int
	updatedAt       time.Time
	loaded          bool
	requiredSources []string
}

func NewSanctionsSet() *SanctionsSet {
	return &SanctionsSet{byAddress: make(map[string]string)}
}

// RequireSources configures the publisher lists needed before screening may
// ever return clear. Configure this before the initial LoadFromRepository.
// A source that has never loaded is distinct from a temporarily stale source:
// the latter retains its last successful snapshot on refresh failure.
func (s *SanctionsSet) RequireSources(sources ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requiredSources = append([]string(nil), sources...)
	if s.loaded {
		// If configured after initial load, require a fresh load. Never
		// silently re-mark a partially populated set as ready.
		s.loaded = false
	}
}

// Replace swaps in a freshly parsed list.
//
// An empty list is ignored rather than applied. The real SDN list always has
// thousands of entries, so "empty" means either a fresh database before the
// first refresh or a malformed download — and applying it would both wipe a
// good set and mark the screener loaded, turning every subsequent screen into
// a silent pass. Ignoring it keeps the set unloaded (or keeps the previous
// one), which is what makes the screener fail closed.
func (s *SanctionsSet) Replace(entities []*domain.SanctionsEntity, updatedAt time.Time) {
	if len(entities) == 0 {
		return
	}

	byAddress := make(map[string]string, len(entities))
	names := make([]nameEntry, 0, len(entities))
	seenName := make(map[string]struct{}, len(entities))

	for _, e := range entities {
		if e == nil {
			continue
		}
		if addr := strings.TrimSpace(e.Address); addr != "" {
			byAddress[normalizeAddress(addr)] = e.Name
		}
		if n := normalizeName(e.Name); n != "" {
			if _, ok := seenName[n]; !ok {
				seenName[n] = struct{}{}
				names = append(names, nameEntry{normalized: n, display: e.Name})
			}
		}
	}

	// Build a separate immutable fuzzy index before publishing a refreshed set.
	fuzzy := NewFuzzySanctionsIndex(entities)
	sourcePresent := make(map[string]struct{})
	for _, e := range entities {
		if e != nil && normalizeName(e.Name) != "" {
			sourcePresent[e.Source] = struct{}{}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byAddress = byAddress
	s.names = names
	s.fuzzy = fuzzy
	s.entityCount = len(entities)
	s.updatedAt = updatedAt
	ready := len(names) > 0
	for _, src := range s.requiredSources {
		if _, ok := sourcePresent[src]; !ok {
			ready = false
			break
		}
	}
	s.loaded = ready
}

func (s *SanctionsSet) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

// Stats reports what the set currently holds, for the sanctions-status endpoint.
func (s *SanctionsSet) Stats() (entityCount, addressCount, nameCount int, updatedAt time.Time, loaded bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entityCount, len(s.byAddress), len(s.names), s.updatedAt, s.loaded
}

// MatchAddress reports an exact (case- and whitespace-normalized) address hit.
func (s *SanctionsSet) MatchAddress(addr string) (string, bool) {
	if addr == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	name, ok := s.byAddress[normalizeAddress(addr)]
	return name, ok
}

// MatchName returns the closest sanctioned name within threshold edits.
func (s *SanctionsSet) MatchName(name string, threshold int) (string, int, bool) {
	n := normalizeName(name)
	if n == "" {
		return "", 0, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	best := threshold + 1
	bestName := ""
	for _, e := range s.names {
		d := levenshteinAtMost(n, e.normalized, threshold)
		if d < best {
			best, bestName = d, e.display
			if best == 0 {
				break
			}
		}
	}
	if bestName == "" {
		return "", 0, false
	}
	return bestName, best, true
}

// MatchDetailedName uses the immutable indexed scorer and returns the
// original sanction-list ID and source for auditability.
func (s *SanctionsSet) MatchDetailedName(name string, threshold float64) (FuzzySanctionsMatch, bool) {
	s.mu.RLock()
	index := s.fuzzy
	s.mu.RUnlock()
	return index.Match(name, threshold)
}

// LoadFromRepository rebuilds the set from the sanctions_entities table. Every
// process calls this on startup and on a ticker, which is how a refresh
// performed by cmd/worker reaches cmd/api's memory.
func (s *SanctionsSet) LoadFromRepository(ctx context.Context, repo SanctionsReader) error {
	entities, err := repo.ListSanctionsEntities(ctx)
	if err != nil {
		return err
	}
	updatedAt := time.Now().UTC()
	if len(entities) > 0 && !entities[0].RefreshedAt.IsZero() {
		updatedAt = entities[0].RefreshedAt
	}
	s.Replace(entities, updatedAt)
	return nil
}

// StartReloader reloads the set from the database on every tick until ctx is
// canceled. A failed reload keeps the previous set rather than emptying it —
// dropping to an empty set would silently turn every screen into a pass.
func (s *SanctionsSet) StartReloader(ctx context.Context, repo SanctionsReader, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.LoadFromRepository(ctx, repo); err != nil {
					log.Error().Err(err).Msg("compliance: sanctions set reload failed, keeping previous set")
				}
			}
		}
	}()
}

// SanctionsScreener matches the destination against the OFAC SDN set.
type SanctionsScreener struct {
	set                 *SanctionsSet
	fuzzyThreshold      int
	similarityThreshold float64
}

func NewSanctionsScreener(set *SanctionsSet, fuzzyThreshold int) *SanctionsScreener {
	if fuzzyThreshold <= 0 {
		fuzzyThreshold = DefaultFuzzyThreshold
	}
	return &SanctionsScreener{set: set, fuzzyThreshold: fuzzyThreshold, similarityThreshold: DefaultSimilarityThreshold}
}

// NewSanctionsScreenerWithSimilarity configures a [0,1] Jaro-Winkler/
// Double Metaphone score threshold without changing the legacy constructor.
func NewSanctionsScreenerWithSimilarity(set *SanctionsSet, threshold float64) *SanctionsScreener {
	s := NewSanctionsScreener(set, DefaultFuzzyThreshold)
	if threshold > 0 && threshold <= 1 {
		s.similarityThreshold = threshold
	}
	return s
}

func (s *SanctionsScreener) Name() string { return "sanctions" }

func (s *SanctionsScreener) Screen(_ context.Context, req domain.ScreeningRequest) (domain.ScreeningDecision, error) {
	if s.set == nil || !s.set.Loaded() {
		return domain.ScreeningDecision{}, ErrSanctionsSetNotLoaded
	}

	// An exact address hit is unambiguous — the counterparty *is* the
	// designated party — so it blocks outright rather than queuing a review.
	if entity, ok := s.set.MatchAddress(req.ToPublicKey); ok {
		return domain.ScreeningDecision{
			Status:        domain.ScreeningBlocked,
			RulesFired:    []string{"sanctions_address_match"},
			Reason:        "destination address appears on the OFAC SDN list",
			MatchedEntity: entity,
			RiskScore:     100,
		}, nil
	}

	// Indexed Jaro-Winkler + Double Metaphone handles primary names, aliases,
	// corporate entities and both sides of a transfer. A name can only hold
	// for human review; never block automatically based on name similarity.
	for _, value := range []string{req.ToFederation, req.ToName, req.FromName} {
		if match, ok := s.set.MatchDetailedName(value, s.similarityThreshold); ok {
			rule := "sanctions_name_fuzzy_match"
			if normalizeName(value) == normalizeName(match.EntityName) {
				rule = "sanctions_name_match"
			}
			return domain.ScreeningDecision{
				Status:     domain.ScreeningHold,
				RulesFired: []string{rule},
				Reason: fmt.Sprintf("possible sanctions-list name match; name=%s, source=%s, entity_id=%s, score=%.4f, jaro_winkler=%.4f, phonetic=%t; manual review required",
					match.EntityName, match.Source, match.EntityID, match.Score, match.JaroWinkler, match.PhoneticMatch),
				MatchedEntity:   match.EntityName,
				MatchedEntityID: match.EntityID,
				MatchedSource:   match.Source,
				MatchScore:      match.Score,
				RiskScore:       int(match.Score * 80),
			}, nil
		}
	}

	return domain.ScreeningDecision{Status: domain.ScreeningClear}, nil
}

// normalizeAddress upper-cases and trims a Stellar address. Stellar strkeys
// are base32 and case-significant in practice, but SDN data is inconsistently
// cased, so both sides are folded.
func normalizeAddress(a string) string {
	return strings.ToUpper(strings.TrimSpace(a))
}

// normalizeName lower-cases, collapses internal whitespace, and drops
// punctuation so "Al-Zawahiri, Ayman" and "al zawahiri ayman" compare equal
// before the edit-distance check ever runs.
func normalizeName(n string) string {
	var b strings.Builder
	b.Grow(len(n))
	lastSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(n)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSpace = false
		case r > 127:
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
