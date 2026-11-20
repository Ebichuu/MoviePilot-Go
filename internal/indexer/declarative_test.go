package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDeclarativeHTMLAdapterParsesTorrentRows(t *testing.T) {
	var requestURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURL = r.URL.String()
		if r.Header.Get("Cookie") != "session=fixture" || r.Header.Get("User-Agent") != "fixture-agent" {
			t.Errorf("站点请求头未按定义发送: cookie=%q ua=%q", r.Header.Get("Cookie"), r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><table><tr class="result"><td><a class="title">Example S01E01</a><a class="detail" href="/torrent/1">详情</a><a class="download" href="/download/1">下载</a><span class="size">1.5 GiB</span><span class="seeders">12</span><span class="peers">3</span><time class="published">2026-08-10 12:30:00</time></td></tr></table></body></html>`))
	}))
	defer server.Close()

	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 2), Definition{
		ID: 7, Name: "fixture-html", BaseURL: server.URL, SearchPath: "/search",
		Format: "html", QueryParam: "q", PageParam: "page", ResultSelector: "tr.result",
		Fields: Fields{
			Title: Field{Selector: "a.title"}, DetailURL: Field{Selector: "a.detail", Attribute: "href"},
			DownloadURL: Field{Selector: "a.download", Attribute: "href"}, Size: Field{Selector: ".size"},
			Seeders: Field{Selector: ".seeders"}, Peers: Field{Selector: ".peers"}, PublishedAt: Field{Selector: ".published"},
		}, Cookie: "session=fixture", UserAgent: "fixture-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := adapter.Search(context.Background(), Query{Keyword: "Example", Page: 2, Limit: 20})
	if err != nil || len(items) != 1 {
		t.Fatalf("HTML 站点解析失败: items=%#v err=%v", items, err)
	}
	item := items[0]
	if item.SiteID != 7 || item.Title != "Example S01E01" || item.DetailURL != server.URL+"/torrent/1" || item.DownloadURL != server.URL+"/download/1" {
		t.Fatalf("HTML URL/标题映射错误: %#v", item)
	}
	if item.SizeBytes != 1610612736 || item.Seeders != 12 || item.Peers != 3 || item.PublishedAt.Year() != 2026 {
		t.Fatalf("HTML 数值/时间映射错误: %#v", item)
	}
	if !strings.Contains(requestURL, "q=Example") || !strings.Contains(requestURL, "page=2") {
		t.Fatalf("搜索参数未编码: %s", requestURL)
	}
}

func TestDeclarativeRSSAdapterParsesFeedItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Fixture</title><item><title>Example Movie 2026</title><link>/torrent/2</link><guid>2</guid><description>release</description><pubDate>Mon, 10 Aug 2026 12:30:00 GMT</pubDate><enclosure url="/download/2" length="1024" type="application/x-bittorrent"/></item></channel></rss>`))
	}))
	defer server.Close()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		ID: 8, Name: "fixture-rss", BaseURL: server.URL, SearchPath: "/rss", Format: "rss",
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := adapter.Search(context.Background(), Query{Keyword: "Example", Limit: 10})
	if err != nil || len(items) != 1 {
		t.Fatalf("RSS 站点解析失败: items=%#v err=%v", items, err)
	}
	if items[0].TorrentID != "" || items[0].DetailURL != server.URL+"/torrent/2" ||
		items[0].DownloadURL != server.URL+"/download/2" || items[0].Title != "Example Movie 2026" {
		t.Fatalf("RSS 字段映射错误: %#v", items[0])
	}
}

