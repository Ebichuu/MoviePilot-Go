package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newRSSFixture(t *testing.T, id int, baseURL, rssPath string) *declarativeRSSAdapter {
	t.Helper()
	adapter, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		ID: id, Name: "rss-fixture", BaseURL: baseURL, SearchPath: "/search", RSSPath: rssPath,
		Format: "rss", Cookie: "session=fixture", UserAgent: "fixture-ua",
		Headers: map[string]string{"X-Extra": "1"},
	})
	if err != nil {
		t.Fatalf("构造声明式适配器失败: %v", err)
	}
	return &declarativeRSSAdapter{DeclarativeAdapter: adapter}
}

func TestDeclarativeRSSCapabilityGuards(t *testing.T) {
	var nilOuter *declarativeRSSAdapter
	if nilOuter.rssCapable() {
		t.Fatal("nil 适配器不应声称具备 RSS 能力")
	}
	if nilOuter.RSSCursorKey() != "" {
		t.Fatal("nil 适配器的游标键应为空")
	}
	// 只有外层没有内层（构造过程中的半成品）同样不能声称能力。
	if (&declarativeRSSAdapter{}).rssCapable() {
		t.Fatal("缺少内层适配器时不应具备 RSS 能力")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("没有 RSS 地址时不应发起请求")
	}))
	defer server.Close()

	// HTML 站点没有 rss_path：必须保持普通适配器。注意 RSS 格式的站点会把
	// search_path 当作 feed 地址，所以这里只能用 HTML 站点来验证「无 RSS」。
	html, err := NewDeclarativeAdapter(newIndexerHTTPClient(t, 1), Definition{
		ID: 1, Name: "html-no-rss", BaseURL: server.URL, SearchPath: "/search", Format: "html",
		QueryParam: "q", ResultSelector: "tr.result",
		Fields: Fields{
			Title:       Field{Selector: "a.title"},
			DownloadURL: Field{Selector: "a.download", Attribute: "href"},
		},
	})
	if err != nil {
		t.Fatalf("构造 HTML 适配器失败: %v", err)
	}
	wrapped := &declarativeRSSAdapter{DeclarativeAdapter: html}
	if wrapped.rssCapable() {
		t.Fatal("没有 rss_path 的 HTML 站点不应具备 RSS 能力")
	}
	if wrapped.RSSCursorKey() != "" {
		t.Fatalf("没有 RSS 能力时游标键应为空，实际 %q", wrapped.RSSCursorKey())
	}
	// 没有 RSS 地址的站点不能提供下载解析器，否则会拿搜索页当种子返回。
	if _, err := wrapped.ResolveDownload(context.Background(), "/download/1.torrent"); err == nil {
		t.Fatal("没有 RSS 地址时应拒绝解析下载")
	}
}

func TestDeclarativeRSSCursorKeyIsStableAndOpaque(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()

	first := newRSSFixture(t, 7, server.URL, "/rss/filter?passkey=secret-key")
	key := first.RSSCursorKey()
	if !strings.HasPrefix(key, "sha256:") {
		t.Fatalf("游标键格式 = %q", key)
	}
	if len(key) != len("sha256:")+64 {
		t.Fatalf("游标键长度 = %d", len(key))
	}
	// 游标键必须稳定：同一站点同一地址每次算出来一样，否则会重复抓取整份 feed。
	if again := first.RSSCursorKey(); again != key {
		t.Fatalf("游标键不稳定: %q != %q", again, key)
	}
	// 且必须是不透明的：不能把地址、主机或 passkey 泄进持久化表。
	for _, secret := range []string{"127.0.0.1", "secret-key", "/rss/"} {
		if strings.Contains(key, secret) {
			t.Fatalf("游标键泄露了 %q: %q", secret, key)
		}
	}

	// 站点 ID 或 RSS 地址变化都必须换键，否则不同站点会共享游标。
	if other := newRSSFixture(t, 8, server.URL, "/rss/filter?passkey=secret-key").RSSCursorKey(); other == key {
		t.Fatal("不同站点 ID 应产生不同游标键")
	}
	if other := newRSSFixture(t, 7, server.URL, "/rss/other").RSSCursorKey(); other == key {
		t.Fatal("不同 RSS 地址应产生不同游标键")
	}
}

func TestDeclarativeRSSResolverIdentity(t *testing.T) {
	adapter := newRSSFixture(t, 3, "https://site.example", "/rss")
	if got := adapter.ResolverKind(); got != "declarative-rss" {
		t.Fatalf("解析器类型 = %q", got)
	}
	if got := adapter.ResolverVersion(); got != downloadResolverVersionV1 {
		t.Fatalf("解析器版本 = %d", got)
	}
}

func TestDeclarativeResolveDownloadForwardsCredentials(t *testing.T) {
	var gotCookie, gotAgent, gotExtra string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotAgent = r.Header.Get("User-Agent")
		gotExtra = r.Header.Get("X-Extra")
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write([]byte("d8:announce4:http"))
	}))
	defer server.Close()

	adapter := newRSSFixture(t, 1, server.URL, "/rss")
	resolved, err := adapter.ResolveDownload(context.Background(), "/download/1.torrent")
	if err != nil {
		t.Fatalf("解析下载失败: %v", err)
	}
	if string(resolved.Body) != "d8:announce4:http" {
		t.Fatalf("正文 = %q", resolved.Body)
	}
	// 种子必须按 application/x-bittorrent 交给下游，而不是让下游猜。
	if resolved.ContentType != "application/x-bittorrent" {
		t.Fatalf("Content-Type = %q", resolved.ContentType)
	}
	if gotCookie != "session=fixture" || gotAgent != "fixture-ua" || gotExtra != "1" {
		t.Fatalf("请求头未透传: cookie=%q ua=%q extra=%q", gotCookie, gotAgent, gotExtra)
	}
}

func TestDeclarativeResolveDownloadRejectsCrossHost(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
	}))
	defer server.Close()

	adapter := newRSSFixture(t, 1, server.URL, "/rss")
	// 资源 ID 可能来自站点 HTML，必须限制在同主机内，否则就是 SSRF。
	for _, resourceID := range []string{"https://evil.example/x.torrent", "ftp://evil.example/x"} {
		if _, err := adapter.ResolveDownload(context.Background(), resourceID); err == nil {
			t.Fatalf("跨主机资源 %q 应被拒绝", resourceID)
		}
	}
	if hits != 0 {
		t.Fatalf("被拒绝的资源不应产生请求，实际 %d 次", hits)
	}
}

func TestDeclarativeRSSNoCachePolicy(t *testing.T) {
	mteam := newRSSFixture(t, 1, "https://kp.m-team.cc", "/rss")
	if !mteam.RSSNoCache() {
		t.Fatal("m-team 站点应声明不走缓存")
	}
	other := newRSSFixture(t, 1, "https://site.example", "/rss")
	if other.RSSNoCache() {
		t.Fatal("其它站点不应声明不走缓存")
	}
	var nilAdapter *DeclarativeAdapter
	if nilAdapter.RSSNoCache() {
		t.Fatal("nil 适配器不应声明不走缓存")
	}
}
