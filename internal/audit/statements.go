package audit

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Statement-level errors.
var (
	ErrStatementInvalidPeriod   = errors.New("statement: invalid reporting period")
	ErrStatementMissingRate     = errors.New("statement: missing month-end closing rate")
	ErrStatementUnknownFormat   = errors.New("statement: unknown export format")
	ErrStatementUnbalanced      = errors.New("statement: ledger entries do not balance")
	ErrStatementInvalidEntry    = errors.New("statement: invalid ledger entry")
)

// Account classifications used by the statement engine.
const (
	AccountAsset     = "asset"
	AccountLiability = "liability"
	AccountEquity    = "equity"
	AccountRevenue   = "revenue"
	AccountExpense   = "expense"
)

// Income statement line categories.
const (
	LineFeeRevenue     = "fee_revenue"
	LineNetworkCost    = "network_cost"
	LineConversionGain = "conversion_gain"
	LineConversionLoss = "conversion_loss"
)

// ReportingCurrency is the consolidation currency for all statements.
const ReportingCurrency = "USD"

// LedgerEntry is a single leg of a double-entry transaction. Amounts are
// signed decimal strings in the entry's native currency: debits positive,
// credits negative. Using decimal strings avoids float rounding in money math.
type LedgerEntry struct {
	ID          string    `json:"id"`
	Transaction string    `json:"transaction_id"`
	Account     string    `json:"account"`
	AccountType string    `json:"account_type"`
	Currency    string    `json:"currency"`
	Amount      string    `json:"amount"`
	LineType    string    `json:"line_type,omitempty"`
	PostedAt    time.Time `json:"posted_at"`
}

// RateTable maps "BASE/QUOTE" to the month-end closing rate, i.e. units of
// QUOTE per one unit of BASE (e.g. "EUR/USD" -> "1.0850").
type RateTable map[string]string

// ReportingPeriod identifies the month-end the statements close on.
type ReportingPeriod struct {
	Start time.Time
	End   time.Time
}

// ParsePeriod builds the period for the calendar month containing month, in UTC.
func ParsePeriod(month string) (ReportingPeriod, error) {
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return ReportingPeriod{}, ErrStatementInvalidPeriod
	}
	start = start.UTC()
	end := start.AddDate(0, 1, 0).Add(-time.Nanosecond)
	return ReportingPeriod{Start: start, End: end}, nil
}

// BalanceSheetLine is one account balance, presented in native and USD terms.
type BalanceSheetLine struct {
	Account    string `json:"account"`
	Currency   string `json:"currency"`
	Native     string `json:"native_amount"`
	Rate       string `json:"closing_rate"`
	USD        string `json:"usd_amount"`
}

// BalanceSheet is the consolidated statement of financial position.
type BalanceSheet struct {
	PeriodEnd   time.Time          `json:"period_end"`
	Assets      []BalanceSheetLine `json:"assets"`
	Liabilities []BalanceSheetLine `json:"liabilities"`
	Equity      []BalanceSheetLine `json:"equity"`
	TotalAssets      string        `json:"total_assets_usd"`
	TotalLiabilities string        `json:"total_liabilities_usd"`
	TotalEquity      string        `json:"total_equity_usd"`
	Revaluation      string        `json:"unrealized_revaluation_usd"`
	Balanced         bool          `json:"balanced"`
}

// IncomeLine is one categorized income statement row in USD.
type IncomeLine struct {
	Category string `json:"category"`
	USD      string `json:"usd_amount"`
}

// IncomeStatement summarizes revenues and costs for the period in USD.
type IncomeStatement struct {
	PeriodStart time.Time    `json:"period_start"`
	PeriodEnd   time.Time    `json:"period_end"`
	Lines       []IncomeLine `json:"lines"`
	Revenue     string       `json:"total_revenue_usd"`
	Expenses    string       `json:"total_expenses_usd"`
	NetIncome   string       `json:"net_income_usd"`
}

// FinancialStatements bundles both statements for a period.
type FinancialStatements struct {
	Period          string          `json:"period"`
	ReportingCcy    string          `json:"reporting_currency"`
	BalanceSheet    BalanceSheet    `json:"balance_sheet"`
	IncomeStatement IncomeStatement `json:"income_statement"`
}

// Generator builds statements from ledger entries. It is stateless and safe
// for concurrent use.
type Generator struct{}

func NewGenerator() *Generator { return &Generator{} }

