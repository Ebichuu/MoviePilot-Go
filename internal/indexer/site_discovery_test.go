package indexer

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newDiscoveryClient(t *testing.T) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient(): %v", err)
	}
	// 测试服务器监听 127.0.0.1，必须显式放开内网限制。
	client.SetAllowPrivateHosts(true)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func TestDiscoverSiteFavicon(t *testing.T) {
	icon := []byte("ICON-BYTES")
	page := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/":
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, body)
			case "/static/icon.png", "/favicon.ico":
				_, _ = w.Write(icon)
			default:
				http.NotFound(w, r)
			}
		}))
	}

	t.Run("优先使用声明的图标", func(t *testing.T) {
		server := page(`<html><head><link rel="shortcut icon" href="/static/icon.png"></head></html>`)
		defer server.Close()
		iconURL, data, err := newDiscoveryClient(t).DiscoverSiteFavicon(t.Context(), server.URL, "", "")
		if err != nil {
			t.Fatalf("DiscoverSiteFavicon(): %v", err)
		}
		if iconURL != server.URL+"/static/icon.png" {
			t.Fatalf("图标地址 = %q", iconURL)
		}
		if data != base64.StdEncoding.EncodeToString(icon) {
			t.Fatalf("图标数据 = %q", data)
		}
	})

	t.Run("没有声明时回落到 favicon.ico", func(t *testing.T) {
		server := page(`<html><head><title>x</title></head></html>`)
		defer server.Close()
		iconURL, data, err := newDiscoveryClient(t).DiscoverSiteFavicon(t.Context(), server.URL, "", "")
		if err != nil {
			t.Fatalf("DiscoverSiteFavicon(): %v", err)
		}
		if iconURL != server.URL+"/favicon.ico" || data == "" {
			t.Fatalf("回落图标 = (%q, %q)", iconURL, data)
		}
	})

	t.Run("声明的 href 为空时回落到 favicon.ico", func(t *testing.T) {
		server := page(`<html><head><link rel="icon" href="  "></head></html>`)
		defer server.Close()
		iconURL, _, err := newDiscoveryClient(t).DiscoverSiteFavicon(t.Context(), server.URL, "", "")
		if err != nil {
			t.Fatalf("DiscoverSiteFavicon(): %v", err)
		}
		if iconURL != server.URL+"/favicon.ico" {
			t.Fatalf("回落图标 = %q", iconURL)
		}
	})

	t.Run("图标 404 时只返回地址", func(t *testing.T) {
		server := page(`<html><head><link rel="icon" href="/missing-icon.png"></head></html>`)
		defer server.Close()
		iconURL, data, err := newDiscoveryClient(t).DiscoverSiteFavicon(t.Context(), server.URL, "", "")
		if err != nil {
			t.Fatalf("DiscoverSiteFavicon(): %v", err)
		}
		if iconURL != server.URL+"/missing-icon.png" || data != "" {
			t.Fatalf("缺失图标 = (%q, %q)", iconURL, data)
		}
	})

	t.Run("跨主机图标地址被拒绝", func(t *testing.T) {
		server := page(`<html><head><link rel="icon" href="https://evil.example/icon.png"></head></html>`)
		defer server.Close()
		iconURL, data, err := newDiscoveryClient(t).DiscoverSiteFavicon(t.Context(), server.URL, "", "")
		if err != nil || iconURL != "" || data != "" {
			t.Fatalf("跨主机图标 = (%q, %q, %v)", iconURL, data, err)
		}
	})

	t.Run("首页非 2xx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()
		_, _, err := newDiscoveryClient(t).DiscoverSiteFavicon(t.Context(), server.URL, "", "")
		var statusErr *HTTPStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("首页 500 = %v", err)
		}
	})

	t.Run("参数守卫", func(t *testing.T) {
		client := newDiscoveryClient(t)
		if _, _, err := client.DiscoverSiteFavicon(nil, "https://example.com", "", ""); err == nil {
			t.Fatal("nil ctx 应报错")
		}
		if _, _, err := client.DiscoverSiteFavicon(t.Context(), "https://example.com", "a=b\r\nX: y", ""); err == nil {
			t.Fatal("换行 Cookie 应报错")
		}
		if _, _, err := client.DiscoverSiteFavicon(t.Context(), "http://localhost:8080", "", ""); err == nil {
			t.Fatal("localhost 应报错")
		}
		if _, _, err := client.DiscoverSiteFavicon(t.Context(), "ftp://example.com", "", ""); err == nil {
			t.Fatal("非 HTTP 协议应报错")
		}
	})
}

