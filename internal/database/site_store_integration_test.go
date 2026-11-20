//go:build integration

package database

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// TestSiteStorePostgresCRUDIntegration 覆盖站点读写面：加密分离、公开视图、
// 优先级排序、重名冲突、图标缓存与级联清理。
func TestSiteStorePostgresCRUDIntegration(t *testing.T) {
	base := newSiteStatisticsIntegrationStore(t)
	box, err := auth.NewSecretBox("moviepilot-site-store-integration-key")
	if err != nil {
		t.Fatalf("创建敏感配置加密器: %v", err)
	}
	// 站点读写要求加密器可用，基础 helper 只给公开配置，这里换成带 SecretBox 的 Store。
	store := NewStore(base.db, box)
	ctx := t.Context()
	if store.Pool() == nil {
		t.Fatal("Store.Pool() = nil")
	}

	id, err := store.UpsertSite(ctx, Site{
		Name:    "站点A",
		Domain:  "A.Example.com",
		URL:     "https://a.example.com",
		Adapter: "mteam",
		Enabled: true,
		Config: map[string]any{
			"cookie": "secret-cookie",
			"apikey": "secret-apikey",
			"pri":    10,
			"note":   "public",
		},
	})
	if err != nil {
		t.Fatalf("UpsertSite() error = %v", err)
	}
	if id <= 0 {
		t.Fatalf("UpsertSite() id = %d", id)
	}

	// 同名再插入必须冲突，且不覆盖已存在站点。
	if _, err := store.UpsertSite(ctx, Site{
		Name: "站点A", Domain: "other.example.com", URL: "https://other.example.com",
		Adapter: "mteam", Enabled: true, Config: map[string]any{"pri": 1},
	}); !errors.Is(err, ErrSiteConflict) {
		t.Fatalf("重名 UpsertSite() error = %v, want ErrSiteConflict", err)
	}

	sites, err := store.ListSites(ctx)
	if err != nil {
		t.Fatalf("ListSites() error = %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("ListSites() = %d 条, want 1", len(sites))
	}
	public := sites[0]
	if public.Config["cookie"] != nil || public.Config["apikey"] != nil {
		t.Fatalf("公开视图泄漏敏感配置: %#v", public.Config)
	}
	if public.Config["note"] != "public" {
		t.Fatalf("公开视图 config = %#v", public.Config)
	}
	if public.Domain != "A.Example.com" {
		t.Fatalf("站点域名 = %q", public.Domain)
	}

	runtimeSite, err := store.RuntimeSiteByID(ctx, id)
	if err != nil {
		t.Fatalf("RuntimeSiteByID() error = %v", err)
	}
	if runtimeSite.Config["cookie"] != "secret-cookie" || runtimeSite.Config["apikey"] != "secret-apikey" {
		t.Fatalf("运行时配置未解密: %#v", runtimeSite.Config)
	}

	// 图标缓存写在 sites.config._site_icon，不需要额外迁移。
	if err := store.UpdateSiteIcon(ctx, "https://a.example.com/some/path", "https://a.example.com/favicon.ico", ""); err != nil {
		t.Fatalf("UpdateSiteIcon() error = %v", err)
	}
	iconBase64, iconURL, found, err := store.SiteIconByDomain(ctx, "A.EXAMPLE.COM")
	if err != nil || !found || iconURL != "https://a.example.com/favicon.ico" || iconBase64 != "" {
		t.Fatalf("SiteIconByDomain() = (%q, %q, %v, %v)", iconBase64, iconURL, found, err)
	}
	if err := store.UpdateSiteIcon(ctx, "a.example.com", "", "aWNvbg=="); err != nil {
		t.Fatalf("UpdateSiteIcon(base64) error = %v", err)
	}
	if iconBase64, _, found, err = store.SiteIconByDomain(ctx, "a.example.com"); err != nil || !found || iconBase64 != "aWNvbg==" {
		t.Fatalf("base64 图标 = (%q, %v, %v)", iconBase64, found, err)
	}
	if _, _, found, err := store.SiteIconByDomain(ctx, "missing.example.com"); err != nil || found {
		t.Fatalf("未知域名图标 = (found=%v, err=%v)", found, err)
	}
	if err := store.UpdateSiteIcon(ctx, "missing.example.com", "https://x/y.ico", ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("未知域名 UpdateSiteIcon() error = %v, want pgx.ErrNoRows", err)
	}
	if err := store.UpdateSiteIcon(ctx, "", "https://x/y.ico", ""); err == nil {
		t.Fatal("空域名 UpdateSiteIcon() 应报错")
	}

	secondID, err := store.UpsertSite(ctx, Site{
		Name: "站点B", Domain: "b.example.com", URL: "https://b.example.com",
		Adapter: "mteam", Enabled: false, Config: map[string]any{"pri": 1},
	})
	if err != nil {
		t.Fatalf("UpsertSite(B) error = %v", err)
	}

	if err := store.UpdateSite(ctx, Site{
		ID: id, Name: "站点A2", Domain: "a.example.com", URL: "https://a.example.com",
		Adapter: "mteam", Enabled: false, Config: map[string]any{"cookie": "new-cookie", "pri": 5},
	}); err != nil {
		t.Fatalf("UpdateSite() error = %v", err)
	}
	updated, err := store.RuntimeSiteByID(ctx, id)
	if err != nil {
		t.Fatalf("更新后 RuntimeSiteByID() error = %v", err)
	}
	if updated.Name != "站点A2" || updated.Enabled || updated.Config["cookie"] != "new-cookie" {
		t.Fatalf("更新后站点 = %#v", updated)
	}
	if err := store.UpdateSite(ctx, Site{ID: id, Name: "站点B", Domain: "a.example.com", URL: "https://a.example.com", Adapter: "mteam"}); !errors.Is(err, ErrSiteConflict) {
		t.Fatalf("改名冲突 UpdateSite() error = %v, want ErrSiteConflict", err)
	}
	if err := store.UpdateSite(ctx, Site{ID: 0}); err == nil {
		t.Fatal("ID=0 UpdateSite() 应报错")
	}
	if err := store.UpdateSite(ctx, Site{ID: 999999, Name: "不存在", Domain: "x.example.com"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("不存在站点 UpdateSite() error = %v, want pgx.ErrNoRows", err)
	}

	// pri 小的排前面；同 pri 按 id。
	if err := store.UpdateSitePriorities(ctx, []SitePriority{{ID: id, Priority: 100}}); err != nil {
		t.Fatalf("UpdateSitePriorities() error = %v", err)
	}
	sites, err = store.ListSites(ctx)
	if err != nil {
		t.Fatalf("优先级更新后 ListSites() error = %v", err)
	}
	if len(sites) != 2 || sites[0].ID != secondID || sites[1].ID != id {
		t.Fatalf("优先级排序 = %#v", sites)
	}
	if err := store.UpdateSitePriorities(ctx, []SitePriority{{ID: 999999, Priority: 1}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("未知站点优先级 error = %v, want pgx.ErrNoRows", err)
	}
	if err := store.UpdateSitePriorities(ctx, nil); err != nil {
		t.Fatalf("空优先级列表 error = %v", err)
	}

	deleted, err := store.DeleteSite(ctx, secondID)
	if err != nil || !deleted {
		t.Fatalf("DeleteSite() = (%v, %v)", deleted, err)
	}
	if deleted, err := store.DeleteSite(ctx, secondID); err != nil || deleted {
		t.Fatalf("重复 DeleteSite() = (%v, %v)", deleted, err)
	}

	removed, err := store.DeleteAllSites(ctx)
	if err != nil || removed != 1 {
		t.Fatalf("DeleteAllSites() = (%d, %v), want 1", removed, err)
	}
	if sites, err := store.ListSites(ctx); err != nil || len(sites) != 0 {
		t.Fatalf("清空后 ListSites() = (%d, %v)", len(sites), err)
	}
	if _, err := store.SiteByID(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("删除后 SiteByID() error = %v, want pgx.ErrNoRows", err)
	}
}