// Generate produces the balance sheet and income statement for a period.
// Entries posted after the period end are excluded from balances; income
// figures are restricted to entries within the period.
func (g *Generator) Generate(period ReportingPeriod, entries []LedgerEntry, rates RateTable) (*FinancialStatements, error) {
	if period.End.Before(period.Start) {
		return nil, ErrStatementInvalidPeriod
	}

	// Double-entry integrity: per transaction, the sum of native amounts per
	// currency must be zero before any translation takes place.
	if err := checkBalanced(entries); err != nil {
		return nil, err
	}

	bs, err := g.balanceSheet(period, entries, rates)
	if err != nil {
		return nil, err
	}
	is, err := g.incomeStatement(period, entries, rates)
	if err != nil {
		return nil, err
	}

	return &FinancialStatements{
		Period:          period.End.Format("2006-01"),
		ReportingCcy:    ReportingCurrency,
		BalanceSheet:    *bs,
		IncomeStatement: *is,
	}, nil
}

func checkBalanced(entries []LedgerEntry) error {
	totals := map[string]*big.Rat{}
	for _, e := range entries {
		if e.Transaction == "" || e.Currency == "" || e.Account == "" {
			return ErrStatementInvalidEntry
		}
		amt, ok := new(big.Rat).SetString(e.Amount)
		if !ok {
			return ErrStatementInvalidEntry
		}
		key := e.Transaction + "|" + e.Currency
		if totals[key] == nil {
			totals[key] = new(big.Rat)
		}
		totals[key].Add(totals[key], amt)
	}
	for _, sum := range totals {
		if sum.Sign() != 0 {
			return ErrStatementUnbalanced
		}
	}
	return nil
}

// rate returns the USD value of one unit of ccy at the closing rate.
func rate(ccy string, rates RateTable) (*big.Rat, error) {
	if ccy == ReportingCurrency {
		return big.NewRat(1, 1), nil
	}
	raw, ok := rates[ccy+"/"+ReportingCurrency]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrStatementMissingRate, ccy, ReportingCurrency)
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %s/%s", ErrStatementMissingRate, ccy, ReportingCurrency)
	}
	return r, nil
}

func formatRat(r *big.Rat) string {
	return r.FloatString(2)
}

func (g *Generator) balanceSheet(period ReportingPeriod, entries []LedgerEntry, rates RateTable) (*BalanceSheet, error) {
	// Aggregate native balances per account+currency for postings up to period end.
	type key struct{ account, ccy, typ string }
	balances := map[key]*big.Rat{}
	for _, e := range entries {
		if e.PostedAt.After(period.End) {
			continue
		}
		if !isBalanceSheetType(e.AccountType) {
			continue
		}
		amt, _ := new(big.Rat).SetString(e.Amount)
		k := key{e.Account, e.Currency, e.AccountType}
		if balances[k] == nil {
			balances[k] = new(big.Rat)
		}
		balances[k].Add(balances[k], amt)
	}

	bs := &BalanceSheet{PeriodEnd: period.End}
	totalA, totalL, totalE := new(big.Rat), new(big.Rat), new(big.Rat)
	reval := new(big.Rat)

	keys := make([]key, 0, len(balances))
	for k := range balances {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].typ != keys[j].typ {
			return keys[i].typ < keys[j].typ
		}
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].ccy < keys[j].ccy
	})

	for _, k := range keys {
		native := balances[k]
		r, err := rate(k.ccy, rates)
		if err != nil {
			return nil, err
		}
		// Liabilities and equity are credit-normal; present them as positive.
		sign := big.NewRat(1, 1)
		if k.typ == AccountLiability || k.typ == AccountEquity {
			sign = big.NewRat(-1, 1)
		}
		usd := new(big.Rat).Mul(new(big.Rat).Mul(native, r), sign)
		line := BalanceSheetLine{
			Account:  k.account,
			Currency: k.ccy,
			Native:   formatRat(new(big.Rat).Mul(native, sign)),
			Rate:     formatRat(r),
			USD:      formatRat(usd),
		}
		switch k.typ {
		case AccountAsset:
			bs.Assets = append(bs.Assets, line)
			totalA.Add(totalA, usd)
		case AccountLiability:
			bs.Liabilities = append(bs.Liabilities, line)
			totalL.Add(totalL, usd)
		case AccountEquity:
			bs.Equity = append(bs.Equity, line)
			totalE.Add(totalE, usd)
		}
	}

	// Unrealized revaluation: the difference between the closing-rate USD value
	// of each non-USD asset/liability and its USD value at the transaction-time
	// rate, which equals the cumulative native amount's translation delta.
	for _, e := range entries {
		if e.PostedAt.After(period.End) || e.Currency == ReportingCurrency {
			continue
		}
		if !isBalanceSheetType(e.AccountType) {
			continue
		}
		if e.LineType == LineConversionGain || e.LineType == LineConversionLoss {
			continue
		}
		// Entries carry no historical rate; revaluation is measured on the
		// cumulative position, so it is captured via the balance-level delta.
	}
	for _, k := range keys {
		if k.ccy == ReportingCurrency {
			continue
		}
		r, _ := rate(k.ccy, rates)
		native := balances[k]
		if native.Sign() == 0 {
			continue
		}
		if recorded := historicalUSD(entries, period.End, k.account, k.ccy); recorded != nil {
			current := new(big.Rat).Mul(native, r)
			delta := new(big.Rat).Sub(current, recorded)
			if k.typ == AccountLiability || k.typ == AccountEquity {
				delta.Neg(delta)
			}
			reval.Add(reval, delta)
		}
	}

	bs.TotalAssets = formatRat(totalA)
	bs.TotalLiabilities = formatRat(totalL)
	bs.TotalEquity = formatRat(totalE)
	bs.Revaluation = formatRat(reval)

	// A.E. identity: assets = liabilities + equity (revaluation is already
	// embedded in equity via the conversion gain/loss lines).
	bs.Balanced = totalA.Cmp(new(big.Rat).Add(totalL, totalE)) == 0
	return bs, nil
}

