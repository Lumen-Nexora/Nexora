package compliance

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

func TestJaroWinklerReferenceVectors(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want float64
	}{
		{"MARTHA", "MARHTA", 0.9611111111111111},
		{"DWAYNE", "DUANE", 0.84},
		{"", "DAVID", 0},
		{"DAVID", "DAVID", 1},
	} {
		got := jaroWinkler(tc.a, tc.b)
		if math.Abs(got-tc.want) > 0.0000001 {
			t.Errorf("JaroWinkler(%q,%q)=%f; want %f", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFuzzyIndexSourceAliasAndCorporate(t *testing.T) {
	idx := NewFuzzySanctionsIndex([]*domain.SanctionsEntity{
		{UID: "uid-ofac", Source: SourceOFAC, Name: "Ayman Zawahiri"},
		{UID: "uid-ofac", Source: SourceOFAC, Name: "Aiman Zawahri"},
		{UID: "uid-un", Source: "un", Name: "NORTHERN SHIPPING LIMITED", EntityType: "Entity"},
		{UID: "uid-eu", Source: "eu", Name: "EXAMPLE CONSORTIUM", EntityType: "Entity"},
		nil,
	})
	for _, tc := range []struct {
		query, uid, source string
	}{
		{"Ayman Zawahiri", "uid-ofac", SourceOFAC},
		{"Ayman Zawahirn", "uid-ofac", SourceOFAC},
		{"Aiman Zawahri", "uid-ofac", SourceOFAC},
		{"NORTHERN SHIPPING LIMITED", "uid-un", "un"},
		{"EXAMPLE CONSORTIUM", "uid-eu", "eu"},
	} {
		got, ok := idx.Match(tc.query, 0.85)
		if !ok {
			t.Errorf("no match for %q", tc.query)
			continue
		}
		if got.EntityID != tc.uid || got.Source != tc.source {
			t.Errorf("%q: got UID/source %q/%q; want %q/%q", tc.query, got.EntityID, got.Source, tc.uid, tc.source)
		}
		if got.Score < 0.85 || got.Score > 1.00000001 {
			t.Errorf("invalid score for %q: %f", tc.query, got.Score)
		}
	}
}

func TestFuzzyIndexAvoidsPrefixMatchesAndWeakNames(t *testing.T) {
	idx := NewFuzzySanctionsIndex([]*domain.SanctionsEntity{
		{UID: "1001", Source: SourceOFAC, Name: "Ayman Zawahiri"},
	})
	for _, n := range []string{"Ayman Zawa", "Completely Different Counterparty", "", "Ayman"} {
		if m, ok := idx.Match(n, 0.85); ok {
			t.Errorf("should not flag unrelated/fragment name %q: %#v", n, m)
		}
	}
}

func TestIndexedScreenerMatchesSenderAndRecipient(t *testing.T) {
	set := NewSanctionsSet()
	set.Replace([]*domain.SanctionsEntity{
		{UID: "un-123", Source: "un", Name: "NORTHERN SHIPPING LIMITED", EntityType: "Entity"},
	}, time.Now())
	screener := NewSanctionsScreener(set, DefaultFuzzyThreshold)
	for _, req := range []domain.ScreeningRequest{
		{FromName: "NORTHERN SHIPPING LIMITED"},
		{ToName: "NORTHERN SHIPPING LIMITED"},
	} {
		got, err := screener.Screen(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.ScreeningHold || got.MatchedEntityID != "un-123" || got.MatchedSource != "un" || got.MatchScore < .85 {
			t.Fatalf("missing provenance on review hold: %#v", got)
		}
	}
}

func TestIndexedFederationNameIncludesProvenance(t *testing.T) {
	set := NewSanctionsSet()
	set.Replace([]*domain.SanctionsEntity{
		{UID: "un-999", Source: "un", Name: "NORTHERN SHIPPING LIMITED"},
	}, time.Now())
	d, err := NewSanctionsScreener(set, DefaultFuzzyThreshold).Screen(
		context.Background(), domain.ScreeningRequest{ToFederation: "NORTHERN SHIPPING LIMITED"},
	)
	if err != nil || d.Status != domain.ScreeningHold || d.MatchedEntityID != "un-999" ||
		d.MatchedSource != "un" || d.MatchScore != 1 ||
		len(d.RulesFired) != 1 || d.RulesFired[0] != "sanctions_name_match" {
		t.Fatalf("unexpected federation provenance: %+v, err=%v", d, err)
	}
}

func TestFuzzyIndexDoesNotReplaceGoodSnapshotOnEmptyReload(t *testing.T) {
	set := NewSanctionsSet()
	set.Replace([]*domain.SanctionsEntity{
		{UID: "ofac-42", Source: SourceOFAC, Name: "Ayman Zawahiri"},
	}, time.Now())
	set.Replace(nil, time.Now())
	match, ok := set.MatchDetailedName("Ayman Zawahiri", 0.85)
	if !ok || match.EntityID != "ofac-42" {
		t.Errorf("empty refresh erased good index: %#v %v", match, ok)
	}
}

// Benchmark includes 30K unrelated sanctions entries to prove lookup cost
// is driven by the indexed candidate set, not by the full sanctions history.
func BenchmarkFuzzySanctionsScreening(b *testing.B) {
	entities := make([]*domain.SanctionsEntity, 0, 30001)
	entities = append(entities, &domain.SanctionsEntity{UID: "matched", Source: SourceOFAC, Name: "Ayman Zawahiri"})
	for i := 0; i < 30000; i++ {
		entities = append(entities, &domain.SanctionsEntity{
			UID:    fmt.Sprintf("uid-%05d", i),
			Name:   fmt.Sprintf("CORPORATE COUNTERPARTY %05d SERVICE", i),
			Source: SourceOFAC,
		})
	}
	idx := NewFuzzySanctionsIndex(entities)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := idx.Match("Ayman Zawahirn", 0.85); !ok {
			b.Fatal("fuzzy name not found")
		}
	}
}

// Benchmark the complete sanctions screener, not only the fuzzy lookup.
func BenchmarkSanctionsScreenerIndexedName(b *testing.B) {
	entities := make([]*domain.SanctionsEntity, 0, 30001)
	entities = append(entities, &domain.SanctionsEntity{UID: "matched", Source: SourceOFAC, Name: "Ayman Zawahiri"})
	for i := 0; i < 30000; i++ {
		entities = append(entities, &domain.SanctionsEntity{
			UID:    fmt.Sprintf("uid-%05d", i),
			Name:   fmt.Sprintf("CORPORATE COUNTERPARTY %05d SERVICE", i),
			Source: SourceOFAC,
		})
	}
	set := NewSanctionsSet()
	set.Replace(entities, time.Now())
	screener := NewSanctionsScreener(set, DefaultFuzzyThreshold)
	req := domain.ScreeningRequest{ToFederation: "Ayman Zawahirn"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := screener.Screen(ctx, req)
		if err != nil || result.Status != domain.ScreeningHold {
			b.Fatalf("unmatched: %+v %v", result, err)
		}
	}
}

// Benchmark a realistic no-hit, for which the old Levenshtein fallback
// examined every sanctioned name and grew linearly with list size.
func BenchmarkSanctionsScreenerNegative(b *testing.B) {
	entries := make([]*domain.SanctionsEntity, 30000)
	for i := range entries {
		entries[i] = &domain.SanctionsEntity{UID: fmt.Sprintf("uid-%05d", i),
			Source: SourceOFAC, Name: fmt.Sprintf("CORPORATE COUNTERPARTY %05d SERVICE", i)}
	}
	set := NewSanctionsSet()
	set.Replace(entries, time.Now())
	s := NewSanctionsScreenerWithSimilarity(set, .85)
	req := domain.ScreeningRequest{ToFederation: "EXTERNAL UNAFFILIATED PAYEE"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decision, err := s.Screen(context.Background(), req)
		if err != nil || decision.Status != domain.ScreeningClear {
			b.Fatalf("expected clear: %+v, %v", decision, err)
		}
	}
}

// Shared corporate terms exercise a busy phonetic posting list.
func BenchmarkSanctionsScreenerBusyBucket(b *testing.B) {
	entries := make([]*domain.SanctionsEntity, 30000)
	for i := range entries {
		entries[i] = &domain.SanctionsEntity{UID: fmt.Sprintf("uid-%05d", i),
			Source: SourceOFAC, Name: fmt.Sprintf("CORPORATE COUNTERPARTY %05d SERVICE", i)}
	}
	set := NewSanctionsSet()
	set.Replace(entries, time.Now())
	s := NewSanctionsScreenerWithSimilarity(set, .85)
	req := domain.ScreeningRequest{ToFederation: "CORPORATE COUNTERPARTY 99999 SERVICE"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decision, err := s.Screen(context.Background(), req)
		if err != nil || decision.Status != domain.ScreeningHold {
			b.Fatalf("expected hold for a single changed digit: %+v, %v", decision, err)
		}
	}
}
