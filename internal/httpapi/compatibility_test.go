package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

func TestPasskeyAuthenticationUnavailableUsesV2Response(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mfa/passkey/authenticate/start", strings.NewReader(`{}`))

	passkeyAuthenticationUnavailable(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var payload Response
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if payload.Success || payload.Message == "" {
		t.Fatalf("payload = %#v, want explicit unsupported response", payload)
	}
}

func TestCompatibilityEmptyCollectionsRemainRawArrays(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "通知历史", handler: emptyList},
		{name: "插件仪表板", handler: emptyList},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			tt.handler(response, httptest.NewRequest(http.MethodGet, "/", nil))
			if got := strings.TrimSpace(response.Body.String()); got != "[]" {
				t.Fatalf("body = %q, want []", got)
			}
		})
	}
}

func TestSystemMessageStartsSSEAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	response := &cancelOnFlushRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/system/message", nil).WithContext(ctx)

	Dependencies{}.systemMessage(response, request)

	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := response.Body.String(); got != "data: \n\n" {
		t.Fatalf("body = %q, want initial empty SSE event", got)
	}
}

func TestUserConfigRoundTripIsScopedByAuthenticatedUser(t *testing.T) {
	store := &memoryUserConfigStore{values: make(map[string]any)}
	deps := Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}, UserConfigs: store}

	for _, userID := range []string{"7", "8"} {
		request := authenticatedRequest(http.MethodPost, "/api/v1/user/config/SubscribeMovieOrder", `[1,2]`, userID)
		request = withRouteParam(request, "key", "SubscribeMovieOrder")
		response := httptest.NewRecorder()
		deps.setUserConfig(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("set user %s status = %d; body=%s", userID, response.Code, response.Body.String())
		}
	}
	if len(store.values) != 2 {
		t.Fatalf("stored keys = %d, want 2 user-scoped keys", len(store.values))
	}

	request := authenticatedRequest(http.MethodGet, "/api/v1/user/config/SubscribeMovieOrder", "", "7")
	request = withRouteParam(request, "key", "SubscribeMovieOrder")
	response := httptest.NewRecorder()
	deps.getUserConfig(response, request)
	var payload Response
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	data, ok := payload.Data.(map[string]any)
	if !ok || data["value"] == nil {
		t.Fatalf("payload = %#v, want persisted value", payload)
	}
}

