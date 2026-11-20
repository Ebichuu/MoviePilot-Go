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

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
)

const (
	downloaderTestIDOne = "b7088afb-12ab-4a76-82b7-2f478428ec85"
	downloaderTestIDTwo = "067ca317-1e34-48d7-a726-f7749f0a4db1"
)

func TestDownloadersSettingRequiresAdministrator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		body      string
		superUser bool
		useToken  bool
		want      int
	}{
		{name: "普通用户不能读取", method: http.MethodGet, useToken: true, want: http.StatusForbidden},
		{name: "普通用户不能写入", method: http.MethodPost, body: `[]`, useToken: true, want: http.StatusForbidden},
		{name: "未认证用户不能读取", method: http.MethodGet, want: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeDownloaderSettingStore{}
			user := auth.User{ID: 1, Username: "alice", SuperUser: tt.superUser}
			handler, token := newDownloaderTestRouter(t, store, user)
			if !tt.useToken {
				token = ""
			}
			response := performDownloaderRequest(handler, tt.method, "/api/v1/system/setting/Downloaders", tt.body, token)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.want, response.Body.String())
			}
			if store.loadCalls != 0 || store.saveCalls != 0 {
				t.Fatalf("未授权请求访问了存储: load=%d save=%d", store.loadCalls, store.saveCalls)
			}
		})
	}
}

func TestGetDownloadersSettingContractAndRedaction(t *testing.T) {
	t.Parallel()

	store := &fakeDownloaderSettingStore{
		value:     json.RawMessage(`[{"id":"` + downloaderTestIDOne + `","name":"qb","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"https://qb.example/","username":"admin","password":"password-secret","apikey":"apikey-secret"}}]`),
		encrypted: true,
		found:     true,
	}
	handler, token := newDownloaderTestRouter(t, store, auth.User{ID: 1, Username: "admin", SuperUser: true})
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/system/setting/Downloaders", "", token)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"password-secret", "apikey-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("GET 响应泄漏秘密 %q: %s", secret, response.Body.String())
		}
	}

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Value []downloaderSetting `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 GET 响应: %v", err)
	}
	if !payload.Success || len(payload.Data.Value) != 1 {
		t.Fatalf("GET 响应契约错误: %#v", payload)
	}
	config := payload.Data.Value[0].Config
	if config["password"] != "" || config["apikey"] != "" {
		t.Fatalf("秘密字段未清空: %#v", config)
	}
	if config["has_password"] != true || config["has_apikey"] != true {
		t.Fatalf("秘密存在标志错误: %#v", config)
	}
	if store.saveCalls != 0 {
		t.Fatalf("已加密且规范的 GET 意外写回 %d 次", store.saveCalls)
	}
}

func TestGetDownloadersSettingUpgradesLegacyValue(t *testing.T) {
	t.Parallel()

	store := &fakeDownloaderSettingStore{
		value:     json.RawMessage(`[{"name":" qb ","type":"QBITTORRENT","default":false,"enabled":true,"config":{"host":" https://qb.example/ ","password":"password-secret","apikey":"apikey-secret","has_password":true}}]`),
		encrypted: false,
		found:     true,
	}
	handler, token := newDownloaderTestRouter(t, store, auth.User{ID: 1, Username: "admin", SuperUser: true})
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/system/setting/Downloaders", "", token)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if store.saveCalls != 1 {
		t.Fatalf("旧设置写回次数 = %d, want 1", store.saveCalls)
	}

	saved, err := decodeDownloaderSettings(store.saved)
	if err != nil {
		t.Fatalf("解析升级后的设置: %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("升级后的设置数量 = %d", len(saved))
	}
	if _, err := uuid.Parse(saved[0].ID); err != nil {
		t.Fatalf("升级后 ID 无效: %q", saved[0].ID)
	}
	if saved[0].Name != "qb" || saved[0].Type != "qbittorrent" || !saved[0].Default {
		t.Fatalf("旧设置未规范化: %#v", saved[0])
	}
	if saved[0].Config["password"] != "password-secret" || saved[0].Config["apikey"] != "apikey-secret" {
		t.Fatalf("升级时丢失秘密: %#v", saved[0].Config)
	}
	if _, exists := saved[0].Config["has_password"]; exists {
		t.Fatalf("派生秘密标志被持久化: %#v", saved[0].Config)
	}
	if strings.Contains(response.Body.String(), "password-secret") || strings.Contains(response.Body.String(), "apikey-secret") {
		t.Fatalf("升级 GET 响应泄漏秘密: %s", response.Body.String())
	}
}

func TestSetDownloadersSettingSecretPatchSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		body         string
		wantPassword string
		wantAPIKey   string
	}{
		{
			name:         "空字段保留旧秘密并允许改名",
			body:         `[{"id":"` + downloaderTestIDOne + `","name":"qb-renamed","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"https://qb.example/","password":"","apikey":"","has_password":true,"has_apikey":true}}]`,
			wantPassword: "old-password",
			wantAPIKey:   "old-apikey",
		},
		{
			name:         "非空字段替换秘密",
			body:         `[{"id":"` + downloaderTestIDOne + `","name":"qb","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"https://qb.example/","password":"new-password","apikey":"new-apikey"}}]`,
			wantPassword: "new-password",
			wantAPIKey:   "new-apikey",
		},
		{
			name:         "显式标志清除秘密",
			body:         `[{"id":"` + downloaderTestIDOne + `","name":"qb","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"https://qb.example/","password":"ignored-password","apikey":"ignored-apikey","clear_password":true,"clear_apikey":true}}]`,
			wantPassword: "",
			wantAPIKey:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeDownloaderSettingStore{
				value:     json.RawMessage(`[{"id":"` + downloaderTestIDOne + `","name":"qb","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"https://qb.example/","password":"old-password","apikey":"old-apikey"}}]`),
				encrypted: true,
				found:     true,
			}
			handler, token := newDownloaderTestRouter(t, store, auth.User{ID: 1, Username: "admin", SuperUser: true})
			response := performDownloaderRequest(handler, http.MethodPost, "/api/v1/system/setting/Downloaders", tt.body, token)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
			}
			if store.saveCalls != 1 {
				t.Fatalf("保存次数 = %d, want 1", store.saveCalls)
			}
			saved, err := decodeDownloaderSettings(store.saved)
			if err != nil {
				t.Fatalf("解析保存值: %v", err)
			}
			if got := saved[0].Config["password"]; got != tt.wantPassword {
				t.Fatalf("password = %#v, want %q", got, tt.wantPassword)
			}
			if got := saved[0].Config["apikey"]; got != tt.wantAPIKey {
				t.Fatalf("apikey = %#v, want %q", got, tt.wantAPIKey)
			}
			for _, key := range []string{"has_password", "has_apikey", "clear_password", "clear_apikey"} {
				if _, exists := saved[0].Config[key]; exists {
					t.Fatalf("写入了临时字段 %q: %#v", key, saved[0].Config)
				}
			}
			for _, secret := range []string{"new-password", "new-apikey", "ignored-password", "ignored-apikey"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatalf("POST 响应泄漏秘密 %q: %s", secret, response.Body.String())
				}
			}
		})
	}
}