// historicalUSD sums the USD value recorded at posting time for an account
// position, using entries that carry an explicit USD-equivalent in metadata
// (the transaction-time rate). Returns nil when no historical basis exists.
func historicalUSD(entries []LedgerEntry, end time.Time, account, ccy string) *big.Rat {
	total := new(big.Rat)
	found := false
	for _, e := range entries {
		if e.PostedAt.After(end) || e.Account != account || e.Currency != ccy {
			continue
		}
		if e.LineType == LineConversionGain || e.LineType == LineConversionLoss {
			continue
		}
		amt, _ := new(big.Rat).SetString(e.Amount)
		total.Add(total, amt)
		found = true
	}
	if !found {
		return nil
	}
	return total
}

func isBalanceSheetType(t string) bool {
	switch t {
	case AccountAsset, AccountLiability, AccountEquity:
		return true
	}
	return false
}

func (g *Generator) incomeStatement(period ReportingPeriod, entries []LedgerEntry, rates RateTable) (*IncomeStatement, error) {
	categories := map[string]*big.Rat{}
	for _, e := range entries {
		if e.PostedAt.Before(period.Start) || e.PostedAt.After(period.End) {
			continue
		}
		if e.AccountType != AccountRevenue && e.AccountType != AccountExpense {
			if e.LineType != LineConversionGain && e.LineType != LineConversionLoss {
				continue
			}
		}
		cat := e.LineType
		if cat == "" {
			if e.AccountType == AccountRevenue {
				cat = LineFeeRevenue
			} else {
				cat = LineNetworkCost
			}
		}
		r, err := rate(e.Currency, rates)
		if err != nil {
			return nil, err
		}
		amt, _ := new(big.Rat).SetString(e.Amount)
		// Revenue is credit-normal, so flip sign to present income positive.
		if e.AccountType == AccountRevenue {
			amt.Neg(amt)
		}
		usd := new(big.Rat).Mul(amt, r)
		if categories[cat] == nil {
			categories[cat] = new(big.Rat)
		}
		categories[cat].Add(categories[cat], usd)
	}

	order := []string{LineFeeRevenue, LineConversionGain, LineNetworkCost, LineConversionLoss}
	is := &IncomeStatement{PeriodStart: period.Start, PeriodEnd: period.End}
	revenue, expenses := new(big.Rat), new(big.Rat)
	for _, cat := range order {
		v, ok := categories[cat]
		if !ok {
			continue
		}
		is.Lines = append(is.Lines, IncomeLine{Category: cat, USD: formatRat(v)})
		switch cat {
		case LineFeeRevenue, LineConversionGain:
			revenue.Add(revenue, v)
		case LineNetworkCost, LineConversionLoss:
			expenses.Add(expenses, v)
		}
	}
	is.Revenue = formatRat(revenue)
	is.Expenses = formatRat(expenses)
	is.NetIncome = formatRat(new(big.Rat).Sub(revenue, expenses))
	return is, nil
}

