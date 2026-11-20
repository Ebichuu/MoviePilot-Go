package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPublicRouterEndpoints 覆盖 v1/v2 里无需鉴权的静态端点。
func TestPublicRouterEndpoints(t *testing.T) {
	deps := Dependencies{}
	cases := []struct {
		name   string
		router http.Handler
		path   string
		substr string
	}{
		{name: "v1 壁纸", router: deps.v1Router(), path: "/login/wallpaper", substr: `"success":true`},
		{name: "v1 壁纸列表", router: deps.v1Router(), path: "/login/wallpapers", substr: "[]"},
		{name: "v1 认证提供者", router: deps.v1Router(), path: "/auth/providers", substr: "[]"},
		{name: "v2 壁纸", router: deps.v2Router(), path: "/login/wallpaper", substr: `"success":true`},
		{name: "v2 壁纸列表", router: deps.v2Router(), path: "/login/wallpapers", substr: `"success":true`},
		{name: "v2 认证提供者", router: deps.v2Router(), path: "/auth/providers", substr: `"success":true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("状态码 = %d（%s）", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.substr) {
				t.Fatalf("响应 = %s，期望包含 %q", recorder.Body.String(), tc.substr)
			}
		})
	}
}

// TestRouterHealthAndMethodNotAllowed 覆盖健康检查与两类 405 响应。
func TestRouterHealthAndMethodNotAllowed(t *testing.T) {
	router := NewRouter(Dependencies{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("健康检查 = %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), backendVersion) {
		t.Fatalf("健康检查缺少版本号: %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("就绪检查 = %d", recorder.Code)
	}

	// 非 API 路径：纯文本 405。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if recorder.Code != http.StatusMethodNotAllowed || strings.Contains(recorder.Body.String(), `"success"`) {
		t.Fatalf("非 API 405 = %d %s", recorder.Code, recorder.Body.String())
	}

	// API 路径：JSON 405。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/login/wallpaper", nil))
	if recorder.Code != http.StatusMethodNotAllowed ||
		!strings.Contains(recorder.Body.String(), "请求方法不受支持") {
		t.Fatalf("API 405 = %d %s", recorder.Code, recorder.Body.String())
	}
}