func TestSetUserConfigRejectsInvalidJSONWithoutWriting(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "非法 JSON", body: `{"broken":`},
		{name: "多个 JSON 值", body: `[] {}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryUserConfigStore{values: make(map[string]any)}
			request := authenticatedRequest(http.MethodPost, "/api/v1/user/config/key", tt.body, "7")
			request = withRouteParam(request, "key", "key")
			response := httptest.NewRecorder()
			Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}, UserConfigs: store}.setUserConfig(response, request)
			if response.Code != http.StatusBadRequest || len(store.values) != 0 {
				t.Fatalf("status=%d writes=%d body=%s", response.Code, len(store.values), response.Body.String())
			}
		})
	}
}

func TestSetUserConfigTreatsEmptyBodyAsNull(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "空请求体"},
		{name: "仅空白字符", body: " \r\n\t"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &memoryUserConfigStore{values: make(map[string]any)}
			request := authenticatedRequest(http.MethodPost, "/api/v1/user/config/key", tt.body, "7")
			request = withRouteParam(request, "key", "key")
			response := httptest.NewRecorder()

			Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}, UserConfigs: store}.setUserConfig(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			if len(store.values) != 1 {
				t.Fatalf("writes = %d, want 1", len(store.values))
			}
			for _, value := range store.values {
				if value != nil {
					t.Fatalf("stored value = %#v, want nil", value)
				}
			}
		})
	}
}

func TestUserConfigRejectsMissingStoreAndOversizedBody(t *testing.T) {
	tests := []struct {
		name   string
		deps   Dependencies
		body   string
		status int
	}{
		{
			name:   "未装配持久化存储",
			deps:   Dependencies{Config: config.Config{MaxRequestBody: 16}},
			body:   `[]`,
			status: http.StatusServiceUnavailable,
		},
		{
			name: "请求体超过硬上限",
			deps: Dependencies{
				Config:      config.Config{MaxRequestBody: 2},
				UserConfigs: &memoryUserConfigStore{values: make(map[string]any)},
			},
			body:   `[1]`,
			status: http.StatusRequestEntityTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := authenticatedRequest(http.MethodPost, "/api/v1/user/config/key", tt.body, "7")
			request = withRouteParam(request, "key", "key")
			response := httptest.NewRecorder()
			tt.deps.setUserConfig(response, request)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.status, response.Body.String())
			}
		})
	}
}

func TestRouterNeverServesSPAForAPIPaths(t *testing.T) {
	frontend := t.TempDir()
	if err := writeTestFrontend(frontend, "<html>spa</html>"); err != nil {
		t.Fatal(err)
	}
	handler := NewRouter(Dependencies{Config: config.Config{FrontendDir: frontend, MaxRequestBody: 1 << 20}})

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "未知 V1 接口", method: http.MethodGet, path: "/api/v1/not-planned", status: http.StatusNotFound},
		{name: "未知 V2 接口", method: http.MethodGet, path: "/api/v2/not-planned", status: http.StatusNotFound},
		{name: "API 根", method: http.MethodGet, path: "/api/unknown", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tt.method, tt.path, nil))
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.status, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json; body=%s", got, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "<html") {
				t.Fatalf("API response leaked SPA HTML: %s", response.Body.String())
			}
		})
	}
}

func TestSystemMessageRouteRequiresResourceToken(t *testing.T) {
	authService := auth.NewService(nil, "compat-jwt-secret", "compat-resource-secret", time.Hour)
	handler := NewRouter(Dependencies{
		Config: config.Config{MaxRequestBody: 1 << 20},
		Auth:   authService,
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/system/message", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusForbidden, response.Body.String())
	}
}

func TestSystemMessageRouteAcceptsResourceCookieAndStartsSSE(t *testing.T) {
	authService := auth.NewService(nil, "compat-jwt-secret", "compat-resource-secret", time.Hour)
	resourceToken, err := authService.ResourceToken(auth.User{ID: 7, Username: "alice"}, time.Minute)
	if err != nil {
		t.Fatalf("ResourceToken() error = %v", err)
	}
	handler := NewRouter(Dependencies{
		Config: config.Config{MaxRequestBody: 1 << 20},
		Auth:   authService,
	})
	ctx, cancel := context.WithCancel(context.Background())
	response := &cancelOnFlushRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/system/message?role=notification", nil).WithContext(ctx)
	request.AddCookie(&http.Cookie{Name: "MoviePilot", Value: resourceToken})

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if got := response.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	if got := response.Body.String(); got != "data: \n\n" {
		t.Fatalf("body = %q, want initial empty SSE event", got)
	}
}

func TestNotificationRoutesRequireBearerAndPreserveV2Shapes(t *testing.T) {
	authService := auth.NewService(nil, "compat-jwt-secret", "compat-resource-secret", time.Hour)
	token := accessTokenForTest(t, authService, auth.User{ID: 7, Username: "alice"})
	handler := NewRouter(Dependencies{
		Config:              config.Config{MaxRequestBody: 1 << 20, Location: time.UTC},
		Auth:                authService,
		NotificationHistory: &notificationHistoryStoreFake{},
	})

	tests := []struct {
		name       string
		method     string
		authorized bool
		wantStatus int
		wantBody   string
	}{
		{name: "list rejects missing bearer", method: http.MethodGet, wantStatus: http.StatusUnauthorized},
		{name: "delete rejects missing bearer", method: http.MethodDelete, wantStatus: http.StatusUnauthorized},
		{name: "list returns raw array", method: http.MethodGet, authorized: true, wantStatus: http.StatusOK, wantBody: "[]"},
		{name: "delete returns success envelope", method: http.MethodDelete, authorized: true, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, "/api/v1/message/notification?scope=all&page=1&count=20", nil)
			if tt.authorized {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json; body=%s", got, response.Body.String())
			}
			if tt.wantBody != "" && strings.TrimSpace(response.Body.String()) != tt.wantBody {
				t.Fatalf("body = %q, want %s", response.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestPluginDashboardMetaRequiresCurrentAdminAndReturnsRawArray(t *testing.T) {
	authService := auth.NewService(nil, "compat-jwt-secret", "compat-resource-secret", time.Hour)
	token := accessTokenForTest(t, authService, auth.User{ID: 7, Username: "alice", SuperUser: true})

	tests := []struct {
		name        string
		currentUser auth.User
		wantStatus  int
		wantBody    string
	}{
		{name: "current non-admin is forbidden", currentUser: auth.User{ID: 7, Username: "alice"}, wantStatus: http.StatusForbidden},
		{name: "current admin receives raw array", currentUser: auth.User{ID: 7, Username: "alice", SuperUser: true}, wantStatus: http.StatusOK, wantBody: "[]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewRouter(Dependencies{
				Config: config.Config{MaxRequestBody: 1 << 20},
				Auth:   authService,
				Users:  fakeUserLookup{user: tt.currentUser},
			})
			request := httptest.NewRequest(http.MethodGet, "/api/v1/plugin/dashboard/meta", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json; body=%s", got, response.Body.String())
			}
			if tt.wantBody != "" && strings.TrimSpace(response.Body.String()) != tt.wantBody {
				t.Fatalf("body = %q, want %s", response.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestUserConfigRoutesRequireBearerAndPreserveValueEnvelope(t *testing.T) {
	authService := auth.NewService(nil, "compat-jwt-secret", "compat-resource-secret", time.Hour)
	token := accessTokenForTest(t, authService, auth.User{ID: 7, Username: "alice"})
	store := &memoryUserConfigStore{values: make(map[string]any)}
	handler := NewRouter(Dependencies{
		Config:      config.Config{MaxRequestBody: 1 << 20},
		Auth:        authService,
		UserConfigs: store,
	})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/user/config/Dashboard", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if got := unauthorized.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("missing bearer Content-Type = %q, want application/json", got)
	}

	post := httptest.NewRequest(http.MethodPost, "/api/v1/user/config/Dashboard", strings.NewReader(`{"layout":[1,2],"compact":true}`))
	post.Header.Set("Authorization", "Bearer "+token)
	post.Header.Set("Content-Type", "application/json")
	postResponse := httptest.NewRecorder()
	handler.ServeHTTP(postResponse, post)
	if postResponse.Code != http.StatusOK {
		t.Fatalf("post status = %d, want %d; body=%s", postResponse.Code, http.StatusOK, postResponse.Body.String())
	}
	if got := postResponse.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("post Content-Type = %q, want application/json", got)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/v1/user/config/Dashboard", nil)
	get.Header.Set("Authorization", "Bearer "+token)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body=%s", getResponse.Code, http.StatusOK, getResponse.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Value map[string]any `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(getResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if !payload.Success || payload.Data.Value["compact"] != true {
		t.Fatalf("payload = %#v, want data.value with persisted object", payload)
	}
}

func TestUnknownAPIMethodsNeverServeSPA(t *testing.T) {
	frontend := t.TempDir()
	if err := writeTestFrontend(frontend, "<html>spa</html>"); err != nil {
		t.Fatal(err)
	}
	handler := NewRouter(Dependencies{Config: config.Config{FrontendDir: frontend, MaxRequestBody: 1 << 20}})

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, "/api/v1/not-planned", nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusNotFound, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json; body=%s", got, response.Body.String())
			}
			if strings.Contains(strings.ToLower(response.Body.String()), "<html") {
				t.Fatalf("API response leaked SPA HTML: %s", response.Body.String())
			}
		})
	}
}

