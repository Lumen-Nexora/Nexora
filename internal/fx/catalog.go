package fx

import (
	"context"
	"fmt"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/assets"
)

type CatalogPair struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Provider string `json:"provider"`
}

type Catalog struct {
	Assets []assets.Asset `json:"assets"`
	Pairs  []CatalogPair  `json:"supported_pairs"`
}

func parsePair(pair string) (from, to string, ok bool) {
	if parts := strings.Split(pair, "-"); len(parts) == 2 {
		return parts[0], parts[1], true
	}
	if parts := strings.Split(pair, "/"); len(parts) == 2 {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func (s *service) GetCatalog(ctx context.Context, registry *assets.Registry) (*Catalog, error) {
	pairs := make([]CatalogPair, 0)
	for _, p := range s.providers {
		for _, pair := range p.SupportedPairs() {
			from, to, _ := parsePair(pair)
			pairs = append(pairs, CatalogPair{
				From:     from,
				To:       to,
				Provider: fmt.Sprintf("%T", p),
			})
		}
	}
	return &Catalog{
		Assets: registry.List(),
		Pairs:  pairs,
	}, nil
}
