package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type fakeSiteUserDataAPIStore struct {
	*fakeSiteStore
	latest          []database.SiteUserData
	history         []database.SiteUserData
	latestErr       error
	historyErr      error
	queriedDomain   string
	queriedWorkdate string
}

func (s *fakeSiteUserDataAPIStore) LatestSiteUserData(context.Context) ([]database.SiteUserData, error) {
	if s.latestErr != nil {
		return nil, s.latestErr
	}
	return append([]database.SiteUserData(nil), s.latest...), nil
}

func (s *fakeSiteUserDataAPIStore) SiteUserDataByDomain(
	_ context.Context,
	domain string,
	workdate string,
) ([]database.SiteUserData, error) {
	s.queriedDomain = domain
	s.queriedWorkdate = workdate
	if s.historyErr != nil {
		return nil, s.historyErr
	}
	return append([]database.SiteUserData(nil), s.history...), nil
}

func TestLatestSiteUserDataMatchesV1AndV2ResponseShapes(t *testing.T) {
	username := "alice"
	upload := 123.0
	store := &fakeSiteUserDataAPIStore{
		fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{}},
		latest: []database.SiteUserData{{
			ID: 9, Domain: "tracker.example", Name: "Internal name", Username: &username,
			Upload: &upload, SeedingInfo: json.RawMessage(`[]`), MessageUnreadContents: nil,
			UpdatedDay: "2026-08-16", UpdatedTime: "12:00:00",
		}},
	}
	router := siteUserDataCompatibilityRouter(t, store, auth.User{ID: 1, SuperUser: true})

	tests := []struct {
		name    string
		path    string
		wrapped bool
	}{
		{name: "v1 raw array", path: "/api/v1/site/userdata/latest"},
		{name: "v2 envelope", path: "/api/v2/site/userdata/latest", wrapped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := performSiteUserDataRequest(router, tt.path, true)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			var data []map[string]any
			if tt.wrapped {
				var envelope struct {
					Success bool             `json:"success"`
					Message *string          `json:"message"`
					Data    []map[string]any `json:"data"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
					t.Fatalf("解析 V2 latest: %v; body=%s", err, response.Body.String())
				}
				if !envelope.Success || envelope.Message == nil || *envelope.Message != "" {
					t.Fatalf("V2 envelope = %#v", envelope)
				}
				data = envelope.Data
			} else if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
				t.Fatalf("解析 V1 latest: %v; body=%s", err, response.Body.String())
			}
			if len(data) != 1 {
				t.Fatalf("latest data = %#v", data)
			}
			if _, exists := data[0]["id"]; exists {
				t.Fatalf("latest 泄漏数据库 id: %#v", data[0])
			}
			if _, exists := data[0]["name"]; exists {
				t.Fatalf("latest 泄漏数据库 name: %#v", data[0])
			}
			if data[0]["domain"] != "tracker.example" || data[0]["username"] != "alice" ||
				data[0]["upload"] != float64(123) || data[0]["message_unread_contents"] != nil {
				t.Fatalf("latest 字段 = %#v", data[0])
			}
		})
	}
}

func TestSiteUserDataHistoryMatchesV2LookupAndWorkdateSemantics(t *testing.T) {
	store := &fakeSiteUserDataAPIStore{
		fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{7: {
			ID: 7, Domain: "tracker.example", Name: "Tracker", URL: "https://tracker.example",
		}}},
		history: []database.SiteUserData{{
			ID: 11, Domain: "tracker.example", Name: "Tracker", UpdatedDay: "2026-08-16", UpdatedTime: "12:00:00",
		}},
	}
	router := siteUserDataCompatibilityRouter(t, store, auth.User{ID: 1, SuperUser: true})

	response := performSiteUserDataRequest(router, "/api/v1/site/userdata/7?workdate=%20", true)
	if response.Code != http.StatusOK {
		t.Fatalf("history status = %d; body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Success     bool                    `json:"success"`
		Message     *string                 `json:"message"`
		MessageI18N *string                 `json:"message_i18n"`
		Data        []database.SiteUserData `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析 history: %v", err)
	}
	if !envelope.Success || len(envelope.Data) != 1 || envelope.Data[0].ID != 11 || envelope.Data[0].Name != "Tracker" {
		t.Fatalf("history envelope = %#v", envelope)
	}
	var rawEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &rawEnvelope); err != nil {
		t.Fatalf("解析 history 原始字段: %v", err)
	}
	if string(rawEnvelope["message"]) != "null" || string(rawEnvelope["message_i18n"]) != "null" {
		t.Fatalf("history 空消息字段 = %s", response.Body.String())
	}
	if store.queriedDomain != "tracker.example" || store.queriedWorkdate != " " {
		t.Fatalf("history query = domain %q workdate %q", store.queriedDomain, store.queriedWorkdate)
	}

	store.history = nil
	response = performSiteUserDataRequest(router, "/api/v2/site/userdata/7?workdate=invalid-date", true)
	if response.Code != http.StatusOK {
		t.Fatalf("empty history status = %d; body=%s", response.Code, response.Body.String())
	}
	var empty struct {
		Success bool  `json:"success"`
		Data    []any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &empty); err != nil {
		t.Fatalf("解析空 history: %v", err)
	}
	if empty.Success || empty.Data == nil || len(empty.Data) != 0 || store.queriedWorkdate != "invalid-date" {
		t.Fatalf("空 history = %#v, workdate=%q", empty, store.queriedWorkdate)
	}
}

func TestSiteUserDataHistoryRejectsInvalidOrMissingSiteLikeV2(t *testing.T) {
	store := &fakeSiteUserDataAPIStore{fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{}}}
	router := siteUserDataCompatibilityRouter(t, store, auth.User{ID: 1, SuperUser: true})

	tests := []struct {
		name   string
		path   string
		status int
		body   string
	}{
		{
			name: "non integer", path: "/api/v1/site/userdata/not-an-int",
			status: http.StatusUnprocessableEntity, body: "站点 ID 无效",
		},
		{name: "zero", path: "/api/v1/site/userdata/0", status: http.StatusNotFound, body: "站点 0 不存在"},
		{name: "negative", path: "/api/v1/site/userdata/-1", status: http.StatusNotFound, body: "站点 -1 不存在"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := performSiteUserDataRequest(router, tt.path, true)
			if response.Code != tt.status || !strings.Contains(response.Body.String(), tt.body) {
				t.Fatalf(
					"response = status %d body %s, want %d containing %q",
					response.Code, response.Body.String(), tt.status, tt.body,
				)
			}
		})
	}
}

