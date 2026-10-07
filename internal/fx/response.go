package fx

import (
	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

// RateResponse is an alias for domain.RateResponse so it lives in a package
// that both fx and wallet can depend on without creating an import cycle.
type RateResponse = domain.RateResponse
type Quote = domain.Quote
