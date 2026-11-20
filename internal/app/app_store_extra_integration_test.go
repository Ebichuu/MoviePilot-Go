//go:build integration

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
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestCookieCloudSiteDirectoryIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	mustUpsertAppSite(t, store, database.Site{
		Name: "RSS站", Domain: "a.example", URL: "https://a.example", Adapter: "rss", Enabled: true,
		Config: map[string]any{"rss": "/feed", "public": true},
	})
	directory := &cookieCloudSiteDirectory{store: store}

	entries, err := directory.entries(ctx)
	if err != nil {
		t.Fatalf("entries() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "RSS站" || entries[0].Domain != "a.example" ||
		entries[0].Adapter != "rss" {
		t.Fatalf("站点目录条目 = %#v", entries)
	}
	if _, err := directory.directory(ctx); err != nil {
		t.Fatalf("directory() error = %v", err)
	}

	// 分类清单对 RSS 站点没有可枚举的分类，但不能因此报错。
	if _, _, err := directory.SiteCategories(ctx, "a.example"); err != nil {
		t.Fatalf("SiteCategories() error = %v", err)
	}

	entry, found, err := directory.IndexerByDomain(ctx, "a.example")
	if err != nil || !found {
		t.Fatalf("IndexerByDomain() = (%+v, %t, %v)", entry, found, err)
	}
	if entry.Name != "RSS站" || entry.Domain != "a.example" || entry.Adapter != "rss" || !entry.Public {
		t.Fatalf("Indexer = %+v", entry)
	}
	// 未收录的域名返回 found=false 而不是错误。
	if _, found, err := directory.IndexerByDomain(ctx, "missing.example"); err != nil || found {
		t.Fatalf("未知域名 = (found=%t, err=%v)", found, err)
	}
}

func TestCookieCloudSiteStoreCacheSiteIconIntegration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nicon-bytes"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>站点</title></head></html>`))
	}))
	t.Cleanup(server.Close)

	client, err := indexer.NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	client.SetAllowPrivateHosts(true)

	store := newAppIntegrationStore(t)
	mustUpsertAppSite(t, store, database.Site{
		Name: "图标站", Domain: "a.example", URL: server.URL, Adapter: "rss", Enabled: true,
		Config: map[string]any{"rss": "/feed"},
	})
	// UpdateCookie 的 refreshIcon=true 分支会顺带抓取图标并写入 sites.config._site_icon。
	siteStore := &cookieCloudSiteStore{store: store, siteHTTP: client}
	if err := siteStore.UpdateCookie(t.Context(), "a.example", "uid=1"); err != nil {
		t.Fatalf("UpdateCookie() error = %v", err)
	}
	icon, iconURL, found, err := store.SiteIconByDomain(t.Context(), "a.example")
	if err != nil || !found {
		t.Fatalf("SiteIconByDomain() = (found=%t, err=%v)", found, err)
	}
	if iconURL != server.URL+"/favicon.ico" || !strings.HasPrefix(icon, "iVBOR") {
		t.Fatalf("图标 = (%q, %q)", iconURL, icon)
	}
}

func TestTelegramServicesSubscriptionLinesIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	// 订阅对 users 有外键，先建账号。
	for _, username := range []string{"alice", "bob"} {
		if _, err := store.CreateUser(ctx, username, "hash", false, map[string]any{}); err != nil {
			t.Fatalf("创建用户 %q: %v", username, err)
		}
	}

	for _, input := range []database.NewSubscription{
		{
			Username: "alice", Name: "某剧", MediaType: "电视剧", Year: "2024",
			MediaSource: "tmdb", MediaID: "/tv/1", State: subscription.PendingSearch,
			Config: map[string]any{},
		},
		{
			Username: "alice", Name: "某电影", MediaType: "电影",
			MediaSource: "tmdb", MediaID: "/movie/2", State: subscription.PendingSearch,
			Config: map[string]any{},
		},
	} {
		if _, err := store.CreateSubscription(ctx, input); err != nil {
			t.Fatalf("创建订阅 %q: %v", input.Name, err)
		}
	}

	services := newTelegramServices(nil, nil, store, "alice")
	lines, err := services.Subscriptions(ctx)
	if err != nil {
		t.Fatalf("Subscriptions() error = %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("订阅行 = %#v", lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "某剧 (2024)") || !strings.Contains(joined, "电视剧") {
		t.Fatalf("订阅行内容 = %q", joined)
	}
	// 没有年份时不拼空括号。
	if !strings.Contains(joined, "某电影") || strings.Contains(joined, "某电影 ()") {
		t.Fatalf("无年份订阅行 = %q", joined)
	}

	// 机器人以管理员视角列出全部订阅（`ListSubscriptions(..., true)`）：
	// 账号名只用于确定操作者，不改变可见范围。
	other := newTelegramServices(nil, nil, store, "bob")
	otherLines, err := other.Subscriptions(ctx)
	if err != nil {
		t.Fatalf("bob Subscriptions() error = %v", err)
	}
	if len(otherLines) != len(lines) {
		t.Fatalf("管理员视角订阅行 = %#v, want %#v", otherLines, lines)
	}
}

func TestCookieCloudSiteStoreUnknownDomainIsNoopIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	siteStore := &cookieCloudSiteStore{store: store}
	// 完全没有站点时，任何域名的更新都是 no-op 且不写脏标记。
	if err := siteStore.UpdateCookie(t.Context(), "a.example", "x"); err != nil {
		t.Fatalf("UpdateCookie() error = %v", err)
	}
	calls := 0
	reload := func(context.Context) error { calls++; return nil }
	if err := siteStore.reloadIfChanged(t.Context(), reload); err != nil || calls != 0 {
		t.Fatalf("no-op 后重载 = (%v, calls=%d)", err, calls)
	}
	if _, found, err := siteStore.SiteByDomain(t.Context(), cookiecloud.URLDomain("a.example")); err != nil || found {
		t.Fatalf("空库 SiteByDomain = (found=%t, err=%v)", found, err)
	}
}
