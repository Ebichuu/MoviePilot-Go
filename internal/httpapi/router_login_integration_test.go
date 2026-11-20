//go:build integration

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// TestV2LoginWrapperIntegration 覆盖 V2 登录包装器的成功包装与失败透传。
func TestV2LoginWrapperIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	deps.Auth = auth.NewService(deps.Store, "jwt-secret-for-test", "resource-secret-for-test", time.Hour)
	seedHTTPAPIUser(t, deps, "login-user", false)
	router := deps.v2Router()

	login := func(password string) *httptest.ResponseRecorder {
		form := url.Values{"username": {"login-user"}, "password": {password}}
		request := httptest.NewRequest(http.MethodPost, "/login/access-token", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	success := login("abcd1234")
	if success.Code != http.StatusOK {
		t.Fatalf("登录成功 = %d（%s）", success.Code, success.Body.String())
	}
	body := success.Body.String()
	if !strings.Contains(body, `"success":true`) || !strings.Contains(body, "access_token") {
		t.Fatalf("V2 登录响应 = %s", body)
	}
	if cookie := success.Header().Get("Set-Cookie"); !strings.Contains(cookie, "MoviePilot=") {
		t.Fatalf("资源令牌 Cookie = %q", cookie)
	}

	failure := login("wrong-password")
	if failure.Code != http.StatusUnauthorized {
		t.Fatalf("登录失败 = %d（%s）", failure.Code, failure.Body.String())
	}
	if !strings.Contains(failure.Body.String(), "用户名或密码错误") {
		t.Fatalf("失败响应 = %s", failure.Body.String())
	}
}
