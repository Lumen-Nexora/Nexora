package treasury_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/server"
	"github.com/stellar/go/keypair"
	"github.com/stretchr/testify/assert"
)

func TestPlatformOperatorMiddleware(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/admin/treasury/config", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	record := httptest.NewRecorder()

	mw := server.RequirePlatformOperator()
	next := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	next.ServeHTTP(record, req)
	assert.Equal(t, http.StatusForbidden, record.Code)
}

func TestTreasuryServiceValidation(t *testing.T) {
	_, err := keypair.ParseAddress("not-a-stellar-address")
	assert.Error(t, err)
}