func TestSetDownloadersSettingReloadsRuntimeAfterSave(t *testing.T) {
	t.Parallel()
	store := &fakeDownloaderSettingStore{}
	var reloadCalls int
	handler, token := newDownloaderTestRouterWithReload(t, store, auth.User{ID: 1, Username: "admin", SuperUser: true}, func(ctx context.Context) error {
		reloadCalls++
		if ctx.Err() != nil {
			t.Fatalf("重载回调收到已取消上下文: %v", ctx.Err())
		}
		return nil
	})
	response := performDownloaderRequest(handler, http.MethodPost, "/api/v1/system/setting/Downloaders", `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{}}]`, token)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if reloadCalls != 1 {
		t.Fatalf("重载次数 = %d, want 1", reloadCalls)
	}
}

func TestSetDownloadersSettingReloadFailureKeepsPersistedConfig(t *testing.T) {
	t.Parallel()
	store := &fakeDownloaderSettingStore{}
	handler, token := newDownloaderTestRouterWithReload(t, store, auth.User{ID: 1, Username: "admin", SuperUser: true}, func(context.Context) error {
		return errors.New("qB password-secret unavailable")
	})
	response := performDownloaderRequest(handler, http.MethodPost, "/api/v1/system/setting/Downloaders", `[{"name":"qb","type":"qbittorrent","enabled":false,"config":{}}]`, token)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", response.Code, response.Body.String())
	}
	if store.saveCalls != 1 {
		t.Fatalf("保存次数 = %d, want 1", store.saveCalls)
	}
	if strings.Contains(response.Body.String(), "password-secret") || strings.Contains(response.Body.String(), "unavailable") {
		t.Fatalf("重载错误泄漏底层信息: %s", response.Body.String())
	}
	var payload Response
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if payload.Success {
		t.Fatal("重载失败响应 success=true")
	}
	data, ok := payload.Data.(map[string]any)
	if !ok || data["reload"] != false {
		t.Fatalf("重载失败 data.reload 错误: %#v", payload.Data)
	}
}

