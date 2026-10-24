package compliance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

const unFixture = `<?xml version="1.0"?>
<CONSOLIDATED_LIST><INDIVIDUALS><INDIVIDUAL>
 <DATAID>100</DATAID><REFERENCE_NUMBER>QDi.100</REFERENCE_NUMBER>
 <FIRST_NAME>MOHAMMED</FIRST_NAME><SECOND_NAME>AHMED</SECOND_NAME><THIRD_NAME>na</THIRD_NAME>
 <INDIVIDUAL_ALIAS><ALIAS_NAME>Mohamad Ahmad</ALIAS_NAME></INDIVIDUAL_ALIAS>
 </INDIVIDUAL></INDIVIDUALS>
 <ENTITIES><ENTITY><DATAID>250</DATAID><REFERENCE_NUMBER>QDe.250</REFERENCE_NUMBER>
 <FIRST_NAME>NORTHERN SHIPPING LIMITED</FIRST_NAME>
 <ENTITY_ALIAS><ALIAS_NAME>Northern Sea Lines</ALIAS_NAME></ENTITY_ALIAS>
 </ENTITY></ENTITIES></CONSOLIDATED_LIST>`
const euFixture = `<?xml version="1.0"?>
<export xmlns="http://europa.eu/fsd/export" generationDate="2026-10-22">
 <sanctionEntity logicalId="131" euReferenceNumber="EU.131">
  <subjectType code="person" />
  <nameAlias wholeName="ALEXANDRA EXAMPLE" strong="true"/>
  <nameAlias wholeName="Sasha Example" strong="false"/>
 </sanctionEntity>
 <sanctionEntity logicalId="132" euReferenceNumber="EU.132">
  <subjectType code="enterprise" />
  <nameAlias wholeName="EASTERN HOLDINGS LTD" />
 </sanctionEntity>
</export>`

func TestOfficialUNAndEUParsers(t *testing.T) {
	for _, tc := range []struct {
		source   string
		parse    func(io.Reader) ([]*domain.SanctionsEntity, error)
		data     string
		expected map[string]string
	}{
		{SourceUN, ParseUN, unFixture, map[string]string{
			"MOHAMMED AHMED":            "un_consolidated:QDi.100",
			"Mohamad Ahmad":             "un_consolidated:QDi.100",
			"NORTHERN SHIPPING LIMITED": "un_consolidated:QDe.250",
			"Northern Sea Lines":        "un_consolidated:QDe.250",
		}},
		{SourceEU, ParseEU, euFixture, map[string]string{
			"ALEXANDRA EXAMPLE":    "eu_fsf:131",
			"Sasha Example":        "eu_fsf:131",
			"EASTERN HOLDINGS LTD": "eu_fsf:132",
		}},
	} {
		t.Run(tc.source, func(t *testing.T) {
			entries, err := tc.parse(strings.NewReader(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(tc.expected) {
				t.Fatalf("got %d entries, want %d", len(entries), len(tc.expected))
			}
			for _, entry := range entries {
				if entry.Source != tc.source || entry.UID != tc.expected[entry.Name] {
					t.Fatalf("wrong source/uid for %s: %+v", entry.Name, entry)
				}
				if entry.RefreshedAt.IsZero() {
					t.Fatal("missing refresh date")
				}
			}
			idx := NewFuzzySanctionsIndex(entries)
			for name, uid := range tc.expected {
				hit, ok := idx.Match(name, .85)
				if !ok || hit.EntityID != uid || hit.Source != tc.source {
					t.Errorf("no provenance for %q: %+v, ok=%v", name, hit, ok)
				}
			}
		})
	}
}

func TestOfficialParsersFailClosed(t *testing.T) {
	for _, tc := range []struct {
		source string
		parse  func(io.Reader) ([]*domain.SanctionsEntity, error)
		inputs []string
	}{
		{SourceUN, ParseUN, []string{"<html><p>login</p></html>", "<CONSOLIDATED_LIST/>", "<CONSOLIDATED_LIST><INDIVIDUAL>"}},
		{SourceEU, ParseEU, []string{"<html><p>login</p></html>", "<export/>", "<export><sanctionEntity logicalId=\"42\"/>", "<export><sanctionEntity"}},
	} {
		for _, xmlInput := range tc.inputs {
			if _, err := tc.parse(strings.NewReader(xmlInput)); err == nil {
				t.Errorf("%s accepted invalid/empty XML: %q", tc.source, xmlInput)
			}
		}
	}
}

func TestWorkerRefreshesAllSourcesWithoutLosingExistingList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ofac":
			_, _ = w.Write([]byte(loadFixture(t)))
		case "/un":
			_, _ = w.Write([]byte(unFixture))
		case "/eu":
			_, _ = w.Write([]byte(euFixture))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	repo := newComplianceMockRepo()
	set := NewSanctionsSet()
	worker := NewWorker(repo, NewHTTPSDNSource(server.URL+"/ofac", server.Client()), set, nil).
		WithOfficialSources(server.URL+"/un", server.URL+"/eu")
	if err := worker.HandleRefreshSanctions(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(repo.updates) != 3 {
		t.Fatalf("expected three audited sources: %d", len(repo.updates))
	}
	if count, _, _, _, loaded := set.Stats(); !loaded || count < 7 {
		t.Fatalf("missing combined sources: %d", count)
	}
	for _, tc := range []struct{ name, source string }{
		{"Ayman Zawahiri", SourceOFAC},
		{"Mohamad Ahmad", SourceUN},
		{"Sasha Example", SourceEU},
	} {
		match, ok := set.MatchDetailedName(tc.name, .85)
		if !ok || match.Source != tc.source {
			t.Fatalf("%q not combined in memory: %#v %v", tc.name, match, ok)
		}
	}
}

func TestMultiSourceFailedRefreshRetainsPreviousSnapshot(t *testing.T) {
	failUN := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ofac":
			_, _ = w.Write([]byte(loadFixture(t)))
		case "/un":
			if failUN {
				w.WriteHeader(http.StatusBadGateway)
			} else {
				_, _ = w.Write([]byte(unFixture))
			}
		case "/eu":
			_, _ = w.Write([]byte(euFixture))
		}
	}))
	defer server.Close()
	repo := newComplianceMockRepo()
	set := NewSanctionsSet()
	worker := NewWorker(repo, NewHTTPSDNSource(server.URL+"/ofac", server.Client()), set, nil).
		WithOfficialSources(server.URL+"/un", server.URL+"/eu")
	if err := worker.HandleRefreshSanctions(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	failUN = true
	err := worker.HandleRefreshSanctions(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), SourceUN) {
		t.Fatalf("expected source-specific error: %v", err)
	}
	if len(repo.updates) != 6 {
		t.Fatalf("expected audit result per source: %d", len(repo.updates))
	}
	if repo.updates[4].Status != domain.SanctionsUpdateFailed {
		t.Fatal("no UN failure audit")
	}
	match, ok := set.MatchDetailedName("Mohamad Ahmad", .85)
	if !ok || match.Source != SourceUN {
		t.Fatalf("previous UN snapshot lost: %+v", match)
	}
	if _, ok := set.MatchDetailedName("Sasha Example", .85); !ok {
		t.Fatal("EU disappeared after UN failure")
	}
}

