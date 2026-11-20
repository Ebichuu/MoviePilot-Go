package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// declarativeFlowServer 起一个只在本机监听的站点，供声明式适配器的流程测试使用。
func declarativeFlowServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func buildDeclarative(t *testing.T, baseURL string, mutate func(*Definition)) *DeclarativeAdapter {
	t.Helper()
	definition := Definition{
		ID: 11, Name: "flow", BaseURL: baseURL, Format: "html",
		SearchPath: "/search", ResultSelector: ".row",
		Fields: Fields{Title: Field{Selector: "a.title"}, DetailURL: Field{Selector: "a.title", Attribute: "href"}},
	}
	if mutate != nil {
		mutate(&definition)
	}
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 2), definition)
	if err != nil {
		t.Fatalf("构造声明式适配器: %v", err)
	}
	return adapter
}

func rssDocument(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>feed</title>` +
		strings.Join(items, "") + `</channel></rss>`
}

func rssItem(guid, title, link, enclosure string) string {
	body := "<item>"
	if title != "" {
		body += "<title>" + title + "</title>"
	}
	if link != "" {
		body += "<link>" + link + "</link>"
	}
	if guid != "" {
		body += "<guid>" + guid + "</guid>"
	}
	if enclosure != "" {
		body += `<enclosure url="` + enclosure + `" length="1073741824" type="application/x-bittorrent"/>`
	}
	body += "<pubDate>Mon, 02 Jan 2006 15:04:05 +0800</pubDate></item>"
	return body
}

// TestDeclarativeSearchRefreshesCSRFOnce 覆盖 POST 搜索遇到 401 时刷新一次 CSRF 并重试。
func TestDeclarativeSearchRefreshesCSRFOnce(t *testing.T) {
	var csrfHits, postHits atomic.Int32
	var mu sync.Mutex
	var seenTokens []string
	server := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/csrf":
			count := csrfHits.Add(1)
			_, _ = fmt.Fprintf(w, `<meta name="csrf" content="tok-%d">`, count)
		case "/search":
			postHits.Add(1)
			token := r.Header.Get("X-CSRF-TOKEN")
			mu.Lock()
			seenTokens = append(seenTokens, token)
			mu.Unlock()
			if token == "tok-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"name":"某剧.S01E01"}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	adapter := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "json"
		definition.Method = http.MethodPost
		definition.Body = map[string]any{"kw": "${keyword}", "page": "${page}"}
		definition.ResultPath = "items"
		definition.Fields = Fields{Title: Field{Selector: "name"}}
		definition.CSRFPath = "/csrf"
		definition.CSRFRegex = `content="([^"]+)"`
	})

	resources, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Limit: 10})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(resources) != 1 || resources[0].Title != "某剧.S01E01" {
		t.Fatalf("搜索结果 = %+v", resources)
	}
	if csrfHits.Load() != 2 || postHits.Load() != 2 {
		t.Fatalf("CSRF 刷新次数=%d 请求次数=%d，期望各 2 次", csrfHits.Load(), postHits.Load())
	}
	mu.Lock()
	tokens := append([]string(nil), seenTokens...)
	mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "tok-1" || tokens[1] != "tok-2" {
		t.Fatalf("请求携带的 token = %v", tokens)
	}

	// 第二次搜索必须命中 CSRF 缓存，不再重新抓取 token 页面。
	if _, err := adapter.Search(context.Background(), Query{Keyword: "某剧", Limit: 10}); err != nil {
		t.Fatalf("第二次搜索失败: %v", err)
	}
	if csrfHits.Load() != 2 {
		t.Fatalf("CSRF 缓存未生效，抓取 %d 次", csrfHits.Load())
	}
}

// TestDeclarativeSearchSecondCSRFRefreshFails 覆盖刷新 CSRF 时 token 页面自身失败。
func TestDeclarativeSearchSecondCSRFRefreshFails(t *testing.T) {
	var csrfHits atomic.Int32
	server := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/csrf":
			if csrfHits.Add(1) == 1 {
				_, _ = w.Write([]byte(`<meta content="tok-1">`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		case "/search":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	})

	adapter := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "json"
		definition.Method = http.MethodPost
		definition.Body = map[string]any{"kw": "${keyword}"}
		definition.ResultPath = "items"
		definition.CSRFPath = "/csrf"
		definition.CSRFRegex = `content="([^"]+)"`
	})

	_, err := adapter.Search(context.Background(), Query{Keyword: "x", Limit: 5})
	var statusError *HTTPStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusInternalServerError {
		t.Fatalf("刷新 CSRF 失败时错误 = %v", err)
	}
	if csrfHits.Load() != 2 {
		t.Fatalf("CSRF 抓取次数 = %d，期望 2", csrfHits.Load())
	}
}