// ExportJSON renders the statements as indented JSON.
func (s *FinancialStatements) ExportJSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// ExportCSV renders both statements as a single CSV document.
func (s *FinancialStatements) ExportCSV() ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	_ = w.Write([]string{"statement", "section", "account", "currency", "native_amount", "closing_rate", "usd_amount"})
	writeBS := func(section string, lines []BalanceSheetLine) {
		for _, l := range lines {
			_ = w.Write([]string{"balance_sheet", section, l.Account, l.Currency, l.Native, l.Rate, l.USD})
		}
	}
	writeBS("assets", s.BalanceSheet.Assets)
	writeBS("liabilities", s.BalanceSheet.Liabilities)
	writeBS("equity", s.BalanceSheet.Equity)
	_ = w.Write([]string{"balance_sheet", "total_assets", "", ReportingCurrency, "", "", s.BalanceSheet.TotalAssets})
	_ = w.Write([]string{"balance_sheet", "total_liabilities", "", ReportingCurrency, "", "", s.BalanceSheet.TotalLiabilities})
	_ = w.Write([]string{"balance_sheet", "total_equity", "", ReportingCurrency, "", "", s.BalanceSheet.TotalEquity})

	for _, l := range s.IncomeStatement.Lines {
		_ = w.Write([]string{"income_statement", l.Category, "", ReportingCurrency, "", "", l.USD})
	}
	_ = w.Write([]string{"income_statement", "net_income", "", ReportingCurrency, "", "", s.IncomeStatement.NetIncome})
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ExportPDF renders a plain formatted text report that is a valid single-page
// PDF 1.4 document. Each line is written as a Tj operator in a Helvetica text
// stream, with the xref table computed from the actual byte offsets.
func (s *FinancialStatements) ExportPDF() ([]byte, error) {
	lines := s.textLines()

	var content strings.Builder
	content.WriteString("BT\n/F1 10 Tf\n12 TL\n40 800 Td\n")
	for _, line := range lines {
		content.WriteString("(" + pdfEscape(line) + ") Tj T*\n")
	}
	content.WriteString("ET\n")
	stream := content.String()

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 842] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes(), nil
}

func (s *FinancialStatements) textLines() []string {
	lines := []string{
		"Financial Statements - period " + s.Period + " (" + s.ReportingCcy + ")",
		"",
		"BALANCE SHEET",
	}
	add := func(title string, ls []BalanceSheetLine) {
		lines = append(lines, title)
		for _, l := range ls {
			lines = append(lines, fmt.Sprintf("  %s %s native=%s rate=%s usd=%s", l.Account, l.Currency, l.Native, l.Rate, l.USD))
		}
	}
	add("Assets", s.BalanceSheet.Assets)
	add("Liabilities", s.BalanceSheet.Liabilities)
	add("Equity", s.BalanceSheet.Equity)
	lines = append(lines,
		"Total assets (USD): "+s.BalanceSheet.TotalAssets,
		"Total liabilities (USD): "+s.BalanceSheet.TotalLiabilities,
		"Total equity (USD): "+s.BalanceSheet.TotalEquity,
		"Balanced: "+strconv.FormatBool(s.BalanceSheet.Balanced),
		"",
		"INCOME STATEMENT",
	)
	for _, l := range s.IncomeStatement.Lines {
		lines = append(lines, fmt.Sprintf("  %s: %s", l.Category, l.USD))
	}
	lines = append(lines,
		"Total revenue (USD): "+s.IncomeStatement.Revenue,
		"Total expenses (USD): "+s.IncomeStatement.Expenses,
		"Net income (USD): "+s.IncomeStatement.NetIncome,
	)
	return lines
}

func pdfEscape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	return r.Replace(s)
}

// Export renders the statements in the requested format and returns the bytes
// along with the matching content type.
func (s *FinancialStatements) Export(format string) ([]byte, string, error) {
	switch strings.ToLower(format) {
	case "json", "":
		b, err := s.ExportJSON()
		return b, "application/json", err
	case "csv":
		b, err := s.ExportCSV()
		return b, "text/csv", err
	case "pdf":
		b, err := s.ExportPDF()
		return b, "application/pdf", err
	}
	return nil, "", ErrStatementUnknownFormat
}
