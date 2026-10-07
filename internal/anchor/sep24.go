package anchor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

// Sep24Client starts interactive deposits/withdrawals (SEP-24) against an
// anchor's TRANSFER_SERVER_SEP0024, handing back a URL to open in an iframe
// or webview rather than collecting bank details directly.
type Sep24Client struct {
	httpClient *http.Client
}

func NewSep24Client(httpClient *http.Client) *Sep24Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Sep24Client{httpClient: httpClient}
}

type interactiveRequest struct {
	AssetCode string `json:"asset_code"`
	Amount    string `json:"amount,omitempty"`
	Account   string `json:"account,omitempty"`
}

type interactiveResponse struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	ID    string `json:"id"`
	Error string `json:"error"`
}

// GetInteractiveUrl starts a SEP-24 interactive deposit or withdrawal
// ("deposit" or "withdraw") and returns the HTTPS URL the client should open
// to complete it, along with the anchor-assigned transaction id.
func (c *Sep24Client) GetInteractiveUrl(ctx context.Context, transferServerSep24, jwt, assetCode, account, amount, operation string) (interactiveURL, transactionID string, err error) {
	var path string
	switch operation {
	case domain.AnchorTxTypeDeposit:
		path = "/transactions/deposit/interactive"
	case domain.AnchorTxTypeWithdrawal, "withdraw":
		path = "/transactions/withdraw/interactive"
	default:
		return "", "", fmt.Errorf("unsupported sep-24 operation %q", operation)
	}

	reqBody, err := json.Marshal(interactiveRequest{AssetCode: assetCode, Amount: amount, Account: account})
	if err != nil {
		return "", "", fmt.Errorf("marshal interactive request: %w", err)
	}

	var out interactiveResponse
	if err := doAnchorRequest(ctx, c.httpClient, http.MethodPost, transferServerSep24+path, jwt, bytes.NewReader(reqBody), &out); err != nil {
		return "", "", fmt.Errorf("start sep-24 %s: %w", operation, err)
	}
	if out.Error != "" {
		return "", "", fmt.Errorf("anchor returned error: %s", out.Error)
	}
	if out.URL == "" {
		return "", "", fmt.Errorf("anchor did not return an interactive url")
	}
	parsed, err := url.Parse(out.URL)
	if err != nil || parsed.Scheme != "https" {
		return "", "", fmt.Errorf("anchor returned a non-https interactive url: %q", out.URL)
	}

	return out.URL, out.ID, nil
}

// PollTransaction fetches the current status of a SEP-24 transaction
// previously started with GetInteractiveUrl.
func (c *Sep24Client) PollTransaction(ctx context.Context, transferServerSep24, jwt, transactionID string) (*AnchorTransaction, error) {
	q := url.Values{}
	q.Set("id", transactionID)

	var out getTransactionResponse
	if err := doAnchorRequest(ctx, c.httpClient, http.MethodGet, transferServerSep24+"/transaction?"+q.Encode(), jwt, nil, &out); err != nil {
		return nil, fmt.Errorf("get sep-24 transaction: %w", err)
	}
	return &out.Transaction, nil
}