// TestDeclarativeSearchRejectsInvalidPostBody 覆盖 POST 模板渲染失败。
func TestDeclarativeSearchRejectsInvalidPostBody(t *testing.T) {
	server := declarativeFlowServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	adapter := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "json"
		definition.Method = http.MethodPost
		definition.Body = map[string]any{"": "x"}
		definition.ResultPath = "items"
	})
	if _, err := adapter.Search(context.Background(), Query{Keyword: "x"}); err == nil ||
		!strings.Contains(err.Error(), "字段名无效") {
		t.Fatalf("非法模板字段错误 = %v", err)
	}
}

// TestDeclarativeSearchRejectsUnknownFormat 覆盖构造后被篡改成未知格式的兜底分支。
func TestDeclarativeSearchRejectsUnknownFormat(t *testing.T) {
	server := declarativeFlowServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><div class="row"></div></html>`))
	})
	adapter := buildDeclarative(t, server.URL, nil)
	adapter.def.Format = "xml"
	if _, err := adapter.Search(context.Background(), Query{Keyword: "x", Limit: 1}); err == nil ||
		!strings.Contains(err.Error(), "未知站点响应格式") {
		t.Fatalf("未知格式错误 = %v", err)
	}
}

// TestDeclarativeSearchEndpointFailure 覆盖搜索地址无法解析。
func TestDeclarativeSearchEndpointFailure(t *testing.T) {
	adapter := buildDeclarative(t, "https://indexer.example.com", func(definition *Definition) {
		definition.SearchPath = "/%zz"
	})
	if _, err := adapter.Search(context.Background(), Query{Keyword: "x"}); err == nil ||
		!strings.Contains(err.Error(), "站点链接无效") {
		t.Fatalf("非法搜索地址错误 = %v", err)
	}
}

// TestDeclarativeCSRFBranches 覆盖 CSRF 抓取的地址、网络、缺失与超长分支。
func TestDeclarativeCSRFBranches(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		server  http.HandlerFunc
		message string
	}{
		{
			name:    "地址非法",
			path:    "/%zz",
			message: "站点链接无效",
		},
		{
			name: "抓取失败",
			path: "/csrf",
			server: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			message: "HTTP 500",
		},
		{
			name: "缺少 token",
			path: "/csrf",
			server: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html>no token here</html>`))
			},
			message: "站点 CSRF token 缺失",
		},
		{
			name: "token 超长",
			path: "/csrf",
			server: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<meta content="` + strings.Repeat("a", 4097) + `">`))
			},
			message: "站点 CSRF token 缺失",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseURL := "https://indexer.example.com"
			if tc.server != nil {
				baseURL = declarativeFlowServer(t, tc.server).URL
			}
			adapter := buildDeclarative(t, baseURL, func(definition *Definition) {
				definition.CSRFPath = tc.path
				definition.CSRFRegex = `content="([^"]+)"`
			})
			if _, err := adapter.csrf(context.Background(), http.Header{}); err == nil ||
				!strings.Contains(err.Error(), tc.message) {
				t.Fatalf("CSRF 错误 = %v，期望包含 %q", err, tc.message)
			}
		})
	}
}

// TestDeclarativeCSRFWithoutPathSkipsRequest 覆盖未配置 CSRF 时直接返回空 token。
func TestDeclarativeCSRFWithoutPathSkipsRequest(t *testing.T) {
	adapter := buildDeclarative(t, "https://indexer.example.com", nil)
	token, err := adapter.csrf(context.Background(), http.Header{})
	if err != nil || token != "" {
		t.Fatalf("未配置 CSRF 时 token=%q err=%v", token, err)
	}
}

