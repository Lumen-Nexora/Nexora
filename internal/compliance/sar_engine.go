package compliance

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrUnauthorized = errors.New("compliance: caller is not authorized to access SAR records")
	ErrNotFound     = errors.New("compliance: SAR case not found")
)

// Role identifies the authorization level of a caller.
type Role string

const (
	RoleComplianceOfficer Role = "COMPLIANCE_OFFICER"
	RoleSystem            Role = "SYSTEM"
	RoleOther             Role = "OTHER"
)

// Principal is the identity performing an operation.
type Principal struct {
	ID   string
	Role Role
}

// CustomerProfile carries KYC context included in SAR drafts.
type CustomerProfile struct {
	CustomerID  string
	FullName    string
	Country     string
	RiskRating  string
	OpenedAt    time.Time
}

// ProfileProvider supplies customer profiles.
type ProfileProvider interface {
	Profile(customerID string) (CustomerProfile, error)
}

// TransactionSource supplies transaction streams.
type TransactionSource interface {
	Transactions(customerID string, since time.Time) ([]Transaction, error)
}

// CaseStatus is the lifecycle state of a SAR case.
type CaseStatus string

const (
	CaseDraft     CaseStatus = "DRAFT"
	CaseUnderReview CaseStatus = "UNDER_REVIEW"
	CaseFiled     CaseStatus = "FILED"
	CaseDismissed CaseStatus = "DISMISSED"
)

// TimelineEntry is a single event in the SAR transaction timeline.
type TimelineEntry struct {
	Timestamp   time.Time
	Transaction Transaction
}

// SARCase is a standardized suspicious activity report draft.
type SARCase struct {
	ID           string
	CustomerID   string
	Profile      CustomerProfile
	Typologies   []Typology
	Findings     []Finding
	Timeline     []TimelineEntry
	Narrative    string
	Score        float64
	Status       CaseStatus
	CreatedAt    time.Time
	AccessRoles  []Role
}

// Engine evaluates transactions against rules and drafts SAR cases.
type Engine struct {
	rules    []Rule
	profiles ProfileProvider
	txns     TransactionSource
	lookback time.Duration
	minScore float64

	mu    sync.RWMutex
	cases map[string]*SARCase
	seq   int
	now   func() time.Time
}

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// WithLookback sets how far back transaction history is evaluated.
func WithLookback(d time.Duration) EngineOption {
	return func(e *Engine) { e.lookback = d }
}

// WithMinScore sets the minimum aggregate score required to draft a SAR.
func WithMinScore(s float64) EngineOption {
	return func(e *Engine) { e.minScore = s }
}

// WithClock overrides the time source (for tests).
func WithClock(now func() time.Time) EngineOption {
	return func(e *Engine) { e.now = now }
}