func TestDiscoverSiteRSS(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"rss", `<link rel="alternate" type="application/rss+xml" href="/feed.xml">`, "/feed.xml"},
		{"atom", `<link rel="alternate" type="application/atom+xml" href="/atom.xml">`, "/atom.xml"},
		{"json feed", `<link rel="alternate" type="application/feed+json" href="/feed.json">`, "/feed.json"},
		{"带字符集的类型", `<link rel="alternate" type="Application/RSS+XML; charset=utf-8" href="/feed.xml">`, "/feed.xml"},
		{"没有 feed", `<link rel="icon" href="/favicon.ico">`, ""},
		{"rel 不是 alternate", `<link rel="stylesheet" type="application/rss+xml" href="/feed.xml">`, ""},
		{"href 为空", `<link rel="alternate" type="application/rss+xml" href=" ">`, ""},
		{"跨主机 feed", `<link rel="alternate" type="application/rss+xml" href="https://evil.example/feed.xml">`, ""},
		{"错误的类型", `<link rel="alternate" type="text/html" href="/feed.xml">`, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<html><head>"+testCase.body+"</head></html>")
			}))
			defer server.Close()
			feed, err := newDiscoveryClient(t).DiscoverSiteRSS(t.Context(), server.URL, "", "")
			if err != nil {
				t.Fatalf("DiscoverSiteRSS(): %v", err)
			}
			want := testCase.want
			if want != "" {
				want = server.URL + testCase.want
			}
			if feed != want {
				t.Fatalf("feed = %q, want %q", feed, want)
			}
		})
	}

	t.Run("首页非 2xx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()
		if _, err := newDiscoveryClient(t).DiscoverSiteRSS(t.Context(), server.URL, "", ""); err == nil {
			t.Fatal("403 首页应报错")
		}
	})

	t.Run("nil ctx", func(t *testing.T) {
		if _, err := newDiscoveryClient(t).DiscoverSiteRSS(nil, "https://example.com", "", ""); err == nil {
			t.Fatal("nil ctx 应报错")
		}
	})
}

func TestTestSiteAvailability(t *testing.T) {
	cases := map[int]bool{
		http.StatusOK:                  true,
		http.StatusForbidden:           true,
		http.StatusInternalServerError: true,
		http.StatusNotFound:            false,
		http.StatusBadGateway:          false,
	}
	for status, accept := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			err := newDiscoveryClient(t).TestSiteAvailability(t.Context(), server.URL, "", "")
			if accept && err != nil {
				t.Fatalf("%d 应视为可用: %v", status, err)
			}
			if !accept {
				var statusErr *HTTPStatusError
				if !errors.As(err, &statusErr) || statusErr.StatusCode != status {
					t.Fatalf("%d 应返回状态错误: %v", status, err)
				}
			}
		})
	}

	client := newDiscoveryClient(t)
	if err := client.TestSiteAvailability(nil, "https://example.com", "", ""); err == nil {
		t.Fatal("nil ctx 应报错")
	}
	if err := client.TestSiteAvailability(t.Context(), "https://example.com", "", "ua\r\n"); err == nil {
		t.Fatal("换行 User-Agent 应报错")
	}
	if err := client.TestSiteAvailability(t.Context(), "not-a-url", "", ""); err == nil {
		t.Fatal("非法地址应报错")
	}
	var nilClient *HTTPClient
	if err := nilClient.TestSiteAvailability(t.Context(), "https://example.com", "", ""); err == nil {
		t.Fatal("nil 客户端应报错")
	}
}

func TestTestSiteLoginPage(t *testing.T) {
	serve := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
	}

	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"已登录", http.StatusOK, `<html><body><a href="/logout.php">退出</a></body></html>`, ""},
		{"user-info 容器", http.StatusOK, `<html><body><div class="user-info-side">用户</div></body></html>`, ""},
		{"登录表单", http.StatusOK, `<html><body><input type="password" name="pwd"></body></html>`, "Cookie 已失效"},
		{"Cloudflare 挑战", http.StatusOK, `<html><body>Just a moment...</body></html>`, "Cloudflare"},
		{"空响应", http.StatusOK, "", "站点响应为空"},
		{"无法确认状态", http.StatusOK, `<html><body>欢迎</body></html>`, "无法确认站点登录状态"},
		{"403 且无标记", http.StatusForbidden, `<html><body>欢迎</body></html>`, "site returned HTTP 403"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := serve(testCase.status, testCase.body)
			defer server.Close()
			err := newDiscoveryClient(t).TestSiteLoginPage(t.Context(), server.URL, "", "")
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("错误 = %v，期望包含 %q", err, testCase.wantErr)
			}
		})
	}

	client := newDiscoveryClient(t)
	if err := client.TestSiteLoginPage(nil, "https://example.com", "", ""); err == nil {
		t.Fatal("nil ctx 应报错")
	}
	if err := client.TestSiteLoginPage(t.Context(), "https://example.com", "a\r\nb", ""); err == nil {
		t.Fatal("换行 Cookie 应报错")
	}
	if err := client.TestSiteLoginPage(t.Context(), "https://example.com:bad", "", ""); err == nil {
		t.Fatal("非法端口应报错")
	}
}

