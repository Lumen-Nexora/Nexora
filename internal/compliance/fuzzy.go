package compliance

import (
	"math"
	"strings"
	"unicode/utf8"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/charltoncr/metaphone"
)

// DefaultSimilarityThreshold is a conservative name-match review threshold.
const DefaultSimilarityThreshold = 0.85

// FuzzySanctionsMatch explains a matched primary name or alias. Name matches
// are review signals, never automatic evidence of sanctioned identity.
type FuzzySanctionsMatch struct {
	EntityID      string
	EntityName    string
	Source        string
	Score         float64
	JaroWinkler   float64
	PhoneticMatch bool
}

type fuzzyRecord struct {
	normalized string
	name       string
	uid        string
	source     string
	codes      []string
	numbers    []string
}

// FuzzySanctionsIndex is immutable after construction, safe for concurrent
// screening and indexed by Double Metaphone, avoiding whole-list query scans.
type FuzzySanctionsIndex struct {
	records            []fuzzyRecord
	exact              map[string]int
	phonetic           map[string][]int
	prefix             map[string][]int
	numeric            map[string][]int
	phoneticUnnumbered map[string][]int
}

// NewFuzzySanctionsIndex retains source IDs across primary names, aliases and
// corporate entities while skipping duplicate source/name records.
func NewFuzzySanctionsIndex(entities []*domain.SanctionsEntity) *FuzzySanctionsIndex {
	idx := &FuzzySanctionsIndex{
		exact:              make(map[string]int, len(entities)),
		phonetic:           make(map[string][]int),
		prefix:             make(map[string][]int),
		numeric:            make(map[string][]int),
		phoneticUnnumbered: make(map[string][]int),
	}
	seen := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		if e == nil {
			continue
		}
		n := normalizeName(e.Name)
		if n == "" {
			continue
		}
		identity := e.Source + "\x00" + e.UID + "\x00" + n
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		record := fuzzyRecord{normalized: n, name: e.Name, uid: e.UID, source: e.Source, codes: phoneticCodes(n), numbers: numberTokens(n)}
		i := len(idx.records)
		idx.records = append(idx.records, record)
		if _, ok := idx.exact[n]; !ok {
			idx.exact[n] = i
		}
		for _, c := range record.codes {
			idx.phonetic[c] = append(idx.phonetic[c], i)
			if len(record.numbers) == 0 {
				idx.phoneticUnnumbered[c] = append(idx.phoneticUnnumbered[c], i)
			}
		}
		for _, num := range record.numbers {
			idx.numeric[num] = append(idx.numeric[num], i)
		}
		idx.prefix[namePrefix(n)] = append(idx.prefix[namePrefix(n)], i)
	}
	return idx
}

// Long numeric identifiers are discriminative for corporate entities. Build
// a separate posting index to avoid scanning 30K legal names that only differ
// by a number (where ordinary phonetic codes are identical).
func numberTokens(name string) []string {
	var out []string
	for _, token := range strings.Fields(name) {
		if len(token) < 3 {
			continue
		}
		digits := true
		for _, r := range token {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			out = append(out, token)
		}
	}
	return out
}

// Generate bounded one-substitution numeric variants. Edits beyond one digit
// are not considered a high-confidence full-name match on numeric entities.
func (idx *FuzzySanctionsIndex) numericCandidates(num string) []int {
	seen := make(map[int]struct{})
	var ids []int
	add := func(n string) {
		for _, i := range idx.numeric[n] {
			if _, ok := seen[i]; !ok {
				seen[i] = struct{}{}
				ids = append(ids, i)
			}
		}
	}
	add(num)
	for i := range num {
		for digit := byte('0'); digit <= '9'; digit++ {
			if num[i] == digit {
				continue
			}
			variant := []byte(num)
			variant[i] = digit
			add(string(variant))
		}
	}
	return ids
}

func namePrefix(s string) string {
	r := []rune(s)
	if len(r) > 3 {
		r = r[:3]
	}
	return string(r)
}