func TestSiteUserDataManagePermissionUsesV2BadRequest(t *testing.T) {
	store := &fakeSiteUserDataAPIStore{fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{}}}
	router := siteUserDataCompatibilityRouter(t, store, auth.User{ID: 1, Permissions: map[string]any{}})
	response := performSiteUserDataRequest(router, "/api/v1/site/userdata/latest", true)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "用户权限不足") {
		t.Fatalf("permission response = status %d body %s", response.Code, response.Body.String())
	}
}

func TestSiteUserDataAuthenticationStatusesMatchV2(t *testing.T) {
	user := auth.User{ID: 1, Username: "plain-user", Permissions: map[string]any{}}
	authService := auth.NewService(nil, "site-userdata-jwt-secret", "site-userdata-resource-secret", time.Hour)
	token, err := authService.AccessToken(user)
	if err != nil {
		t.Fatalf("创建站点用户数据测试 token: %v", err)
	}
	store := &fakeSiteUserDataAPIStore{fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{}}}
	handler := NewRouter(Dependencies{
		Config: config.Config{MaxRequestBody: 1024, FrontendDir: t.TempDir()},
		Auth:   authService, Users: fakeUserLookup{user: user}, Sites: store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	tests := []struct {
		name   string
		token  string
		status int
	}{
		{name: "missing bearer", status: http.StatusUnauthorized},
		{name: "bad token", token: "invalid", status: http.StatusForbidden},
		{name: "authenticated without manage", token: token, status: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/site/userdata/latest", nil)
			if tt.token != "" {
				request.Header.Set("Authorization", "Bearer "+tt.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.status, response.Body.String())
			}
		})
	}
}

func TestSiteUserDataErrorsRemainStable(t *testing.T) {
	store := &fakeSiteUserDataAPIStore{
		fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{7: {ID: 7, Domain: "tracker.example"}}},
		latestErr:     errors.New("postgres password=secret"),
	}
	router := siteUserDataCompatibilityRouter(t, store, auth.User{ID: 1, SuperUser: true})
	response := performSiteUserDataRequest(router, "/api/v1/site/userdata/latest", true)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "查询站点用户数据失败") {
		t.Fatalf("latest error response = status %d body %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("latest error leaked internal details: %s", response.Body.String())
	}
}

func siteUserDataCompatibilityRouter(t *testing.T, store *fakeSiteUserDataAPIStore, user auth.User) http.Handler {
	t.Helper()
	deps := Dependencies{Sites: store, Users: fakeUserLookup{user: user}}
	router := chi.NewRouter()
	router.Route("/api/v1", deps.registerSiteCompatibility)
	router.Route("/api/v2", deps.registerSiteCompatibility)
	return router
}

func performSiteUserDataRequest(handler http.Handler, path string, withClaims bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if withClaims {
		request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, auth.Claims{
			SuperUser:        true,
			RegisteredClaims: jwt.RegisteredClaims{Subject: "1"},
		}))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSiteUserDataHistoryDoesNotNormalizeWorkdateEncoding(t *testing.T) {
	store := &fakeSiteUserDataAPIStore{
		fakeSiteStore: &fakeSiteStore{sites: map[int]database.Site{7: {ID: 7, Domain: "tracker.example"}}},
	}
	router := siteUserDataCompatibilityRouter(t, store, auth.User{ID: 1, SuperUser: true})
	workdate := " 2026-08-16 "
	response := performSiteUserDataRequest(router, "/api/v1/site/userdata/7?workdate="+url.QueryEscape(workdate), true)
	if response.Code != http.StatusOK || store.queriedWorkdate != workdate {
		t.Fatalf("workdate = %q, response status=%d body=%s", store.queriedWorkdate, response.Code, response.Body.String())
	}
}