func TestKnownAPIWrongMethodReturnsJSON(t *testing.T) {
	handler := NewRouter(Dependencies{Config: config.Config{MaxRequestBody: 1 << 20}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/v1/mfa/passkey/authenticate/start", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusMethodNotAllowed, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json; body=%s", got, response.Body.String())
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "<html") {
		t.Fatalf("API response leaked SPA HTML: %s", response.Body.String())
	}
}

func accessTokenForTest(t *testing.T, service *auth.Service, user auth.User) string {
	t.Helper()
	token, err := service.AccessToken(user)
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	return token
}

func authenticatedRequest(method, path, body, subject string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	claims := auth.Claims{Username: "alice"}
	claims.Subject = subject
	return request.WithContext(context.WithValue(request.Context(), claimsContextKey, claims))
}

type cancelOnFlushRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (r *cancelOnFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.cancel()
}

type memoryUserConfigStore struct {
	values map[string]any
}

func (s *memoryUserConfigStore) GetSetting(_ context.Context, key string, target any) (bool, error) {
	value, ok := s.values[key]
	if !ok {
		return false, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(encoded, target)
}

func (s *memoryUserConfigStore) SetSetting(_ context.Context, key string, value any, _ bool) error {
	s.values[key] = value
	return nil
}

func writeTestFrontend(root, content string) error {
	return os.WriteFile(filepath.Join(root, "index.html"), []byte(content), 0o600)
}