func TestSetDownloadersSettingRejectsInvalidPayloadWithoutEcho(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		secret string
	}{
		{
			name:   "拒绝 value 包装对象",
			body:   `{"value":[{"name":"qb","type":"qbittorrent","enabled":true,"config":{"host":"https://qb.example/","password":"wrapper-password-secret"}}]}`,
			secret: "wrapper-password-secret",
		},
		{
			name:   "拒绝非 HTTP 下载器地址",
			body:   `[{"name":"qb","type":"qbittorrent","enabled":true,"config":{"host":"file:///tmp/qb.sock","password":"host-password-secret"}}]`,
			secret: "host-password-secret",
		},
		{
			name:   "拒绝 URL 内嵌凭据",
			body:   `[{"name":"qb","type":"qbittorrent","enabled":true,"config":{"host":"https://user:host-url-secret@qb.example/","password":"body-password-secret"}}]`,
			secret: "body-password-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeDownloaderSettingStore{}
			handler, token := newDownloaderTestRouter(t, store, auth.User{ID: 1, Username: "admin", SuperUser: true})
			response := performDownloaderRequest(handler, http.MethodPost, "/api/v1/system/setting/Downloaders", tt.body, token)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), tt.secret) || strings.Contains(response.Body.String(), "host-url-secret") {
				t.Fatalf("错误响应泄漏请求秘密: %s", response.Body.String())
			}
			if store.saveCalls != 0 {
				t.Fatalf("非法请求被保存 %d 次", store.saveCalls)
			}
		})
	}
}

func TestDownloadClientsReturnsEnabledSafeSummary(t *testing.T) {
	t.Parallel()

	store := &fakeDownloaderSettingStore{
		value: json.RawMessage(`[
			{"id":"` + downloaderTestIDOne + `","name":"qb","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"https://qb.example/","password":"password-secret","apikey":"apikey-secret"}},
			{"id":"` + downloaderTestIDTwo + `","name":"disabled","type":"qbittorrent","default":false,"enabled":false,"config":{}}
		]`),
		encrypted: true,
		found:     true,
	}
	handler, token := newDownloaderTestRouter(t, store, auth.User{ID: 2, Username: "alice"})
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/download/clients", "", token)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	for _, forbidden := range []string{"success", "default", "enabled", "host", "password-secret", "apikey-secret"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("客户端下载摘要包含 %q: %s", forbidden, response.Body.String())
		}
	}
	var clients []downloaderClient
	if err := json.Unmarshal(response.Body.Bytes(), &clients); err != nil {
		t.Fatalf("客户端下载摘要不是原始数组: %v", err)
	}
	if len(clients) != 1 || clients[0] != (downloaderClient{Name: "qb", Type: "qbittorrent"}) {
		t.Fatalf("clients = %#v", clients)
	}
}

func TestDownloaderSettingErrorsAreRedacted(t *testing.T) {
	t.Parallel()

	store := &fakeDownloaderSettingStore{loadErr: errors.New("database failed with password-secret and apikey-secret")}
	handler, token := newDownloaderTestRouter(t, store, auth.User{ID: 1, Username: "admin", SuperUser: true})
	response := performDownloaderRequest(handler, http.MethodGet, "/api/v1/system/setting/Downloaders", "", token)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"password-secret", "apikey-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("错误响应泄漏秘密 %q: %s", secret, response.Body.String())
		}
	}
}

func TestNormalizeDownloaderSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   json.RawMessage
		wantErr bool
		verify  func(*testing.T, []downloaderSetting)
	}{
		{
			name:  "补全 ID 并选择首个启用项为默认",
			value: json.RawMessage(`[{"name":" qb ","type":"QBITTORRENT","enabled":true,"config":{"host":" https://qb.example/ "}},{"id":"` + downloaderTestIDTwo + `","name":"other","type":"transmission","enabled":true,"config":{"host":"http://transmission.example/"}}]`),
			verify: func(t *testing.T, settings []downloaderSetting) {
				t.Helper()
				if _, err := uuid.Parse(settings[0].ID); err != nil {
					t.Fatalf("生成 ID 无效: %q", settings[0].ID)
				}
				if settings[0].Name != "qb" || settings[0].Type != "qbittorrent" || settings[0].Config["host"] != "https://qb.example/" {
					t.Fatalf("字符串未规范化: %#v", settings[0])
				}
				if !settings[0].Default || settings[1].Default {
					t.Fatalf("默认项归一错误: %#v", settings)
				}
			},
		},
		{name: "拒绝重复 ID", value: json.RawMessage(`[{"id":"` + downloaderTestIDOne + `","name":"one","type":"qbittorrent","config":{}},{"id":"` + downloaderTestIDOne + `","name":"two","type":"qbittorrent","config":{}}]`), wantErr: true},
		{name: "拒绝大小写等价名称", value: json.RawMessage(`[{"id":"` + downloaderTestIDOne + `","name":"QB","type":"qbittorrent","config":{}},{"id":"` + downloaderTestIDTwo + `","name":"qb","type":"qbittorrent","config":{}}]`), wantErr: true},
		{name: "拒绝无协议地址", value: json.RawMessage(`[{"id":"` + downloaderTestIDOne + `","name":"qb","type":"qbittorrent","enabled":true,"config":{"host":"qb.example:8080"}}]`), wantErr: true},
		{name: "允许禁用项暂不填写地址", value: json.RawMessage(`[{"id":"` + downloaderTestIDOne + `","name":"qb","type":"qbittorrent","enabled":false,"config":{}}]`)},
		{name: "多个默认项保留首个", value: json.RawMessage(`[{"id":"` + downloaderTestIDOne + `","name":"one","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"http://one.example"}},{"id":"` + downloaderTestIDTwo + `","name":"two","type":"qbittorrent","default":true,"enabled":true,"config":{"host":"http://two.example"}}]`), verify: func(t *testing.T, settings []downloaderSetting) {
			t.Helper()
			if !settings[0].Default || settings[1].Default {
				t.Fatalf("默认项归一错误: %#v", settings)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			settings, err := decodeDownloaderSettings(tt.value)
			if err == nil {
				_, err = normalizeDownloaderSettings(settings)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("normalizeDownloaderSettings() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDownloaderSettings() error = %v", err)
			}
			if tt.verify != nil {
				tt.verify(t, settings)
			}
		})
	}
}

type fakeDownloaderSettingStore struct {
	value     json.RawMessage
	encrypted bool
	found     bool
	loadErr   error
	saveErr   error
	saved     json.RawMessage
	loadCalls int
	saveCalls int
}

func (s *fakeDownloaderSettingStore) LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error) {
	s.loadCalls++
	if s.loadErr != nil {
		return nil, false, s.loadErr
	}
	if !s.found {
		return nil, false, nil
	}
	settings, err := downloadconfig.Decode(s.value)
	if err != nil {
		return nil, false, err
	}
	return settings, true, nil
}

func (s *fakeDownloaderSettingStore) SaveDownloaders(_ context.Context, settings []downloadconfig.Downloader) error {
	s.saveCalls++
	if s.saveErr != nil {
		return s.saveErr
	}
	value, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	s.saved = append(json.RawMessage(nil), value...)
	s.value = append(json.RawMessage(nil), value...)
	s.encrypted = true
	s.found = true
	return nil
}

func newDownloaderTestRouter(t *testing.T, store DownloaderSettingStore, user auth.User) (http.Handler, string) {
	return newDownloaderTestRouterWithReload(t, store, user, nil)
}

func newDownloaderTestRouterWithReload(t *testing.T, store DownloaderSettingStore, user auth.User, reload func(context.Context) error) (http.Handler, string) {
	t.Helper()
	authService := auth.NewService(nil, "downloader-http-test-jwt-secret", "downloader-http-test-resource-secret", time.Hour)
	token, err := authService.AccessToken(user)
	if err != nil {
		t.Fatalf("authService.AccessToken() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewRouter(Dependencies{
		Config: config.Config{
			MaxRequestBody: defaultDownloaderRequestBodyLimit,
			FrontendDir:    t.TempDir(),
		},
		DownloaderSettings: store,
		ReloadDownloaders:  reload,
		Users:              fakeUserLookup{user: user},
		Auth:               authService,
		Logger:             logger,
	})
	return handler, token
}

func performDownloaderRequest(handler http.Handler, method, target, body, token string) *httptest.ResponseRecorder {
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
