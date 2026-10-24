package fiat

import (
	"strings"
	"testing"
	"time"
)

func TestReadSettlementReport(t *testing.T) {
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	header := "reference,amount,currency,status,settled_at\n"
	rows, err := ReadSettlementReport(strings.NewReader(header+"bank-1,100.0100000,NGN,settled,2026-10-22T08:00:00Z\n"), day)
	if err != nil || len(rows) != 1 || rows[0].Amount.String() != "100.01" {
		t.Fatalf("report=%v err=%v", rows, err)
	}
	cases := map[string]string{
		"bad header":     "ref,amount,currency,status,settled_at\n",
		"negative":       header + "a,-1,USD,settled,2026-10-22T00:00:00Z\n",
		"exponent":       header + "a,1e999,USD,settled,2026-10-22T00:00:00Z\n",
		"wrong day":      header + "a,1,USD,settled,2026-10-23T00:00:00Z\n",
		"unknown status": header + "a,1,USD,reversed,2026-10-22T00:00:00Z\n",
		"bad currency":   header + "a,1,usd,settled,2026-10-22T00:00:00Z\n",
		"missing field":  header + "a,1,USD,settled\n",
	}
	for name, csv := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadSettlementReport(strings.NewReader(csv), day); err == nil {
				t.Fatal("expected invalid report")
			}
		})
	}
	t.Run("duplicates preserved for audit", func(t *testing.T) {
		rows, err := ReadSettlementReport(strings.NewReader(header+strings.Repeat("a,1,USD,settled,2026-10-22T00:00:00Z\n", 2)), day)
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows %v err %v", rows, err)
		}
	})
	t.Run("row cap", func(t *testing.T) {
		if _, err := ReadSettlementReport(strings.NewReader(header+strings.Repeat("a,1,USD,settled,2026-10-22T00:00:00Z\n", 10001)), day); err == nil {
			t.Fatal("expected row cap")
		}
	})
}
