package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

func TestRequireAdminUsesCurrentDatabaseRole(t *testing.T) {
	deps := Dependencies{Users: fakeUserLookup{user: auth.User{ID: 1, SuperUser: false}}}
	handler := deps.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
		SuperUser: true, RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("downgraded admin retained token authority: status=%d", response.Code)
	}
}

func TestRequireManageUsesCurrentDatabasePermission(t *testing.T) {
	deps := Dependencies{Users: fakeUserLookup{user: auth.User{
		ID: 1, Permissions: map[string]any{"manage": true},
	}}}
	handler := deps.requireManage(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/manage", nil)
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("current manage permission was rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRequireManageRejectsStaleSuperuserClaim(t *testing.T) {
	deps := Dependencies{Users: fakeUserLookup{user: auth.User{ID: 1, SuperUser: false}}}
	handler := deps.requireManage(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/manage", nil)
	request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
		SuperUser: true, RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("stale superuser claim retained manage authority: status=%d", response.Code)
	}
}
