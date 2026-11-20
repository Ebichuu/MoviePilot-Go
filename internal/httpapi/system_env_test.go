package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

func TestSystemEnvRoutesShareV1AndV2Handlers(t *testing.T) {
	settings := &fakeEnvironmentSettings{
		snapshot: map[string]any{
			"APP_DOMAIN":                    "https://example.invalid",
			"SECRET_KEY":                    "hidden-secret",
			"RESOURCE_SECRET_KEY":           "hidden-resource-secret",
			"MOVIEPILOT_JWT_SECRET":         "hidden-jwt-secret",
			"MOVIEPILOT_RESOURCE_SECRET":    "hidden-resource-secret",
			"MOVIEPILOT_MASTER_KEY":         "hidden-master-key",
			"MOVIEPILOT_BOOTSTRAP_PASSWORD": "hidden-password",
			"MOVIEPILOT_DATABASE_URL":       "hidden-database-url",
			"MOVIEPILOT_REDIS_URL":          "hidden-redis-url",
		},
		update: func(_ context.Context, env map[string]any) (map[string]config.EnvironmentUpdateResult, error) {
			if number, ok := env["LIMIT"].(json.Number); !ok || number.String() != "9007199254740993" {
				t.Fatalf("LIMIT = %#v, want preserved json.Number", env["LIMIT"])
			}
			return map[string]config.EnvironmentUpdateResult{
				"LIMIT": {Changed: true},
			}, nil
		},
	}
	handler, token := newSystemEnvTestRouter(t, settings, auth.User{ID: 1, Username: "admin", SuperUser: true}, 1<<20)

	for _, version := range []string{"v1", "v2"} {
		t.Run(version+" GET", func(t *testing.T) {
			response := performSystemEnvRequest(handler, http.MethodGet, "/api/"+version+"/system/env", "", token)
			payload := decodeSystemEnvResponse(t, response, http.StatusOK)
			if !payload.Success {
				t.Fatalf("payload = %#v", payload)
			}
			data := payload.Data.(map[string]any)
			if data["APP_DOMAIN"] != "https://example.invalid" || data["VERSION"] != backendVersion || data["FRONTEND_VERSION"] != frontendVersion {
				t.Fatalf("data = %#v", data)
			}
			if data["AUTH_VERSION"] != "" || data["INDEXER_VERSION"] != "" || data["RUST_ACCEL_AVAILABLE"] != false || data["RUST_ACCEL_ENABLED"] != false {
				t.Fatalf("compatibility fields = %#v", data)
			}
			for _, key := range []string{
				"SECRET_KEY",
				"RESOURCE_SECRET_KEY",
				"MOVIEPILOT_JWT_SECRET",
				"MOVIEPILOT_RESOURCE_SECRET",
				"MOVIEPILOT_MASTER_KEY",
				"MOVIEPILOT_BOOTSTRAP_PASSWORD",
				"MOVIEPILOT_DATABASE_URL",
				"MOVIEPILOT_REDIS_URL",
			} {
				if _, exists := data[key]; exists {
					t.Fatalf("response exposed %s", key)
				}
			}
		})

		t.Run(version+" POST", func(t *testing.T) {
			response := performSystemEnvRequest(handler, http.MethodPost, "/api/"+version+"/system/env", `{"LIMIT":9007199254740993}`, token)
			payload := decodeSystemEnvResponse(t, response, http.StatusOK)
			if !payload.Success || payload.Message != "所有配置项更新成功" {
				t.Fatalf("payload = %#v", payload)
			}
			updates := payload.Data.(map[string]any)["success_updates"].(map[string]any)
			pair := updates["LIMIT"].([]any)
			if pair[0] != true || pair[1] != nil {
				t.Fatalf("update pair = %#v", pair)
			}
		})
	}
	if _, exists := settings.snapshot["VERSION"]; exists {
		t.Fatal("GET mutated the settings snapshot")
	}
}