// NewEngine constructs a SAR engine.
func NewEngine(rules []Rule, profiles ProfileProvider, txns TransactionSource, opts ...EngineOption) (*Engine, error) {
	if len(rules) == 0 {
		return nil, errors.New("compliance: at least one rule is required")
	}
	if profiles == nil || txns == nil {
		return nil, errors.New("compliance: profile and transaction sources are required")
	}
	e := &Engine{
		rules:    rules,
		profiles: profiles,
		txns:     txns,
		lookback: 90 * 24 * time.Hour,
		minScore: 0.5,
		cases:    make(map[string]*SARCase),
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// Evaluate runs all rules over a customer's recent history. If the aggregate
// score meets the threshold, a DRAFT SAR case is created and returned.
// Returns (nil, nil) when no case is warranted.
func (e *Engine) Evaluate(customerID string) (*SARCase, error) {
	now := e.now()
	txns, err := e.txns.Transactions(customerID, now.Add(-e.lookback))
	if err != nil {
		return nil, fmt.Errorf("compliance: fetch transactions: %w", err)
	}
	sort.SliceStable(txns, func(i, j int) bool { return txns[i].Timestamp.Before(txns[j].Timestamp) })

	var findings []Finding
	typologies := make(map[Typology]struct{})
	for _, r := range e.rules {
		fs := r.Evaluate(customerID, txns)
		for _, f := range fs {
			findings = append(findings, f)
			typologies[f.Typology] = struct{}{}
		}
	}
	if len(findings) == 0 {
		return nil, nil
	}

	var score float64
	for _, f := range findings {
		if f.Score > score {
			score = f.Score
		}
	}
	if score < e.minScore {
		return nil, nil
	}

	profile, err := e.profiles.Profile(customerID)
	if err != nil {
		return nil, fmt.Errorf("compliance: fetch profile: %w", err)
	}

	sc := &SARCase{
		CustomerID:  customerID,
		Profile:     profile,
		Findings:    findings,
		Timeline:    buildTimeline(findings),
		Score:       score,
		Status:      CaseDraft,
		CreatedAt:   now,
		AccessRoles: []Role{RoleComplianceOfficer},
	}
	for t := range typologies {
		sc.Typologies = append(sc.Typologies, t)
	}
	sort.Slice(sc.Typologies, func(i, j int) bool { return sc.Typologies[i] < sc.Typologies[j] })
	sc.Narrative = draftNarrative(sc)

	e.mu.Lock()
	e.seq++
	sc.ID = fmt.Sprintf("SAR-%06d", e.seq)
	e.cases[sc.ID] = sc
	e.mu.Unlock()

	return cloneCase(sc), nil
}

// GetCase returns a SAR case if the principal is authorized to view it.
func (e *Engine) GetCase(p Principal, id string) (*SARCase, error) {
	if !canAccess(p) {
		return nil, ErrUnauthorized
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	sc, ok := e.cases[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneCase(sc), nil
}

// ListCases returns all SAR cases visible to the principal.
func (e *Engine) ListCases(p Principal) ([]*SARCase, error) {
	if !canAccess(p) {
		return nil, ErrUnauthorized
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*SARCase, 0, len(e.cases))
	for _, sc := range e.cases {
		out = append(out, cloneCase(sc))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// UpdateStatus transitions a case. Only compliance officers may do this.
func (e *Engine) UpdateStatus(p Principal, id string, status CaseStatus) error {
	if p.Role != RoleComplianceOfficer {
		return ErrUnauthorized
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sc, ok := e.cases[id]
	if !ok {
		return ErrNotFound
	}
	sc.Status = status
	return nil
}

func canAccess(p Principal) bool {
	return p.Role == RoleComplianceOfficer
}

func buildTimeline(findings []Finding) []TimelineEntry {
	seen := make(map[string]struct{})
	var tl []TimelineEntry
	for _, f := range findings {
		for _, t := range f.Transactions {
			if _, ok := seen[t.ID]; ok {
				continue
			}
			seen[t.ID] = struct{}{}
			tl = append(tl, TimelineEntry{Timestamp: t.Timestamp, Transaction: t})
		}
	}
	sort.Slice(tl, func(i, j int) bool { return tl[i].Timestamp.Before(tl[j].Timestamp) })
	return tl
}

func draftNarrative(sc *SARCase) string {
	narr := fmt.Sprintf("Customer %s (%s, %s) exhibits activity consistent with: ",
		sc.CustomerID, sc.Profile.FullName, sc.Profile.Country)
	for i, t := range sc.Typologies {
		if i > 0 {
			narr += ", "
		}
		narr += string(t)
	}
	narr += ". Findings: "
	for i, f := range sc.Findings {
		if i > 0 {
			narr += "; "
		}
		narr += f.Reason
	}
	narr += fmt.Sprintf(". Aggregate score %.2f. Timeline contains %d transactions.",
		sc.Score, len(sc.Timeline))
	return narr
}

func cloneCase(sc *SARCase) *SARCase {
	c := *sc
	c.Typologies = append([]Typology(nil), sc.Typologies...)
	c.Findings = append([]Finding(nil), sc.Findings...)
	c.Timeline = append([]TimelineEntry(nil), sc.Timeline...)
	c.AccessRoles = append([]Role(nil), sc.AccessRoles...)
	return &c
}