// TestDeclarativeParseRSSItemsBranches 覆盖 RSS 解析的错误与跳过分支。
func TestDeclarativeParseRSSItemsBranches(t *testing.T) {
	adapter := buildDeclarative(t, "https://indexer.example.com", func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/rss"
	})

	if _, err := adapter.parseRSS(context.Background(), []byte("not a feed"), 10); err == nil ||
		!strings.Contains(err.Error(), "解析站点 RSS 失败") {
		t.Fatalf("非法 feed 错误 = %v", err)
	}

	feed := rssDocument(
		rssItem("g1", "某剧.S01E01", "/detail/1", "/download/1.torrent"),
		rssItem("", "", "/detail/2", ""),
		rssItem("g3", "某剧.S01E03", "/detail/3", ""),
	)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.parseRSSItems(canceled, []byte(feed), 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文错误 = %v", err)
	}

	items, err := adapter.parseRSSItems(context.Background(), []byte(feed), 10)
	if err != nil {
		t.Fatalf("解析 feed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("条目数 = %d，期望 2（空标题应被跳过）", len(items))
	}
	if items[0].Resource.DownloadID != items[0].Resource.DownloadURL || items[0].Resource.SizeBytes != 1<<30 {
		t.Fatalf("首条 = %+v", items[0].Resource)
	}
	// 没有 enclosure 的条目必须回落到详情地址作为下载定位符。
	if items[1].Resource.DownloadID != items[1].Resource.DetailURL {
		t.Fatalf("回落定位符 = %q，详情 = %q", items[1].Resource.DownloadID, items[1].Resource.DetailURL)
	}

	limited, err := adapter.parseRSSItems(context.Background(), []byte(feed), 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit 生效 = %d（%v）", len(limited), err)
	}

	// siteID < 1 时 RSSItemKey 为空，条目必须被丢弃而不是带空键落库。
	anonymous := buildDeclarative(t, "https://indexer.example.com", func(definition *Definition) {
		definition.ID = 0
		definition.Format = "rss"
		definition.RSSPath = "/rss"
	})
	keyless, err := anonymous.parseRSSItems(context.Background(), []byte(feed), 10)
	if err != nil || len(keyless) != 0 {
		t.Fatalf("无站点 ID 的条目 = %d（%v）", len(keyless), err)
	}
}

// TestDeclarativeFetchRSSForwardsValidators 覆盖条件请求、头部透传与响应元数据。
func TestDeclarativeFetchRSSForwardsValidators(t *testing.T) {
	var gotETag, gotModified, gotCookie, gotAgent, gotExtra string
	server := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotETag = r.Header.Get("If-None-Match")
		gotModified = r.Header.Get("If-Modified-Since")
		gotCookie = r.Header.Get("Cookie")
		gotAgent = r.Header.Get("User-Agent")
		gotExtra = r.Header.Get("X-Extra")
		w.Header().Set("ETag", `"feed-v1"`)
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		_, _ = w.Write([]byte(rssDocument(
			rssItem("g1", "某剧.S01E01", "/detail/1", "/download/1.torrent"),
			rssItem("g2", "某剧.S01E02", "/detail/2", ""),
		)))
	})
	adapter := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/rss?passkey=secret"
		definition.Cookie = "session=fixture"
		definition.UserAgent = "fixture-ua"
		definition.Headers = map[string]string{"X-Extra": "1"}
	})

	response, err := adapter.FetchRSS(context.Background(), RSSRequest{
		ETag: `"old"`, LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
	})
	if err != nil {
		t.Fatalf("抓取 RSS: %v", err)
	}
	if gotETag != `"old"` || gotModified == "" || gotCookie != "session=fixture" ||
		gotAgent != "fixture-ua" || gotExtra != "1" {
		t.Fatalf("条件请求头未透传: etag=%q modified=%q cookie=%q ua=%q extra=%q",
			gotETag, gotModified, gotCookie, gotAgent, gotExtra)
	}
	if response.ETag != `"feed-v1"` || response.LastModified == "" {
		t.Fatalf("响应元数据 = %+v", response)
	}
	if len(response.Items) != 2 {
		t.Fatalf("条目数 = %d", len(response.Items))
	}
	if response.Items[0].ItemKey == "" || response.Items[0].GUID != "g1" {
		t.Fatalf("条目标识 = %+v", response.Items[0])
	}
}