func phoneticCodes(s string) []string {
	seen := make(map[string]struct{}, 6)
	var out []string
	for _, token := range strings.Fields(s) {
		if utf8.RuneCountInString(token) < 3 {
			continue
		}
		switch token {
		case "inc", "llc", "ltd", "limited", "company", "corporation", "corp", "group", "the":
			continue
		}
		primary, secondary := metaphone.DoubleMetaphone(token, 8)
		for _, code := range []string{primary, secondary} {
			if code == "" {
				continue
			}
			if _, found := seen[code]; !found {
				seen[code] = struct{}{}
				out = append(out, code)
			}
		}
	}
	return out
}

// Match returns the strongest indexed candidate with a transparent score.
// Unrelated names must not match merely because one token sounds similar.
func (idx *FuzzySanctionsIndex) Match(name string, threshold float64) (FuzzySanctionsMatch, bool) {
	if idx == nil {
		return FuzzySanctionsMatch{}, false
	}
	n := normalizeName(name)
	if n == "" {
		return FuzzySanctionsMatch{}, false
	}
	if threshold <= 0 || threshold > 1 || math.IsNaN(threshold) {
		threshold = DefaultSimilarityThreshold
	}
	if i, ok := idx.exact[n]; ok {
		return idx.makeMatch(i, 1, 1, true), true
	}

	codes := phoneticCodes(n)
	var candidates []int
	nums := numberTokens(n)
	if len(nums) > 0 {
		candidates = idx.numericCandidates(nums[0])
	} else {
		// Unnumbered queries cannot identify a distinct numbered company:
		// omit numbered records from phonetic postings altogether.
		for _, code := range codes {
			posting := idx.phoneticUnnumbered[code]
			if len(posting) > 0 && (candidates == nil || len(posting) < len(candidates)) {
				candidates = posting
			}
		}
		if candidates == nil {
			for _, i := range idx.prefix[namePrefix(n)] {
				if len(idx.records[i].numbers) == 0 {
					candidates = append(candidates, i)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return FuzzySanctionsMatch{}, false
	}

	best := FuzzySanctionsMatch{}
	for _, i := range candidates {
		row := idx.records[i]
		short, long := utf8.RuneCountInString(n), utf8.RuneCountInString(row.normalized)
		if short > long {
			short, long = long, short
		}
		// Do not screen a short prefix as if it were a complete identity.
		if long == 0 || float64(short)/float64(long) < 0.82 {
			continue
		}
		jw := jaroWinkler(n, row.normalized)
		if jw < 0.76 {
			continue
		}
		phonetic := hasCommonCode(codes, row.codes)
		score := jw*0.8 + 0.1
		if phonetic {
			score += 0.1
		}
		if score >= threshold && score > best.Score {
			best = idx.makeMatch(i, score, jw, phonetic)
		}
	}
	return best, best.Score >= threshold
}

func (idx *FuzzySanctionsIndex) makeMatch(i int, score, jw float64, phonetic bool) FuzzySanctionsMatch {
	r := idx.records[i]
	return FuzzySanctionsMatch{
		EntityID: r.uid, EntityName: r.name, Source: r.source,
		Score: score, JaroWinkler: jw, PhoneticMatch: phonetic,
	}
}

func hasCommonCode(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// jaroWinkler uses the standard Jaro matching window and up to four shared
// prefix characters for the Winkler bonus. It handles UTF-8 as runes.
func jaroWinkler(a, b string) float64 {
	left, right := []rune(a), []rune(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	if a == b {
		return 1
	}
	window := max(len(left), len(right))/2 - 1
	if window < 0 {
		window = 0
	}
	usedA, usedB := make([]bool, len(left)), make([]bool, len(right))
	count := 0
	for i, c := range left {
		start, end := max(0, i-window), min(len(right), i+window+1)
		for j := start; j < end; j++ {
			if !usedB[j] && c == right[j] {
				usedA[i], usedB[j] = true, true
				count++
				break
			}
		}
	}
	if count == 0 {
		return 0
	}
	swapped, j := 0, 0
	for i, found := range usedA {
		if !found {
			continue
		}
		for !usedB[j] {
			j++
		}
		if left[i] != right[j] {
			swapped++
		}
		j++
	}
	m := float64(count)
	value := (m/float64(len(left)) + m/float64(len(right)) + (m-float64(swapped)/2)/m) / 3
	if value <= 0.7 {
		return value
	}
	common := 0
	for common < 4 && common < len(left) && common < len(right) && left[common] == right[common] {
		common++
	}
	return value + float64(common)*0.1*(1-value)
}
