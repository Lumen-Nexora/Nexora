package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultHorizonLedgerTimeout = 10 * time.Second

// maxHorizonRootBytes caps how much of the Horizon root document is read.
const maxHorizonRootBytes = 1 << 20

// HorizonLedgerSource reads the latest ledger Horizon has ingested from the
// Horizon root endpoint. It is the LedgerSource the auto-remediator uses to
// count an evaluation cycle only after a new ledger has closed.
type HorizonLedgerSource struct {
	baseURL string
	client  *http.Client
}

// NewHorizonLedgerSource builds a ledger source for horizonURL. A
// non-positive timeout falls back to 10s.
func NewHorizonLedgerSource(horizonURL string, timeout time.Duration) *HorizonLedgerSource {
	if timeout <= 0 {
		timeout = defaultHorizonLedgerTimeout
	}
	return &HorizonLedgerSource{
		baseURL: strings.TrimRight(horizonURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// LatestLedger returns Horizon's history_latest_ledger.
func (h *HorizonLedgerSource) LatestLedger(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/", nil)
	if err != nil {
		return 0, fmt.Errorf("build Horizon root request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch Horizon root: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch Horizon root: unexpected status %d", resp.StatusCode)
	}
	var root struct {
		HistoryLatestLedger int64 `json:"history_latest_ledger"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHorizonRootBytes)).Decode(&root); err != nil {
		return 0, fmt.Errorf("decode Horizon root: %w", err)
	}
	if root.HistoryLatestLedger <= 0 {
		return 0, errors.New("horizon root reported no ingested ledger")
	}
	return root.HistoryLatestLedger, nil
}