// TestDeclarativeFetchRSSNotModifiedAndFailures 覆盖 304、错误状态与解析失败。
func TestDeclarativeFetchRSSNotModifiedAndFailures(t *testing.T) {
	server := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rss":
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.Header().Set("ETag", `"v1"`)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write([]byte(rssDocument(rssItem("g1", "某剧.S01E01", "/detail/1", ""))))
		case "/broken":
			_, _ = w.Write([]byte("not a feed"))
		case "/boom":
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	})
	adapter := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/rss"
	})

	response, err := adapter.FetchRSS(context.Background(), RSSRequest{ETag: `"v1"`})
	if err != nil {
		t.Fatalf("304 抓取: %v", err)
	}
	if !response.NotModified || len(response.Items) != 0 || response.ETag != `"v1"` {
		t.Fatalf("304 响应 = %+v", response)
	}

	// limit 越界时必须收敛到上限而不是原样下传。
	for _, limit := range []int{0, -3, 201} {
		response, err := adapter.FetchRSS(context.Background(), RSSRequest{Limit: limit})
		if err != nil || len(response.Items) != 1 {
			t.Fatalf("limit=%d 抓取 = %d 条（%v）", limit, len(response.Items), err)
		}
	}

	adapter.def.RSSPath = "/broken"
	if _, err := adapter.FetchRSS(context.Background(), RSSRequest{}); err == nil ||
		!strings.Contains(err.Error(), "解析站点 RSS 失败") {
		t.Fatalf("损坏 feed 错误 = %v", err)
	}

	adapter.def.RSSPath = "/boom"
	var statusError *HTTPStatusError
	if _, err := adapter.FetchRSS(context.Background(), RSSRequest{}); !errors.As(err, &statusError) ||
		statusError.StatusCode != http.StatusBadGateway {
		t.Fatalf("错误状态 = %v", err)
	}
}

// TestDeclarativeFetchRSSGuards 覆盖 nil 适配器、未配置 RSS 地址与地址非法。
func TestDeclarativeFetchRSSGuards(t *testing.T) {
	var nilAdapter *DeclarativeAdapter
	if _, err := nilAdapter.FetchRSS(context.Background(), RSSRequest{}); err == nil ||
		!strings.Contains(err.Error(), "站点不是 RSS 适配器") {
		t.Fatalf("nil 适配器错误 = %v", err)
	}

	htmlOnly := buildDeclarative(t, "https://indexer.example.com", nil)
	if _, err := htmlOnly.FetchRSS(context.Background(), RSSRequest{}); err == nil ||
		!strings.Contains(err.Error(), "站点未配置 RSS 地址") {
		t.Fatalf("缺少 RSS 地址错误 = %v", err)
	}

	broken := buildDeclarative(t, "https://indexer.example.com", func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/%zz"
	})
	if _, err := broken.FetchRSS(context.Background(), RSSRequest{}); err == nil ||
		!strings.Contains(err.Error(), "站点链接无效") {
		t.Fatalf("非法 RSS 地址错误 = %v", err)
	}
}

// TestDeclarativeTestSiteBranches 覆盖连通性探测的守卫、回落与状态分支。
func TestDeclarativeTestSiteBranches(t *testing.T) {
	var nilAdapter *DeclarativeAdapter
	if err := nilAdapter.TestSite(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "站点适配器未初始化") {
		t.Fatalf("nil 适配器错误 = %v", err)
	}

	uninitialized := buildDeclarative(t, "https://indexer.example.com", nil)
	uninitialized.client = nil
	if err := uninitialized.TestSite(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "站点适配器未初始化") {
		t.Fatalf("缺少客户端错误 = %v", err)
	}

	var gotCookie, gotAgent, gotExtra, gotPath string
	server := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCookie = r.Header.Get("Cookie")
		gotAgent = r.Header.Get("User-Agent")
		gotExtra = r.Header.Get("X-Extra")
		switch r.URL.Path {
		case "/", "/rss":
			_, _ = w.Write([]byte("ok"))
		case "/empty":
			w.WriteHeader(http.StatusOK)
		case "/boom":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	})

	// 未配置 rss_path 时回落到站点根地址。
	rooted := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Cookie = "session=fixture"
		definition.UserAgent = "fixture-ua"
		definition.Headers = map[string]string{"X-Extra": "1"}
	})
	if err := rooted.TestSite(context.Background()); err != nil {
		t.Fatalf("根地址探测失败: %v", err)
	}
	if gotPath != "/" || gotCookie != "session=fixture" || gotAgent != "fixture-ua" || gotExtra != "1" {
		t.Fatalf("探测请求 = path=%q cookie=%q ua=%q extra=%q", gotPath, gotCookie, gotAgent, gotExtra)
	}

	rss := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/rss"
	})
	if err := rss.TestSite(context.Background()); err != nil || gotPath != "/rss" {
		t.Fatalf("RSS 探测 = path=%q err=%v", gotPath, err)
	}

	empty := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/empty"
	})
	var failure siteTestFailure
	if err := empty.TestSite(context.Background()); !errors.As(err, &failure) ||
		!strings.Contains(failure.Error(), "站点返回空响应") {
		t.Fatalf("空响应探测 = %v", err)
	}

	boom := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/boom"
	})
	var statusError *HTTPStatusError
	if err := boom.TestSite(context.Background()); !errors.As(err, &statusError) ||
		statusError.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("错误状态探测 = %v", err)
	}

	invalid := buildDeclarative(t, "https://indexer.example.com", func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/%zz"
	})
	if err := invalid.TestSite(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "站点地址无效") {
		t.Fatalf("非法探测地址 = %v", err)
	}
}