func TestSimilarityThresholdIsConfigurable(t *testing.T) {
	set := NewSanctionsSet()
	set.Replace([]*domain.SanctionsEntity{{UID: "one", Source: SourceUN, Name: "MARTHA"}}, time.Now())
	// Defaults remain consistent for callers using the legacy API.
	low := NewSanctionsScreenerWithSimilarity(set, 0.8)
	high := NewSanctionsScreenerWithSimilarity(set, 0.99)
	req := domain.ScreeningRequest{ToFederation: "MARHTA"}
	l, err := low.Screen(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	h, err := high.Screen(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != domain.ScreeningHold || h.Status != domain.ScreeningClear {
		t.Fatalf("threshold not honored: low=%s high=%s", l.Status, h.Status)
	}
}

func TestRequiredPublisherCoveragePreventsPartialClear(t *testing.T) {
	set := NewSanctionsSet()
	set.RequireSources(SourceOFAC, SourceUN, SourceEU)
	ofac := &domain.SanctionsEntity{UID: "100", Source: SourceOFAC, Name: "OFAC PERSON"}
	un := &domain.SanctionsEntity{UID: "200", Source: SourceUN, Name: "UN PERSON"}
	eu := &domain.SanctionsEntity{UID: "300", Source: SourceEU, Name: "EU PERSON"}
	set.Replace([]*domain.SanctionsEntity{ofac}, time.Now())
	screener := NewSanctionsScreenerWithSimilarity(set, .85)
	if _, err := screener.Screen(context.Background(), domain.ScreeningRequest{}); !errors.Is(err, ErrSanctionsSetNotLoaded) {
		t.Fatalf("partial list must fail closed: %v", err)
	}
	set.Replace([]*domain.SanctionsEntity{ofac, un}, time.Now())
	if set.Loaded() {
		t.Fatal("set is loaded with no EU publisher")
	}
	set.Replace([]*domain.SanctionsEntity{ofac, un, eu}, time.Now())
	if !set.Loaded() {
		t.Fatal("all publishers loaded but set not ready")
	}
}
