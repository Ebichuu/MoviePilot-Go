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

func TestNotificationSettingsGetPreservesV2ValueEnvelopeAndSecret(t *testing.T) {
	t.Parallel()
	const token = "123456:telegram-token-fixture"
	store := &notificationSettingStoreFake{
		found: true,
		value: json.RawMessage(`[{"name":"home","type":"telegram","config":{"TELEGRAM_TOKEN":"` + token + `","TELEGRAM_CHAT_ID":"7"},"switchs":["整理入库"],"enabled":true}]`),
	}
	handler, accessToken := newNotificationSettingTestRouter(t, store, nil)

	response := performNotificationRequest(handler, http.MethodGet, "/api/v1/system/setting/Notifications", "", accessToken)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Value []struct {
				Name   string         `json:"name"`
				Config map[string]any `json:"config"`
			} `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, response.Body.String())
	}
	if !payload.Success || len(payload.Data.Value) != 1 || payload.Data.Value[0].Name != "home" ||
		payload.Data.Value[0].Config["TELEGRAM_TOKEN"] != token {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestNotificationSettingsPostPreservesV2JSONSemantics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "empty body is null", body: "", want: "null"},
		{name: "null remains null", body: "null", want: "null"},
		{name: "empty list becomes null", body: "[]", want: "null"},
		{
			name: "falsy list items are removed",
			body: `[null,false,0,"",[],{},` +
				`{"name":"home","type":"telegram","config":{"TELEGRAM_TOKEN":"token","TELEGRAM_CHAT_ID":"7"},"enabled":true}]`,
			want: `[{"config":{"TELEGRAM_CHAT_ID":"7","TELEGRAM_TOKEN":"token"},"enabled":true,"name":"home","type":"telegram"}]`,
		},
		{name: "object is accepted", body: `{"legacy":true}`, want: `{"legacy":true}`},
		{name: "boolean is accepted", body: "true", want: "true"},
		{name: "integer is accepted", body: "7", want: "7"},
		{name: "string is accepted", body: `"telegram"`, want: `"telegram"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &notificationSettingStoreFake{}
			var reloadCalls int
			handler, accessToken := newNotificationSettingTestRouter(t, store, func(context.Context) error {
				reloadCalls++
				return nil
			})

			response := performNotificationRequest(handler, http.MethodPost, "/api/v1/system/setting/Notifications", tt.body, accessToken)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			if store.saveCalls != 1 || reloadCalls != 1 {
				t.Fatalf("save calls = %d, reload calls = %d", store.saveCalls, reloadCalls)
			}
			assertJSONEqual(t, store.saved, json.RawMessage(tt.want))
		})
	}
}

func TestNotificationSettingsReloadFailureKeepsV2Success(t *testing.T) {
	t.Parallel()
	store := &notificationSettingStoreFake{}
	handler, accessToken := newNotificationSettingTestRouter(t, store, func(context.Context) error {
		return errors.New("reload failed with 123456:telegram-token-fixture")
	})

	response := performNotificationRequest(handler, http.MethodPost, "/api/v1/system/setting/Notifications", "null", accessToken)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var payload Response
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Success || store.saveCalls != 1 {
		t.Fatalf("payload=%#v save calls=%d", payload, store.saveCalls)
	}
	if strings.Contains(response.Body.String(), "telegram-token-fixture") || strings.Contains(response.Body.String(), "reload failed") {
		t.Fatalf("reload error leaked to response: %s", response.Body.String())
	}
}

func TestNotificationSettingsRejectsMalformedOrMultipleJSON(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"broken":`},
		{name: "multiple values", body: `[] {}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &notificationSettingStoreFake{}
			handler, accessToken := newNotificationSettingTestRouter(t, store, nil)

			response := performNotificationRequest(handler, http.MethodPost, "/api/v1/system/setting/Notifications", tt.body, accessToken)

			if response.Code != http.StatusUnprocessableEntity || store.saveCalls != 0 {
				t.Fatalf("status=%d save calls=%d body=%s", response.Code, store.saveCalls, response.Body.String())
			}
		})
	}
}

type notificationSettingStoreFake struct {
	value     json.RawMessage
	found     bool
	loadErr   error
	saveErr   error
	saved     json.RawMessage
	saveCalls int
}

func (s *notificationSettingStoreFake) LoadNotificationSettings(context.Context) (json.RawMessage, bool, error) {
	return append(json.RawMessage(nil), s.value...), s.found, s.loadErr
}

func (s *notificationSettingStoreFake) SaveNotificationSettings(_ context.Context, value json.RawMessage) error {
	s.saveCalls++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(json.RawMessage(nil), value...)
	s.value = append(json.RawMessage(nil), value...)
	s.found = true
	return nil
}

func newNotificationSettingTestRouter(
	t *testing.T,
	store NotificationSettingStore,
	reload func(context.Context) error,
) (http.Handler, string) {
	t.Helper()
	authService := auth.NewService(nil, "notification-setting-jwt-secret", "notification-setting-resource-secret", time.Hour)
	user := auth.User{ID: 1, Username: "admin", SuperUser: true}
	accessToken, err := authService.AccessToken(user)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewRouter(Dependencies{
		Config:               config.Config{MaxRequestBody: 1 << 20, FrontendDir: t.TempDir()},
		NotificationSettings: store,
		ReloadNotifications:  reload,
		Users:                fakeUserLookup{user: user},
		Auth:                 authService,
		Logger:               logger,
	})
	return handler, accessToken
}

func performNotificationRequest(handler http.Handler, method, target, body, accessToken string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON: %v; value=%s", err, got)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want JSON: %v; value=%s", err, want)
	}
	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("JSON = %s, want %s", gotJSON, wantJSON)
	}
}
