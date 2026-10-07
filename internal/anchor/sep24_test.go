package anchor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

func TestSep24Client_GetInteractiveUrl_ReturnsValidHTTPSUrl(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/transactions/deposit/interactive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST per SEP-24, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-jwt" {
			t.Errorf("expected bearer jwt, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"type": "interactive_customer_info_needed",
			"url":  "https://anchor.example.com/kycflow?token=abc",
			"id":   "sep24-tx-1",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSep24Client(nil)
	interactiveURL, txID, err := client.GetInteractiveUrl(context.Background(), srv.URL, "test-jwt", "USDC", "GACCOUNT", "50", domain.AnchorTxTypeDeposit)
	if err != nil {
		t.Fatalf("GetInteractiveUrl returned unexpected error: %v", err)
	}
	if txID != "sep24-tx-1" {
		t.Fatalf("expected transaction id sep24-tx-1, got %q", txID)
	}
	parsed, err := url.Parse(interactiveURL)
	if err != nil {
		t.Fatalf("interactive url did not parse: %v", err)
	}
	if parsed.Scheme != "https" {
		t.Fatalf("expected an https interactive url, got %q", interactiveURL)
	}
}

func TestSep24Client_GetInteractiveUrl_RejectsNonHTTPS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/transactions/withdraw/interactive", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"type": "interactive_customer_info_needed",
			"url":  "http://insecure.example.com/kycflow", // not https
			"id":   "sep24-tx-2",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSep24Client(nil)
	_, _, err := client.GetInteractiveUrl(context.Background(), srv.URL, "test-jwt", "USDC", "GACCOUNT", "", domain.AnchorTxTypeWithdrawal)
	if err == nil {
		t.Fatalf("expected an error for a non-https interactive url")
	}
}

func TestSep24Client_PollTransaction(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/transaction", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"transaction": map[string]interface{}{
				"id":     "sep24-tx-1",
				"status": "pending_user_transfer_start",
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewSep24Client(nil)
	tx, err := client.PollTransaction(context.Background(), srv.URL, "test-jwt", "sep24-tx-1")
	if err != nil {
		t.Fatalf("PollTransaction returned unexpected error: %v", err)
	}
	if tx.Status != "pending_user_transfer_start" {
		t.Fatalf("unexpected status %q", tx.Status)
	}
}
