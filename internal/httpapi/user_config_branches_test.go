package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

type userConfigStoreFake struct {
	values   map[string]any
	getErr   error
	setErr   error
	lastKey  string
	lastJSON any
	setCalls int
}

func (s *userConfigStoreFake) GetSetting(_ context.Context, key string, target any) (bool, error) {
	s.lastKey = key
	if s.getErr != nil {
		return false, s.getErr
	}
	value, ok := s.values[key]
	if !ok {
		return false, nil
	}
	container, ok := target.(*any)
	if !ok {
		return false, errors.New("unsupported target")
	}
	*container = value
	return true, nil
}

func (s *userConfigStoreFake) SetSetting(_ context.Context, key string, value any, _ bool) error {
	s.setCalls++
	s.lastKey, s.lastJSON = key, value
	return s.setErr
}

func userConfigRequest(method, key, body, subject string) *http.Request {
	request := httptest.NewRequest(method, "/api/v1/user/config/"+key, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("key", key)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, rctx)
	claims := auth.Claims{Username: "alice", RegisteredClaims: jwt.RegisteredClaims{Subject: subject}}
	return request.WithContext(context.WithValue(ctx, claimsContextKey, claims))
}

// TestUserConfigStorageKeyBranches 覆盖用户配置存储键的全部拒绝理由与正常编码。
func TestUserConfigStorageKeyBranches(t *testing.T) {
	if _, ok := userConfigStorageKey(httptest.NewRequest(http.MethodGet, "/x", nil), "theme"); ok {
		t.Fatal("没有登录态应被拒绝")
	}
	if _, ok := userConfigStorageKey(userConfigRequest(http.MethodGet, "", "", "1"), "  "); ok {
		t.Fatal("空键应被拒绝")
	}
	if _, ok := userConfigStorageKey(userConfigRequest(http.MethodGet, "", "", "1"),
		strings.Repeat("k", userConfigKeyLimit+1)); ok {
		t.Fatal("超长键应被拒绝")
	}
	if _, ok := userConfigStorageKey(userConfigRequest(http.MethodGet, "", "", "1"), "a\x00b"); ok {
		t.Fatal("含 NUL 的键应被拒绝")
	}
	if _, ok := userConfigStorageKey(userConfigRequest(http.MethodGet, "theme", "", ""), "theme"); ok {
		t.Fatal("没有用户 ID 应被拒绝")
	}
	if _, ok := userConfigStorageKey(userConfigRequest(http.MethodGet, "theme", "", "0"), "theme"); ok {
		t.Fatal("用户 ID 为 0 应被拒绝")
	}
	if _, ok := userConfigStorageKey(userConfigRequest(http.MethodGet, "theme", "", "abc"), "theme"); ok {
		t.Fatal("用户 ID 非数字应被拒绝")
	}
	key, ok := userConfigStorageKey(userConfigRequest(http.MethodGet, "theme", "", "7"), " theme ")
	if !ok || !strings.HasPrefix(key, userConfigKeyPrefix+"7:") {
		t.Fatalf("存储键 = %q（ok=%v）", key, ok)
	}
}