// TestDeclarativeSignInBranches 覆盖登录表单的字段映射、参数附加与失败分支。
func TestDeclarativeSignInBranches(t *testing.T) {
	var nilAdapter *DeclarativeAdapter
	if _, err := nilAdapter.SignIn(context.Background(), "u", "p"); err == nil ||
		!strings.Contains(err.Error(), "站点适配器未初始化") {
		t.Fatalf("nil 适配器错误 = %v", err)
	}

	missing := buildDeclarative(t, "https://indexer.example.com", nil)
	if _, err := missing.SignIn(context.Background(), "u", "p"); err == nil ||
		!strings.Contains(err.Error(), "signin_path") {
		t.Fatalf("缺少登录地址错误 = %v", err)
	}

	invalid := buildDeclarative(t, "https://indexer.example.com", func(definition *Definition) {
		definition.SigninPath = "/%zz"
	})
	if _, err := invalid.SignIn(context.Background(), "u", "p"); err == nil ||
		!strings.Contains(err.Error(), "站点登录地址无效") {
		t.Fatalf("非法登录地址错误 = %v", err)
	}

	var form map[string][]string
	var gotExtra, gotAgent string
	server := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析登录表单: %v", err)
		}
		form = r.PostForm
		gotExtra = r.Header.Get("X-Extra")
		gotAgent = r.Header.Get("User-Agent")
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "uid", Value: "42", Path: "/"})
		w.WriteHeader(http.StatusFound)
	})
	custom := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.SigninPath = "/login"
		definition.SigninUserKey = "email"
		definition.SigninPassKey = "pass"
		definition.SigninParams = map[string]string{"remember": "1"}
		definition.Headers = map[string]string{"X-Extra": "1"}
		definition.UserAgent = "fixture-ua"
	})
	cookies, err := custom.SignIn(context.Background(), "user@example.com", "secret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !strings.Contains(cookies, "sid=abc") || !strings.Contains(cookies, "uid=42") {
		t.Fatalf("登录 Cookie = %q", cookies)
	}
	if form["email"][0] != "user@example.com" || form["pass"][0] != "secret" || form["remember"][0] != "1" {
		t.Fatalf("登录表单 = %+v", form)
	}
	if gotExtra != "1" || gotAgent != "fixture-ua" {
		t.Fatalf("登录请求头 = extra=%q ua=%q", gotExtra, gotAgent)
	}

	// 未配置字段名时使用 username/password 默认值。
	serverDefault := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "x", Path: "/"})
	})
	fallback := buildDeclarative(t, serverDefault.URL, func(definition *Definition) {
		definition.SigninPath = "/login"
	})
	if _, err := fallback.SignIn(context.Background(), "user", "pass"); err != nil {
		t.Fatalf("默认字段登录失败: %v", err)
	}
	if form["username"][0] != "user" || form["password"][0] != "pass" {
		t.Fatalf("默认登录表单 = %+v", form)
	}

	forbidden := declarativeFlowServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	denied := buildDeclarative(t, forbidden.URL, func(definition *Definition) {
		definition.SigninPath = "/login"
	})
	var statusError *HTTPStatusError
	if _, err := denied.SignIn(context.Background(), "u", "p"); !errors.As(err, &statusError) ||
		statusError.StatusCode != http.StatusForbidden {
		t.Fatalf("登录被拒错误 = %v", err)
	}

	noCookie := declarativeFlowServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	missingCookie := buildDeclarative(t, noCookie.URL, func(definition *Definition) {
		definition.SigninPath = "/login"
	})
	if _, err := missingCookie.SignIn(context.Background(), "u", "p"); err == nil ||
		!strings.Contains(err.Error(), "站点未返回登录 Cookie") {
		t.Fatalf("缺少 Cookie 错误 = %v", err)
	}

	dead := declarativeFlowServer(t, func(http.ResponseWriter, *http.Request) {})
	deadURL := dead.URL
	dead.Close()
	unreachable := buildDeclarative(t, deadURL, func(definition *Definition) {
		definition.SigninPath = "/login"
	})
	if _, err := unreachable.SignIn(context.Background(), "u", "p"); err == nil ||
		!strings.Contains(err.Error(), "站点登录请求失败") {
		t.Fatalf("不可达登录错误 = %v", err)
	}
}

