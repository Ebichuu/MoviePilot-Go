package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/cookiecloud"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

// newCookieCloudSiteHTTPClient 返回允许访问 127.0.0.1 的站点 HTTP 客户端。
func newCookieCloudSiteHTTPClient(t *testing.T) *indexer.HTTPClient {
	t.Helper()
	client, err := indexer.NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	client.SetAllowPrivateHosts(true)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// TestCookieCloudConnectivityTesterSiteHTTPBranches 覆盖公开站点探测、登录页探测
// 与探测失败时的固定提示。
func TestCookieCloudConnectivityTesterSiteHTTPBranches(t *testing.T) {
	// 已登录的站点页面：带退出链接即可视为连通。
	loggedIn := `<html><body><a href="/logout">退出</a></body></html>`
	// 未登录：页面里出现密码框就说明 Cookie 失效。
	loginPage := `<html><body><form><input type="text" name="username">` +
		`<input type="password" name="password"></form></body></html>`
	status := http.StatusOK
	body := loggedIn
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	tester := &cookieCloudConnectivityTester{
		store:    &database.Store{},
		siteHTTP: newCookieCloudSiteHTTPClient(t),
	}

	// 公开站点：首页 200 即可连通。
	ok, message, err := tester.Test(context.Background(), cookiecloud.Site{
		Name: "Public", URL: server.URL, Public: true,
	})
	if err != nil || !ok || message != "连接成功" {
		t.Fatalf("公开站点探测 = (%v, %q, %v)", ok, message, err)
	}

	// 非公开站点：已登录页面视为连通。
	ok, message, err = tester.Test(context.Background(), cookiecloud.Site{
		Name: "Private", URL: server.URL,
	})
	if err != nil || !ok || message != "连接成功" {
		t.Fatalf("登录页探测 = (%v, %q, %v)", ok, message, err)
	}

	// 非公开站点 + 登录表单：判定 Cookie 失效。
	body = loginPage
	ok, message, err = tester.Test(context.Background(), cookiecloud.Site{
		Name: "Expired", URL: server.URL,
	})
	if ok || message != "站点连接或登录验证失败" || err == nil {
		t.Fatalf("失效 Cookie 探测 = (%v, %q, %v)", ok, message, err)
	}

	// 公开站点返回白名单外的状态码（404）：判定不可用。
	status = http.StatusNotFound
	body = "not found"
	ok, message, err = tester.Test(context.Background(), cookiecloud.Site{
		Name: "Missing", URL: server.URL, Public: true,
	})
	if ok || message != "站点连接或登录验证失败" || err == nil {
		t.Fatalf("404 探测 = (%v, %q, %v)", ok, message, err)
	}

	// 非公开站点返回 401：登录页探测认为无法确认登录状态。
	status = http.StatusUnauthorized
	ok, message, err = tester.Test(context.Background(), cookiecloud.Site{
		Name: "Unauthorized", URL: server.URL,
	})
	if ok || message != "站点连接或登录验证失败" || err == nil {
		t.Fatalf("401 探测 = (%v, %q, %v)", ok, message, err)
	}
}

// TestCookieCloudCacheSiteIconBranches 覆盖缺少 HTTP 客户端与抓取图标失败两个提前返回。
func TestCookieCloudCacheSiteIconBranches(t *testing.T) {
	// 缺客户端：静默返回。
	(&cookieCloudSiteStore{store: &database.Store{}}).cacheSiteIcon(context.Background(), database.Site{})

	// 抓取失败：同样静默返回，不触碰数据库（store 为零值）。
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "icon unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	(&cookieCloudSiteStore{siteHTTP: newCookieCloudSiteHTTPClient(t)}).
		cacheSiteIcon(context.Background(), database.Site{URL: broken.URL, Domain: "example.test"})

	// nil 接收者：两个入口都必须安全。
	var nilStore *cookieCloudSiteStore
	nilStore.cacheSiteIcon(context.Background(), database.Site{})
	nilStore.markDirty()
}

// TestCookieCloudConnectivityTesterRejectsUnsafeInput 覆盖 cookie/UA 带换行时的参数校验。
func TestCookieCloudConnectivityTesterRejectsUnsafeInput(t *testing.T) {
	tester := &cookieCloudConnectivityTester{
		store:    &database.Store{},
		siteHTTP: newCookieCloudSiteHTTPClient(t),
	}
	ok, message, err := tester.Test(context.Background(), cookiecloud.Site{
		URL: "http://127.0.0.1:1", Public: true, Cookie: "a\r\nb",
	})
	if ok || message != "站点连接或登录验证失败" || err == nil ||
		!strings.Contains(err.Error(), "站点测试参数无效") {
		t.Fatalf("非法 cookie 探测 = (%v, %q, %v)", ok, message, err)
	}
}
