package reconcile

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func BenchmarkAggregateDriftSnapshots100K(b *testing.B) {
	const accounts = 100_000
	snapshots := make([]DriftSnapshot, accounts)
	for i := range snapshots {
		snapshots[i] = DriftSnapshot{
			Asset:           fmt.Sprintf("ASSET-%d", i%10),
			ExpectedBalance: decimal.NewFromInt(int64(i % 1_000)),
			ActualBalance:   decimal.NewFromInt(int64(i%1_000) + 1),
		}
	}
	hour := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = aggregateDriftSnapshots(snapshots, hour, decimal.NewFromInt(1))
	}
}