// TestDeclarativeRSSAdapterErrorPaths 覆盖 RSS 包装器的游标与下载错误分支。
func TestDeclarativeRSSAdapterErrorPaths(t *testing.T) {
	broken := buildDeclarative(t, "https://indexer.example.com", func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/%zz"
	})
	wrapped := &declarativeRSSAdapter{DeclarativeAdapter: broken}
	if key := wrapped.RSSCursorKey(); key != "" {
		t.Fatalf("非法 RSS 地址的游标键 = %q", key)
	}

	server := declarativeFlowServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	failing := &declarativeRSSAdapter{DeclarativeAdapter: buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "rss"
		definition.RSSPath = "/rss"
	})}
	if _, err := failing.ResolveDownload(context.Background(), "/download/1.torrent"); err == nil {
		t.Fatal("下载请求失败时应返回错误")
	}

	if _, err := failing.ResolveDownload(context.Background(), "https://evil.example/x"); err == nil {
		t.Fatal("跨主机下载地址应被拒绝")
	}
}

// TestNewDeclarativeAdapterRejectsInvalidHeaders 覆盖站点请求头的校验。
func TestNewDeclarativeAdapterRejectsInvalidHeaders(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	cases := []struct {
		name    string
		headers map[string]string
		message string
	}{
		{name: "受保护请求头", headers: map[string]string{"Host": "example.com"}, message: "站点请求头名称无效"},
		{name: "空名称", headers: map[string]string{" ": "x"}, message: "站点请求头名称无效"},
		{name: "换行值", headers: map[string]string{"X-A": "a\r\nb"}, message: "站点请求头值无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDeclarativeAdapter(client, Definition{
				Name: "站", BaseURL: "https://indexer.example.com", Format: "html",
				ResultSelector: ".row", Headers: tc.headers,
			})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.message)
			}
		})
	}
}

// TestDeclarativeSearchCSRFFetchFails 覆盖搜索前抓取 CSRF 失败。
func TestDeclarativeSearchCSRFFetchFails(t *testing.T) {
	var searchHits atomic.Int32
	server := declarativeFlowServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/csrf":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/search":
			searchHits.Add(1)
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	})
	adapter := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.Format = "json"
		definition.Method = http.MethodPost
		definition.Body = map[string]any{"kw": "${keyword}"}
		definition.ResultPath = "items"
		definition.CSRFPath = "/csrf"
		definition.CSRFRegex = `content="([^"]+)"`
	})
	var statusError *HTTPStatusError
	if _, err := adapter.Search(context.Background(), Query{Keyword: "x"}); !errors.As(err, &statusError) ||
		statusError.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("搜索前 CSRF 失败 = %v", err)
	}
	if searchHits.Load() != 0 {
		t.Fatalf("CSRF 失败后仍发出搜索请求 %d 次", searchHits.Load())
	}
}

// TestDeclarativeTestSiteTransportFailure 覆盖探测请求本身的网络失败。
func TestDeclarativeTestSiteTransportFailure(t *testing.T) {
	server := declarativeFlowServer(t, func(http.ResponseWriter, *http.Request) {})
	adapter := buildDeclarative(t, server.URL, func(definition *Definition) {
		definition.RSSPath = "/rss"
	})
	server.Close()
	if err := adapter.TestSite(context.Background()); err == nil {
		t.Fatal("不可达站点的探测应返回错误")
	}
}

// TestRenderBodyPropagatesSliceErrors 覆盖数组模板里的嵌套错误。
func TestRenderBodyPropagatesSliceErrors(t *testing.T) {
	_, err := renderBody([]any{"ok", map[string]any{"": "x"}}, Query{}, 0)
	if err == nil || !strings.Contains(err.Error(), "字段名无效") {
		t.Fatalf("数组模板错误 = %v", err)
	}
}
