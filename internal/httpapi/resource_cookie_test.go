package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// 图片和流媒体只会带 Cookie（<img src> / <video src> 加不了 Authorization），
// 所以每个带 Bearer 的鉴权请求都得顺手把资源令牌续上，否则登录 30 分钟后
// 豆瓣海报、演员头像会整片 403。
func TestRefreshResourceCookieIssuesAndKeepsToken(t *testing.T) {
	service := auth.NewService(nil, "cookie-jwt-secret", "cookie-resource-secret", time.Hour)
	accessToken, err := service.AccessToken(auth.User{ID: 7, Username: "alice", SuperUser: true})
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	claims, err := service.VerifyAccessToken(accessToken)
	if err != nil {
		t.Fatalf("VerifyAccessToken() error = %v", err)
	}

	t.Run("没有 Cookie 时下发", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		refreshResourceCookie(service, recorder, httptest.NewRequest(http.MethodGet, "/api/v1/user/name", nil), claims)

		cookie := findResourceCookie(recorder)
		if cookie == nil {
			t.Fatal("没有下发 MoviePilot Cookie")
		}
		issued, verifyErr := service.VerifyResourceToken(cookie.Value)
		if verifyErr != nil {
			t.Fatalf("下发的 Cookie 校验失败: %v", verifyErr)
		}
		if issued.Subject != "7" || issued.Username != "alice" || !issued.SuperUser {
			t.Fatalf("身份丢失: %+v", issued)
		}
		if issued.ExpiresAt == nil || time.Until(issued.ExpiresAt.Time) < resourceTokenLifetime/2 {
			t.Fatalf("有效期不足: %+v", issued.ExpiresAt)
		}
	})

	t.Run("剩余充足时不重签", func(t *testing.T) {
		token, issueErr := service.ResourceToken(auth.User{ID: 7, Username: "alice", SuperUser: true}, resourceTokenLifetime)
		if issueErr != nil {
			t.Fatalf("ResourceToken() error = %v", issueErr)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/user/name", nil)
		request.AddCookie(&http.Cookie{Name: resourceCookieName, Value: token})
		recorder := httptest.NewRecorder()

		refreshResourceCookie(service, recorder, request, claims)

		if cookie := findResourceCookie(recorder); cookie != nil {
			t.Fatalf("有效期还够就不该重签，实际下发了 %q", cookie.Value)
		}
	})

	t.Run("临期时重签", func(t *testing.T) {
		// 1 分钟有效期的令牌剩余时间小于 resourceTokenLifetime/3，必须换新。
		token, issueErr := service.ResourceToken(auth.User{ID: 7, Username: "alice", SuperUser: true}, time.Minute)
		if issueErr != nil {
			t.Fatalf("ResourceToken() error = %v", issueErr)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/user/name", nil)
		request.AddCookie(&http.Cookie{Name: resourceCookieName, Value: token})
		recorder := httptest.NewRecorder()

		refreshResourceCookie(service, recorder, request, claims)

		cookie := findResourceCookie(recorder)
		if cookie == nil {
			t.Fatal("临期令牌没有刷新")
		}
		if cookie.Value == token {
			t.Fatal("下发的还是同一个令牌")
		}
	})
}

func findResourceCookie(recorder *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == resourceCookieName {
			return cookie
		}
	}
	return nil
}
