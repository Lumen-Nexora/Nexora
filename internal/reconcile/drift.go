package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type DriftTrendSnapshot struct {
	ID                           string          `json:"id"`
	Asset                        string          `json:"asset"`
	TotalInternalLedgerBalance   decimal.Decimal `json:"total_internal_ledger_balance"`
	TotalOnChainBalance          decimal.Decimal `json:"total_on_chain_balance"`
	NetDrift                     decimal.Decimal `json:"net_drift"`
	Threshold                    decimal.Decimal `json:"threshold"`
	SnapshotHour                 time.Time       `json:"snapshot_hour"`
	MovingAverageDrift           decimal.Decimal `json:"moving_average_drift"`
	AcceleratingVarianceAnomaly  bool            `json:"accelerating_variance_anomaly"`
}

type DriftTrendRepository interface {
	WriteHourlyDriftSnapshots(context.Context, []DriftTrendSnapshot) error
	ListDriftTrendSnapshots(context.Context, time.Time, time.Time, string) ([]*DriftTrendSnapshot, error)
}

type driftTotals struct {
	internal decimal.Decimal
	onChain  decimal.Decimal
}

func aggregateDriftSnapshots(snapshots []DriftSnapshot, snapshotHour time.Time, threshold decimal.Decimal) []DriftTrendSnapshot {
	totalsByAsset := make(map[string]driftTotals)
	for _, snapshot := range snapshots {
		totals := totalsByAsset[snapshot.Asset]
		totals.internal = totals.internal.Add(snapshot.ExpectedBalance)
		totals.onChain = totals.onChain.Add(snapshot.ActualBalance)
		totalsByAsset[snapshot.Asset] = totals
	}

	assets := make([]string, 0, len(totalsByAsset))
	for asset := range totalsByAsset {
		assets = append(assets, asset)
	}
	sort.Strings(assets)

	result := make([]DriftTrendSnapshot, 0, len(assets))
	for _, asset := range assets {
		totals := totalsByAsset[asset]
		result = append(result, DriftTrendSnapshot{
			ID:                         uuid.New().String(),
			Asset:                      asset,
			TotalInternalLedgerBalance: totals.internal,
			TotalOnChainBalance:       totals.onChain,
			NetDrift:                  totals.onChain.Sub(totals.internal),
			Threshold:                 threshold,
			SnapshotHour:              snapshotHour.UTC(),
		})
	}
	return result
}

func (s *Service) GetDriftTrends(ctx context.Context, from, to time.Time, asset string) ([]*DriftTrendSnapshot, error) {
	repo, ok := s.driftRepo.(DriftTrendRepository)
	if !ok {
		return nil, errors.New("reconciliation drift trend repository is unavailable")
	}
	snapshots, err := repo.ListDriftTrendSnapshots(ctx, from.Add(-2*time.Hour), to, asset)
	if err != nil {
		return nil, fmt.Errorf("list reconciliation drift trends: %w", err)
	}
	if snapshots == nil {
		return []*DriftTrendSnapshot{}, nil
	}

	previous := make(map[string][]*DriftTrendSnapshot)
	trends := make([]*DriftTrendSnapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		history := previous[snapshot.Asset]
		window := history
		if len(window) > 2 {
			window = window[len(window)-2:]
		}
		total := snapshot.NetDrift
		for _, point := range window {
			total = total.Add(point.NetDrift)
		}
		snapshot.MovingAverageDrift = total.Div(decimal.NewFromInt(int64(len(window) + 1)))
		if len(history) >= 2 {
			older := history[len(history)-2]
			newer := history[len(history)-1]
			if newer.SnapshotHour.Sub(older.SnapshotHour) == time.Hour &&
				snapshot.SnapshotHour.Sub(newer.SnapshotHour) == time.Hour {
				olderMagnitude := older.NetDrift.Abs()
				newerMagnitude := newer.NetDrift.Abs()
				currentMagnitude := snapshot.NetDrift.Abs()
				previousIncrease := newerMagnitude.Sub(olderMagnitude)
				currentIncrease := currentMagnitude.Sub(newerMagnitude)
				snapshot.AcceleratingVarianceAnomaly = previousIncrease.IsPositive() &&
					currentIncrease.GreaterThan(previousIncrease)
			}
		}
		history = append(history, snapshot)
		if len(history) > 2 {
			history = history[len(history)-2:]
		}
		previous[snapshot.Asset] = history
		if !snapshot.SnapshotHour.Before(from) {
			trends = append(trends, snapshot)
		}
	}
	return trends, nil
}