// TestGetUserConfigBranches 覆盖读取用户配置的五条出口。
func TestGetUserConfigBranches(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{}.getUserConfig(recorder, userConfigRequest(http.MethodGet, "theme", "", "1"))
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), "用户配置存储不可用") {
		t.Fatalf("无存储 = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	Dependencies{UserConfigs: &userConfigStoreFake{getErr: errors.New("boom")}}.
		getUserConfig(recorder, userConfigRequest(http.MethodGet, "theme", "", "1"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("读取失败 = %d", recorder.Code)
	}

	// 未配置：value 为 null。
	store := &userConfigStoreFake{}
	recorder = httptest.NewRecorder()
	Dependencies{UserConfigs: store}.getUserConfig(recorder, userConfigRequest(http.MethodGet, "theme", "", "1"))
	if !strings.Contains(recorder.Body.String(), `"value":null`) {
		t.Fatalf("未配置 = %s", recorder.Body.String())
	}
	if !strings.HasSuffix(store.lastKey, ":dGhlbWU") {
		t.Fatalf("存储键未按 base64 编码: %q", store.lastKey)
	}

	// 已配置：value 原样返回。
	configured := &userConfigStoreFake{values: map[string]any{
		userConfigKeyPrefix + "1:dGhlbWU": "dark",
	}}
	recorder = httptest.NewRecorder()
	Dependencies{UserConfigs: configured}.
		getUserConfig(recorder, userConfigRequest(http.MethodGet, "theme", "", "1"))
	if !strings.Contains(recorder.Body.String(), `"value":"dark"`) {
		t.Fatalf("已配置 = %s", recorder.Body.String())
	}

	// 键非法：400。
	recorder = httptest.NewRecorder()
	Dependencies{UserConfigs: store}.
		getUserConfig(recorder, userConfigRequest(http.MethodGet, "", "", "1"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("键非法 = %d", recorder.Code)
	}
}

// TestSetUserConfigBranches 覆盖写入用户配置的八条出口。
func TestSetUserConfigBranches(t *testing.T) {
	store := &userConfigStoreFake{}
	base := Dependencies{
		Config:      config.Config{MaxRequestBody: 64},
		UserConfigs: store,
	}

	cases := []struct {
		name    string
		deps    Dependencies
		key     string
		body    string
		code    int
		message string
	}{
		{name: "键非法", deps: base, key: "", body: `{}`, code: http.StatusBadRequest},
		{name: "没有存储", deps: Dependencies{Config: base.Config}, key: "theme", body: `{}`,
			code: http.StatusServiceUnavailable, message: "用户配置存储不可用"},
		{name: "请求限制未配置", deps: Dependencies{UserConfigs: store}, key: "theme", body: `{}`,
			code: http.StatusServiceUnavailable, message: "用户配置请求限制未配置"},
		{name: "请求体超限", deps: base, key: "theme", body: `"` + strings.Repeat("x", 80) + `"`,
			code: http.StatusRequestEntityTooLarge},
		{name: "非法 JSON", deps: base, key: "theme", body: `nope`, code: http.StatusBadRequest},
		{name: "多个 JSON 值", deps: base, key: "theme", body: `1 2`, code: http.StatusBadRequest},
		{name: "保存失败", deps: Dependencies{
			Config: base.Config, UserConfigs: &userConfigStoreFake{setErr: errors.New("boom")},
		}, key: "theme", body: `"dark"`, code: http.StatusInternalServerError},
		{name: "空请求体按 null 保存", deps: base, key: "theme", body: "", code: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.deps.setUserConfig(recorder, userConfigRequest(http.MethodPut, tc.key, tc.body, "1"))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d（%s）", recorder.Code, tc.code, recorder.Body.String())
			}
			if tc.message != "" && !strings.Contains(recorder.Body.String(), tc.message) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.message)
			}
		})
	}

	// 正常写入：空请求体被归一成 null。
	recorder := httptest.NewRecorder()
	base.setUserConfig(recorder, userConfigRequest(http.MethodPut, "theme", "", "1"))
	if recorder.Code != http.StatusOK || store.lastJSON != nil {
		t.Fatalf("空请求体 = %d（value=%v）", recorder.Code, store.lastJSON)
	}
	recorder = httptest.NewRecorder()
	base.setUserConfig(recorder, userConfigRequest(http.MethodPut, "theme", `{"a":1}`, "1"))
	if recorder.Code != http.StatusOK || store.lastJSON == nil {
		t.Fatalf("正常写入 = %d（value=%v）", recorder.Code, store.lastJSON)
	}
}

// TestSystemMessageStreamsEmptyEvents 覆盖 SSE 长连接的首帧与「不支持 Flush」降级。
func TestSystemMessageStreamsEmptyEvents(t *testing.T) {
	plain := &plainResponseWriter{header: http.Header{}}
	Dependencies{}.systemMessage(plain, httptest.NewRequest(http.MethodGet, "/api/v1/system/message", nil))
	if plain.body.Len() != 0 {
		t.Fatalf("不支持 Flush 时不应写入事件: %q", plain.body.String())
	}
	if plain.header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", plain.header.Get("Content-Type"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/system/message", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	Dependencies{}.systemMessage(recorder, request)
	if recorder.Body.String() != "data: \n\n" {
		t.Fatalf("首帧 = %q", recorder.Body.String())
	}
}
