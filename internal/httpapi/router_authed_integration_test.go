//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// loggedInRouter 起一个真实的 `NewRouter`，用表单登录换出访问令牌。
func loggedInRouter(t *testing.T, username string, superUser bool) (http.Handler, string) {
	t.Helper()
	deps := newHTTPAPIIntegrationDeps(t)
	deps.Auth = auth.NewService(deps.Store, "jwt-secret-for-test", "resource-secret-for-test", time.Hour)
	seedHTTPAPIUser(t, deps, username, superUser)

	router := NewRouter(deps)
	form := url.Values{"username": {username}, "password": {"abcd1234"}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/login/access-token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("登录失败 = %d（%s）", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil || payload.AccessToken == "" {
		t.Fatalf("解析访问令牌失败: %v（%s）", err, recorder.Body.String())
	}
	return router, payload.AccessToken
}

// TestRouterStaticEndpointsIntegration 覆盖挂在鉴权组里的静态占位端点。
func TestRouterStaticEndpointsIntegration(t *testing.T) {
	router, token := loggedInRouter(t, "router-admin", true)

	cases := []struct {
		path   string
		substr string
	}{
		{path: "/api/v1/system/ping", substr: "pong"},
		{path: "/api/v1/system/global", substr: "PROJECT_NAME"},
		{path: "/api/v1/plugin/", substr: "[]"},
		{path: "/api/v1/plugin/installed", substr: "[]"},
		{path: "/api/v1/workflow/", substr: "[]"},
		{path: "/api/v1/workflow/event_types", substr: "[]"},
		{path: "/api/v1/llm/models", substr: `"success":true`},
		{path: "/api/v1/llm/providers", substr: `"success":true`},
		{path: "/api/v1/user/current", substr: "router-admin"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("状态码 = %d（%s）", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.substr) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.substr)
			}
		})
	}
}

// TestRouterAdminGuardIntegration 覆盖 `requireAdmin` 对普通用户的拒绝。
func TestRouterAdminGuardIntegration(t *testing.T) {
	router, token := loggedInRouter(t, "router-user", false)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/user/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问管理接口 = %d（%s）", recorder.Code, recorder.Body.String())
	}

	// 没有令牌：401。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/user/", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("匿名访问 = %d（%s）", recorder.Code, recorder.Body.String())
	}
}