func TestSystemEnvRoutesRequireCurrentSuperuser(t *testing.T) {
	tests := []struct {
		name       string
		user       auth.User
		withToken  bool
		wantStatus int
	}{
		{name: "missing bearer", user: auth.User{ID: 1, SuperUser: true}, wantStatus: http.StatusUnauthorized},
		{name: "ordinary user", user: auth.User{ID: 1, Username: "alice"}, withToken: true, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		for _, version := range []string{"v1", "v2"} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				t.Run(tt.name+" "+version+" "+method, func(t *testing.T) {
					handler, token := newSystemEnvTestRouter(t, &fakeEnvironmentSettings{}, tt.user, 1<<20)
					if !tt.withToken {
						token = ""
					}
					body := ""
					if method == http.MethodPost {
						body = `{}`
					}
					response := performSystemEnvRequest(handler, method, "/api/"+version+"/system/env", body, token)
					if response.Code != tt.wantStatus {
						t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
					}
				})
			}
		}
	}
}

func TestSetSystemEnvRejectsInvalidBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty"},
		{name: "null", body: `null`},
		{name: "array", body: `[]`},
		{name: "string", body: `"value"`},
		{name: "number", body: `42`},
		{name: "boolean", body: `true`},
		{name: "malformed", body: `{"key":`},
		{name: "trailing value", body: `{} {}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &fakeEnvironmentSettings{}
			handler, token := newSystemEnvTestRouter(t, settings, auth.User{ID: 1, SuperUser: true}, 1<<20)
			response := performSystemEnvRequest(handler, http.MethodPost, "/api/v1/system/env", tt.body, token)
			payload := decodeSystemEnvResponse(t, response, http.StatusUnprocessableEntity)
			if payload.Success || payload.Message != errSystemEnvRequestInvalid.Error() {
				t.Fatalf("payload = %#v", payload)
			}
			if settings.updateCalls != 0 {
				t.Fatalf("Update calls = %d, want 0", settings.updateCalls)
			}
		})
	}
}

func TestSetSystemEnvRejectsOversizedBody(t *testing.T) {
	settings := &fakeEnvironmentSettings{}
	handler, token := newSystemEnvTestRouter(t, settings, auth.User{ID: 1, SuperUser: true}, 8)
	response := performSystemEnvRequest(handler, http.MethodPost, "/api/v1/system/env", `{"key":"value"}`, token)
	payload := decodeSystemEnvResponse(t, response, http.StatusRequestEntityTooLarge)
	if payload.Success || payload.Message != errSystemEnvRequestTooLarge.Error() || settings.updateCalls != 0 {
		t.Fatalf("payload=%#v updateCalls=%d", payload, settings.updateCalls)
	}
}

func TestSetSystemEnvMapsUpdateStates(t *testing.T) {
	tests := []struct {
		name          string
		results       map[string]config.EnvironmentUpdateResult
		wantSuccess   bool
		wantMessage   string
		wantSuccesses map[string][]any
		wantFailures  map[string][]any
	}{
		{name: "empty", results: map[string]config.EnvironmentUpdateResult{}, wantSuccess: true, wantMessage: "所有配置项更新成功", wantSuccesses: map[string][]any{}},
		{name: "changed", results: map[string]config.EnvironmentUpdateResult{"A": {Changed: true, Message: "normalized"}}, wantSuccess: true, wantMessage: "所有配置项更新成功", wantSuccesses: map[string][]any{"A": {true, "normalized"}}},
		{name: "no-op", results: map[string]config.EnvironmentUpdateResult{"A": {}}, wantSuccess: true, wantMessage: "所有配置项更新成功", wantSuccesses: map[string][]any{}},
		{name: "failed", results: map[string]config.EnvironmentUpdateResult{"A": {Failed: true, Message: "invalid"}}, wantMessage: "invalid", wantSuccesses: map[string][]any{}, wantFailures: map[string][]any{"A": {false, "invalid"}}},
		{name: "failed without message", results: map[string]config.EnvironmentUpdateResult{"A": {Failed: true}}, wantSuccesses: map[string][]any{}, wantFailures: map[string][]any{"A": {false, nil}}},
		{
			name: "mixed",
			results: map[string]config.EnvironmentUpdateResult{
				"changed": {Changed: true},
				"noop":    {},
				"z-error": {Failed: true, Message: "second"},
				"a-error": {Failed: true, Message: "first"},
			},
			wantMessage:   "first, second",
			wantSuccesses: map[string][]any{"changed": {true, nil}},
			wantFailures:  map[string][]any{"a-error": {false, "first"}, "z-error": {false, "second"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &fakeEnvironmentSettings{results: tt.results}
			handler, token := newSystemEnvTestRouter(t, settings, auth.User{ID: 1, SuperUser: true}, 1<<20)
			response := performSystemEnvRequest(handler, http.MethodPost, "/api/v1/system/env", `{}`, token)
			payload := decodeSystemEnvResponse(t, response, http.StatusOK)
			if payload.Success != tt.wantSuccess || payload.Message != tt.wantMessage {
				t.Fatalf("payload = %#v", payload)
			}
			data := payload.Data.(map[string]any)
			assertSystemEnvUpdateMap(t, data["success_updates"], tt.wantSuccesses)
			if tt.wantFailures == nil {
				if _, exists := data["failed_updates"]; exists {
					t.Fatalf("unexpected failed_updates: %#v", data["failed_updates"])
				}
			} else {
				assertSystemEnvUpdateMap(t, data["failed_updates"], tt.wantFailures)
			}
		})
	}
}

func TestSystemEnvServiceFailuresAreStableAndRedacted(t *testing.T) {
	secret := "sensitive backend failure"
	tests := []struct {
		name     string
		settings EnvironmentSettings
		method   string
		body     string
		message  string
	}{
		{name: "missing read service", method: http.MethodGet, message: "读取系统配置失败"},
		{name: "missing update service", method: http.MethodPost, body: `{}`, message: "更新系统配置失败"},
		{name: "update error", settings: &fakeEnvironmentSettings{updateErr: errors.New(secret)}, method: http.MethodPost, body: `{}`, message: "更新系统配置失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, token := newSystemEnvTestRouter(t, tt.settings, auth.User{ID: 1, SuperUser: true}, 1<<20)
			response := performSystemEnvRequest(handler, tt.method, "/api/v1/system/env", tt.body, token)
			payload := decodeSystemEnvResponse(t, response, http.StatusInternalServerError)
			if payload.Success || payload.Message != tt.message {
				t.Fatalf("payload = %#v", payload)
			}
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("response leaked service error: %s", response.Body.String())
			}
		})
	}
}

type fakeEnvironmentSettings struct {
	snapshot    map[string]any
	results     map[string]config.EnvironmentUpdateResult
	update      func(context.Context, map[string]any) (map[string]config.EnvironmentUpdateResult, error)
	updateErr   error
	updateCalls int
}

func (s *fakeEnvironmentSettings) Snapshot() map[string]any {
	return s.snapshot
}

func (s *fakeEnvironmentSettings) Update(ctx context.Context, env map[string]any) (map[string]config.EnvironmentUpdateResult, error) {
	s.updateCalls++
	if s.update != nil {
		return s.update(ctx, env)
	}
	return s.results, s.updateErr
}

func newSystemEnvTestRouter(t *testing.T, settings EnvironmentSettings, user auth.User, maxRequestBody int64) (http.Handler, string) {
	t.Helper()
	authService := auth.NewService(nil, "system-env-test-jwt-secret", "system-env-test-resource-secret", time.Hour)
	token, err := authService.AccessToken(user)
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRouter(Dependencies{
		Config:              config.Config{MaxRequestBody: maxRequestBody, FrontendDir: t.TempDir()},
		EnvironmentSettings: settings,
		Users:               fakeUserLookup{user: user},
		Auth:                authService,
		Logger:              logger,
	}), token
}

func performSystemEnvRequest(handler http.Handler, method, target, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type decodedSystemEnvResponse struct {
	Success bool
	Message string
	Data    any
}

func decodeSystemEnvResponse(t *testing.T, response *httptest.ResponseRecorder, wantStatus int) decodedSystemEnvResponse {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
	}
	var payload decodedSystemEnvResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, response.Body.String())
	}
	return payload
}

func assertSystemEnvUpdateMap(t *testing.T, value any, want map[string][]any) {
	t.Helper()
	got, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("update map = %#v", value)
	}
	if len(got) != len(want) {
		t.Fatalf("update map = %#v, want %#v", got, want)
	}
	for key, wantPair := range want {
		gotPair, ok := got[key].([]any)
		if !ok || len(gotPair) != len(wantPair) {
			t.Fatalf("update pair %s = %#v, want %#v", key, got[key], wantPair)
		}
		for index := range wantPair {
			if gotPair[index] != wantPair[index] {
				t.Fatalf("update pair %s = %#v, want %#v", key, gotPair, wantPair)
			}
		}
	}
}
