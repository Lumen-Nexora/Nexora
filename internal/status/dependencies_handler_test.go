package status

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func TestDependencyHistoryHandlerValidatesQuery(t *testing.T) {
	handler := NewHandler(NewService(nil).WithDependencyHistory(dependencyHistoryStub{}, nil))
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	for _, path := range []string{
		"/status/dependencies/history?dependency=BadName",
		"/status/dependencies/history?since=yesterday",
		"/status/dependencies/history?limit=501",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusBadRequest, recorder.Code, path)
	}
}

func TestDependencyHistoryHandlerReturnsRetentionAndDefaultLimit(t *testing.T) {
	handler := NewHandler(NewService(nil).WithDependencyHistory(dependencyHistoryStub{list: []domain.DependencyHealthCheck{}}, nil))
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status/dependencies/history", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"checks":[],"limit":100,"retention_days":30}`, recorder.Body.String())
}
