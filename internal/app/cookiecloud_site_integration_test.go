//go:build integration

package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/cookiecloud"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// TestCookieCloudSiteDirectoryExtDomainsIntegration 覆盖站点目录里
// 「定义域名与站点域名不一致时补成扩展域名」的分支。
func TestCookieCloudSiteDirectoryExtDomainsIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	// 配置里没有 domain：站点域名应补成扩展域名。
	if _, err := store.UpsertSite(ctx, database.Site{
		Name: "Ext Domain", Domain: "ext.example.com", URL: "https://ext.example.com",
		Adapter: "mteam", Enabled: true, Config: map[string]any{"domain": "other.example.com"},
	}); err != nil {
		t.Fatalf("写入扩展域名站点: %v", err)
	}
	// 配置里的 domain 与站点域名一致：不重复补。
	if _, err := store.UpsertSite(ctx, database.Site{
		Name: "Same Domain", Domain: "same.example.com", URL: "https://same.example.com",
		Adapter: "mteam", Enabled: true,
		Config: map[string]any{"domain": "same.example.com"},
	}); err != nil {
		t.Fatalf("写入同域名站点: %v", err)
	}

	directory := &cookieCloudSiteDirectory{store: store}
	entries, err := directory.entries(ctx)
	if err != nil {
		t.Fatalf("entries() error = %v", err)
	}
	byName := make(map[string]bool, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = true
		if entry.Name == "Ext Domain" {
			if len(entry.ExtDomains) != 1 || entry.ExtDomains[0] != "ext.example.com" {
				t.Fatalf("扩展域名 = %#v，期望 [ext.example.com]", entry.ExtDomains)
			}
		}
		if entry.Name == "Same Domain" && len(entry.ExtDomains) != 0 {
			t.Fatalf("同域名不应补扩展域名：%#v", entry.ExtDomains)
		}
	}
	if !byName["Ext Domain"] || !byName["Same Domain"] {
		t.Fatalf("目录条目 = %#v", byName)
	}

	// 目录整体可用：站点分类与按域名查找都要能查到刚写入的站点。
	if _, err := directory.directory(ctx); err != nil {
		t.Fatalf("directory() error = %v", err)
	}
	if _, _, err := directory.SiteCategories(ctx, "ext.example.com"); err != nil {
		t.Fatalf("SiteCategories() error = %v", err)
	}
}

// TestCookieCloudAddSiteCachesFaviconIntegration 覆盖新增站点时的图标抓取分支
// 与配置合并，以及站点域名匹配后按域名取回运行时配置。
func TestCookieCloudAddSiteCachesFaviconIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	icon := []byte("fake-icon-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			_, _ = w.Write(icon)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><head><link rel="icon" href="/favicon.ico"></head><body>x</body></html>`)
	}))
	t.Cleanup(server.Close)

	siteStore := &cookieCloudSiteStore{store: store, siteHTTP: newCookieCloudSiteHTTPClient(t)}
	// 站点域名按 URL 形式保存，才符合 CookieCloud 载荷里 domain 的形态
	// （URLDomain 会从里面取出主机名）。
	if err := siteStore.AddSite(ctx, cookiecloud.Site{
		Name: "Favicon Site", Domain: "fav.example.com", URL: server.URL,
		Adapter: "mteam", Cookie: "cookie-value", Public: true, RSS: "https://fav.example.com/rss",
	}); err != nil {
		t.Fatalf("AddSite() error = %v", err)
	}
	if !siteStore.dirty {
		t.Fatal("新增站点后必须标记脏，等下一轮重载站点")
	}

	sites, err := store.ListSites(ctx)
	if err != nil {
		t.Fatalf("ListSites() error = %v", err)
	}
	added := false
	for _, site := range sites {
		if site.Name != "Favicon Site" {
			continue
		}
		added = true
		// cookie 存在独立加密列里，ListSites 不会回填，只断言非敏感字段。
		if site.Config["public"] != true || site.Config["rss"] != "https://fav.example.com/rss" {
			t.Fatalf("新增站点配置 = %#v", site.Config)
		}
		if _, ok := site.Config["_site_icon"]; !ok {
			t.Fatalf("应写入抓到的站点图标：%#v", site.Config)
		}
	}
	if !added {
		t.Fatalf("站点未写入：%#v", sites)
	}

	// 已存在的站点按域名可以取回运行时配置。
	found, ok, err := siteStore.SiteByDomain(ctx, cookiecloud.URLDomain("fav.example.com"))
	if err != nil || !ok {
		t.Fatalf("SiteByDomain() = (%v, %v, %v)", found, ok, err)
	}
	if found.Name != "Favicon Site" || found.Cookie != "cookie-value" {
		t.Fatalf("SiteByDomain() = %#v", found)
	}

	// 更新 Cookie 成功后会再次标记脏并刷新图标。
	siteStore.dirty = false
	if err := siteStore.UpdateCookie(ctx, cookiecloud.URLDomain("fav.example.com"), "new-cookie"); err != nil {
		t.Fatalf("UpdateCookie() error = %v", err)
	}
	if !siteStore.dirty {
		t.Fatal("更新 Cookie 后应标记脏")
	}
	updated, ok, err := siteStore.SiteByDomain(ctx, cookiecloud.URLDomain("fav.example.com"))
	if err != nil || !ok || updated.Cookie != "new-cookie" {
		t.Fatalf("更新后的站点 = (%#v, %v, %v)", updated, ok, err)
	}
}

// TestCookieCloudAddSiteSerializeFailureIntegration 覆盖新增站点时配置无法序列化的错误出口。
func TestCookieCloudAddSiteSerializeFailureIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	siteStore := &cookieCloudSiteStore{store: store}
	err := siteStore.AddSite(context.Background(), cookiecloud.Site{
		Name: "Broken Config", Domain: "broken.example.com", URL: "https://broken.example.com",
		Adapter: "mteam", Config: map[string]any{"bad": make(chan int)},
	})
	if err == nil {
		t.Fatal("配置无法序列化时 AddSite 应报错")
	}
}
