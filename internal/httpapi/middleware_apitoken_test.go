package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// TestRequireAPIToken 覆盖 V2 `verify_apitoken` 的三种拒绝与一种通过：
// 未配置 API_TOKEN 一律拒绝、缺 token 拒绝、token 不匹配拒绝、匹配（允许首尾空白）放行。
func TestRequireAPIToken(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	dependencies := Dependencies{Config: config.Config{APIToken: "s3cret"}}
	handler := dependencies.requireAPIToken(next)

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"缺少 token", "", http.StatusForbidden},
		{"token 不匹配", "?token=wrong", http.StatusForbidden},
		{"token 为空", "?token=", http.StatusForbidden},
		{"token 匹配", "?token=s3cret", http.StatusNoContent},
		{"token 允许首尾空白", "?token=%20s3cret%20", http.StatusNoContent},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/system"+testCase.query, nil))
			if recorder.Code != testCase.want {
				t.Fatalf("状态码 = %d, want %d", recorder.Code, testCase.want)
			}
			if testCase.want == http.StatusForbidden &&
				!strings.Contains(recorder.Body.String(), "API_TOKEN 校验失败") {
				t.Fatalf("拒绝响应体 = %q", recorder.Body.String())
			}
		})
	}

	// 未配置 API_TOKEN 时即使带上非空 token 也拒绝。
	empty := Dependencies{}.requireAPIToken(next)
	recorder := httptest.NewRecorder()
	empty.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/system?token=anything", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("未配置 API_TOKEN 状态码 = %d, want 403", recorder.Code)
	}
}
