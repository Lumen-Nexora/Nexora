package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrUnbalancedEntry = errors.New("journal entry debits and credits do not balance")
	ErrEmptyEntry      = errors.New("journal entry has no postings")
	ErrNonPositive     = errors.New("posting amount must be strictly positive")
	ErrCurrencyMix     = errors.New("all postings in an entry must share one currency")
)

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Posting is a single debit or credit against one ledger account.
type Posting struct {
	AccountID string
	Direction Direction
	Amount    decimal.Decimal
}

// JournalEntry groups postings that must balance to zero.
type JournalEntry struct {
	ID          string
	TenantID    string
	Description string
	Currency    string
	PostedAt    time.Time
	Postings    []Posting
}

// Validate enforces the fundamental accounting equation: total debits equal
// total credits, every amount is positive, and one currency is used.
func (e JournalEntry) Validate() error {
	if len(e.Postings) == 0 {
		return ErrEmptyEntry
	}
	debits := decimal.Zero
	credits := decimal.Zero
	for _, p := range e.Postings {
		if !p.Amount.IsPositive() {
			return ErrNonPositive
		}
		switch p.Direction {
		case Debit:
			debits = debits.Add(p.Amount)
		case Credit:
			credits = credits.Add(p.Amount)
		default:
			return fmt.Errorf("invalid posting direction %q", p.Direction)
		}
	}
	if !debits.Equal(credits) {
		return fmt.Errorf("%w: debits=%s credits=%s", ErrUnbalancedEntry, debits, credits)
	}
	return nil
}

// ContentHash returns a deterministic SHA-256 over the entry contents, used to
// chain integrity verification for append-only storage.
func (e JournalEntry) ContentHash(prevHash string) string {
	ps := make([]Posting, len(e.Postings))
	copy(ps, e.Postings)
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].AccountID != ps[j].AccountID {
			return ps[i].AccountID < ps[j].AccountID
		}
		if ps[i].Direction != ps[j].Direction {
			return ps[i].Direction < ps[j].Direction
		}
		return ps[i].Amount.Cmp(ps[j].Amount) < 0
	})
	var b strings.Builder
	b.WriteString(prevHash)
	b.WriteString("|")
	b.WriteString(e.TenantID)
	b.WriteString("|")
	b.WriteString(e.Currency)
	b.WriteString("|")
	b.WriteString(e.Description)
	for _, p := range ps {
		fmt.Fprintf(&b, "|%s:%s:%s", p.AccountID, p.Direction, p.Amount.String())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// AccountBalance computes a signed running balance from postings. Debit-normal
// accounts (asset, expense) increase on debit; credit-normal accounts
// (liability, equity, revenue) increase on credit.
func AccountBalance(accountType string, postings []Posting) decimal.Decimal {
	debits := decimal.Zero
	credits := decimal.Zero
	for _, p := range postings {
		if p.Direction == Debit {
			debits = debits.Add(p.Amount)
		} else {
			credits = credits.Add(p.Amount)
		}
	}
	switch accountType {
	case "asset", "expense":
		return debits.Sub(credits)
	default:
		return credits.Sub(debits)
	}
}

// ZeroSumCheck verifies that across a set of entries total debits equal total
// credits, returning the imbalance (zero when balanced).
func ZeroSumCheck(entries []JournalEntry) decimal.Decimal {
	net := decimal.Zero
	for _, e := range entries {
		for _, p := range e.Postings {
			if p.Direction == Debit {
				net = net.Add(p.Amount)
			} else {
				net = net.Sub(p.Amount)
			}
		}
	}
	return net
}