func TestDeclarativeJSONAdapterParsesNestedResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"torrents": []any{map[string]any{
			"name": "Example", "id": 42, "size": 2048, "seeders": 9, "leechers": 1,
		}}}})
	}))
	defer server.Close()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		ID: 9, Name: "fixture-json", BaseURL: server.URL, SearchPath: "/api/search", Format: "json",
		ResultPath: "data.torrents", Fields: Fields{
			Title: Field{Selector: "name"}, DownloadURL: Field{Selector: "id"},
			ResourceID: Field{Selector: "id"},
			Size:       Field{Selector: "size"}, Seeders: Field{Selector: "seeders"}, Peers: Field{Selector: "leechers"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := adapter.Search(context.Background(), Query{Keyword: "Example", Limit: 10})
	if err != nil || len(items) != 1 {
		t.Fatalf("JSON 站点解析失败: items=%#v err=%v", items, err)
	}
	if items[0].Title != "Example" || items[0].TorrentID != "42" || items[0].DownloadID != "42" ||
		items[0].DownloadURL != server.URL+"/42" || items[0].Seeders != 9 {
		t.Fatalf("JSON 字段映射错误: %#v", items[0])
	}
}

func TestDeclarativePOSTJSONRendersBoundedTemplate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "fixture-key" {
			t.Fatalf("POST 请求方法或鉴权头错误: method=%s", r.Method)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("POST template unexpectedly received default query parameters: %s", r.URL.RawQuery)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		page := body["pageParam"].(map[string]any)
		if body["keyword"] != "Example" || page["current"] != float64(2) || page["pageSize"] != float64(20) {
			t.Fatalf("POST 请求模板渲染错误: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
			"showName": "Example S01E03", "fileSize": 4096, "seedNum": 11,
		}}})
	}))
	defer server.Close()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		ID: 10, Name: "fixture-post", BaseURL: server.URL, SearchPath: "/api/search", Format: "json", Method: http.MethodPost,
		ResultPath: "data", Headers: map[string]string{"Authorization": "fixture-key"},
		Body:   map[string]any{"keyword": "${keyword}", "pageParam": map[string]any{"current": "${page}", "pageSize": "${limit}"}},
		Fields: Fields{Title: Field{Selector: "showName"}, Size: Field{Selector: "fileSize"}, Seeders: Field{Selector: "seedNum"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := adapter.Search(context.Background(), Query{Keyword: "Example", Page: 2, Limit: 20})
	if err != nil || len(items) != 1 || items[0].SizeBytes != 4096 || items[0].Seeders != 11 {
		t.Fatalf("POST JSON 站点解析失败: items=%#v err=%v", items, err)
	}
}

func TestDeclarativePOSTCachesCSRFToken(t *testing.T) {
	var tokenRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			tokenRequests++
			_, _ = w.Write([]byte(`<meta name="x-csrf-token" content="fixture-token">`))
		case "/api/search":
			if r.Header.Get("X-CSRF-TOKEN") != "fixture-token" {
				t.Fatalf("CSRF 请求头缺失: %q", r.Header.Get("X-CSRF-TOKEN"))
			}
			_, _ = w.Write([]byte(`{"data":{"torrents":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		Name: "fixture-csrf", BaseURL: server.URL, SearchPath: "/api/search", Format: "json", Method: http.MethodPost,
		ResultPath: "data.torrents", Body: map[string]any{"keyword": "${keyword}"},
		CSRFPath: "/", CSRFRegex: `name="x-csrf-token" content="([^"]+)"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := adapter.Search(context.Background(), Query{Keyword: "Example"}); err != nil {
			t.Fatal(err)
		}
	}
	if tokenRequests != 1 {
		t.Fatalf("CSRF token 未复用: requests=%d", tokenRequests)
	}
}

func TestDeclarativeJSONRejectsTrailingDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]} {"unexpected":true}`))
	}))
	defer server.Close()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		Name: "fixture-trailing", BaseURL: server.URL, Format: "json", ResultPath: "data",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Search(context.Background(), Query{}); err == nil {
		t.Fatal("JSON 尾随文档未被拒绝")
	}
}

func TestDeclarativeAdapterRejectsUnsafeDefinition(t *testing.T) {
	_, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		Name: "unsafe", BaseURL: "file:///tmp/site", Format: "html",
	})
	if err == nil {
		t.Fatal("不安全站点地址未被拒绝")
	}
}

func TestNewAdapterFromConfigRejectsLegacyParserAndLoadsDeclarativeConfig(t *testing.T) {
	client := newIndexerHTTPClient(t, 1)
	if _, err := NewAdapterFromConfig(client, 1, "legacy", "https://example.test", "TNodeSpider", nil); err == nil {
		t.Fatal("旧 Python parser 不应被伪装为声明式适配器")
	}
	adapter, err := NewAdapterFromConfig(client, 2, "rss", "https://example.test", "rss", map[string]any{
		"search_path": "/feed", "query_param": "keyword",
	})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Name() != "rss" || adapter.(SiteIDAdapter).SiteID() != 2 {
		t.Fatalf("站点配置装配错误: %#v", adapter)
	}
}

func newIndexerHTTPClient(t *testing.T, workers int) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(workers, time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	client.allowPrivate = true
	return client
}

func TestDeclarativeAdapterTestSiteProbesConfiguredEndpoint(t *testing.T) {
	var cookieSeen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.rss":
			cookieSeen = r.Header.Get("Cookie")
			_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>Zmpt</title></channel></rss>`))
		case "/empty.rss":
			w.WriteHeader(http.StatusOK)
		case "/boom.rss":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	build := func(rssPath string) *DeclarativeAdapter {
		t.Helper()
		adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
			ID: 1, Name: "Zmpt", BaseURL: server.URL, Format: "html",
			SearchPath: "/torrents.php", ResultSelector: "table.torrents tr",
			RSSPath: rssPath, Cookie: "c_secure_pass=secret",
		})
		if err != nil {
			t.Fatalf("NewDeclarativeAdapter() error = %v", err)
		}
		return adapter
	}

	if err := build("/feed.rss").TestSite(t.Context()); err != nil {
		t.Fatalf("TestSite() error = %v", err)
	}
	if cookieSeen != "c_secure_pass=secret" {
		t.Fatalf("探测请求未携带站点 Cookie：%q", cookieSeen)
	}

	var emptyFailure siteTestFailure
	if err := build("/empty.rss").TestSite(t.Context()); !errors.As(err, &emptyFailure) {
		t.Fatalf("空响应 TestSite() error = %v, want siteTestFailure", err)
	}

	var statusError *HTTPStatusError
	if err := build("/boom.rss").TestSite(t.Context()); !errors.As(err, &statusError) ||
		statusError.StatusCode != http.StatusInternalServerError {
		t.Fatalf("5xx TestSite() error = %v, want HTTPStatusError 500", err)
	}
}

func TestDeclarativeAdapterSignInCapturesCookies(t *testing.T) {
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/takelogin.php" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotForm = r.PostForm
		http.SetCookie(w, &http.Cookie{Name: "c_secure_uid", Value: "NzM", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "c_secure_pass", Value: "abc123", Path: "/"})
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	newAdapter := func(signinPath string) *DeclarativeAdapter {
		t.Helper()
		adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
			ID: 1, Name: "Zmpt", BaseURL: server.URL, Format: "html",
			SearchPath: "/torrents.php", ResultSelector: "table tr",
			SigninPath: signinPath,
		})
		if err != nil {
			t.Fatalf("NewDeclarativeAdapter() error = %v", err)
		}
		return adapter
	}

	cookie, err := newAdapter("/takelogin.php").SignIn(t.Context(), "Ebichu", "secret")
	if err != nil {
		t.Fatalf("SignIn() error = %v", err)
	}
	if gotForm.Get("username") != "Ebichu" || gotForm.Get("password") != "secret" {
		t.Fatalf("登录表单 = %v, want username/password 字段", gotForm)
	}
	if !strings.Contains(cookie, "c_secure_pass=abc123") || !strings.Contains(cookie, "c_secure_uid=NzM") {
		t.Fatalf("SignIn() cookie = %q, want 站点下发 Cookie", cookie)
	}

	if _, err := newAdapter("").SignIn(t.Context(), "Ebichu", "secret"); err == nil {
		t.Fatal("未配置 signin_path 时 SignIn() 必须如实拒绝")
	}
}