func TestSiteIconAndFeedLinkHelpers(t *testing.T) {
	for _, rel := range []string{"icon", "shortcut icon", "apple-touch-icon", "ICON", "mask-icon shortcut"} {
		if !siteIconRel(rel) {
			t.Fatalf("siteIconRel(%q) = false", rel)
		}
	}
	for _, rel := range []string{"", "stylesheet", "preload", "iconx"} {
		if siteIconRel(rel) {
			t.Fatalf("siteIconRel(%q) = true", rel)
		}
	}

	feeds := []struct {
		rel, mediaType string
		want           bool
	}{
		{"alternate", "application/rss+xml", true},
		{"Alternate", "APPLICATION/ATOM+XML", true},
		{"alternate", "application/feed+json", true},
		{"alternate", "application/rss+xml; charset=utf-8", true},
		{"alternate", "application/json", false},
		{"stylesheet", "application/rss+xml", false},
		{"", "", false},
	}
	for _, testCase := range feeds {
		if got := siteFeedLink(testCase.rel, testCase.mediaType); got != testCase.want {
			t.Fatalf("siteFeedLink(%q, %q) = %v", testCase.rel, testCase.mediaType, got)
		}
	}
}

func TestHTTPClientHelpers(t *testing.T) {
	var nilClient *HTTPClient
	nilClient.SetAllowPrivateHosts(true)
	nilClient.CloseIdleConnections()

	if _, err := NewHTTPClient(0, time.Second, 1<<20); err == nil {
		t.Fatal("workers=0 应报错")
	}
	if _, err := NewHTTPClient(129, time.Second, 1<<20); err == nil {
		t.Fatal("workers=129 应报错")
	}
	if _, err := NewHTTPClient(1, 0, 1<<20); err == nil {
		t.Fatal("timeout=0 应报错")
	}
	if _, err := NewHTTPClient(1, time.Second, 0); err == nil {
		t.Fatal("maxBodyBytes=0 应报错")
	}
	if _, err := NewHTTPClient(1, time.Second, 16<<20+1); err == nil {
		t.Fatal("maxBodyBytes 超限应报错")
	}

	statusErr := (&HTTPStatusError{StatusCode: http.StatusTeapot}).Error()
	if !strings.Contains(statusErr, "418") {
		t.Fatalf("HTTPStatusError = %q", statusErr)
	}

	cause := errors.New("底因")
	notStarted := &httpRequestNotStartedError{cause: cause}
	if notStarted.Error() != "底因" {
		t.Fatalf("notStarted.Error() = %q", notStarted.Error())
	}
	if !errors.Is(notStarted, errHTTPRequestNotStarted) || !errors.Is(notStarted, cause) {
		t.Fatal("httpRequestNotStartedError 应同时匹配哨兵与底因")
	}

	makeRequest := func(rawURL string) *http.Request {
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		return request
	}
	if err := sameHostRedirect(makeRequest("https://example.com/a"), nil); err != nil {
		t.Fatalf("首跳应放行: %v", err)
	}
	first := makeRequest("https://example.com/a")
	if err := sameHostRedirect(makeRequest("https://example.com/b"), []*http.Request{first}); err != nil {
		t.Fatalf("同主机跳转应放行: %v", err)
	}
	if err := sameHostRedirect(makeRequest("https://other.example/b"), []*http.Request{first}); err == nil {
		t.Fatal("跨主机跳转应拒绝")
	}
	if err := sameHostRedirect(makeRequest("http://example.com/b"), []*http.Request{first}); err == nil {
		t.Fatal("跨协议跳转应拒绝")
	}
	if err := sameHostRedirect(first, []*http.Request{first, first, first}); err == nil {
		t.Fatal("超过 3 跳应拒绝")
	}
}
