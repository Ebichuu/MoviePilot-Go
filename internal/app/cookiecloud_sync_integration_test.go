//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/cookiecloud"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// writeCookieCloudLocalPayload 在本地目录写出 `<key>.json` 加密载荷，
// 让 CookieCloud 走 EnableLocal 分支，避免任何外部 HTTP 依赖。
func writeCookieCloudLocalPayload(t *testing.T, dir, key string, cookieData map[string]any) {
	t.Helper()
	plaintext, err := json.Marshal(map[string]any{"cookie_data": cookieData})
	if err != nil {
		t.Fatalf("编码 CookieCloud 载荷: %v", err)
	}
	encrypted, err := cookiecloud.Encrypt(plaintext, cookiecloud.CryptKey(key, "it-pass"))
	if err != nil {
		t.Fatalf("加密 CookieCloud 载荷: %v", err)
	}
	encoded, err := json.Marshal(map[string]any{"encrypted": encrypted})
	if err != nil {
		t.Fatalf("编码密文外壳: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".json"), encoded, 0o600); err != nil {
		t.Fatalf("写出本地 CookieCloud 文件: %v", err)
	}
}

func cookieCloudTestConfig(dir string) config.Config {
	return config.Config{
		CookieCloudKey:         "it-key",
		CookieCloudPassword:    "it-pass",
		CookieCloudEnableLocal: true,
		CookieCloudCookiePath:  dir,
	}
}

// TestSyncCookieCloudIntegration 覆盖 CookieCloud 远程同步的三条主路径：
// 已存在但不连通的站点更新 Cookie、域名无匹配时不触发重载、未知域名的索引器
// 因连通性失败计入失败；另覆盖重载失败与未配置守卫。
func TestSyncCookieCloudIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	if _, err := (&cookieCloudSyncer{}).SyncCookieCloud(ctx); err == nil {
		t.Fatal("未配置时 SyncCookieCloud 应报错")
	}

	// 已存在且启用的站点：连通性测试因缺少 HTTP 客户端而失败 → 更新 Cookie。
	mustUpsertAppSite(t, store, database.Site{
		Name: "A站", Domain: "a.example", URL: "https://a.example", Adapter: "rss",
		Enabled: true, Config: map[string]any{"rss": "/feed"},
	})
	dir := t.TempDir()
	writeCookieCloudLocalPayload(t, dir, "it-key", map[string]any{
		".a.example": []map[string]any{
			{"domain": ".a.example", "name": "token", "value": "abc"},
		},
	})
	reloads := 0
	syncer := newCookieCloudSyncer(cookieCloudTestConfig(dir), store, nil, nil, nil, nil,
		func(context.Context) error {
			reloads++
			return nil
		})
	if syncer == nil {
		t.Fatal("配置齐备时 newCookieCloudSyncer() = nil")
	}
	message, err := syncer.SyncCookieCloud(ctx)
	if err != nil {
		t.Fatalf("SyncCookieCloud() error = %v", err)
	}
	if message != "更新了1个站点，新增了0个站点" {
		t.Fatalf("同步消息 = %q", message)
	}
	if reloads != 1 {
		t.Fatalf("站点重载次数 = %d, want 1", reloads)
	}
	assertCookieCloudSiteCookieIntegration(t, store, "a.example", "token=abc")

	// 载荷里的域名在本地站点里找不到 → 不更新、不重载。
	writeCookieCloudLocalPayload(t, dir, "it-key", map[string]any{
		".zz.example": []map[string]any{
			{"domain": ".zz.example", "name": "token", "value": "zzz"},
		},
	})
	message, err = syncer.SyncCookieCloud(ctx)
	if err != nil {
		t.Fatalf("未知域名 SyncCookieCloud() error = %v", err)
	}
	if message != "更新了0个站点，新增了0个站点" || reloads != 1 {
		t.Fatalf("未知域名同步 = (%q, reloads=%d)", message, reloads)
	}

	// 停用站点通过扩展域名命中已知索引器，但连通性测试失败 → 计为新增失败。
	mustUpsertAppSite(t, store, database.Site{
		Name: "B站", Domain: "b.example", URL: "https://b.example", Adapter: "rss",
		Enabled: false, Config: map[string]any{"rss": "/feed", "ext_domains": "c.example"},
	})
	writeCookieCloudLocalPayload(t, dir, "it-key", map[string]any{
		".c.example": []map[string]any{
			{"domain": ".c.example", "name": "token", "value": "ccc"},
		},
	})
	message, err = syncer.SyncCookieCloud(ctx)
	if err != nil {
		t.Fatalf("新增失败场景 SyncCookieCloud() error = %v", err)
	}
	if !strings.Contains(message, "1个站点添加失败") {
		t.Fatalf("新增失败消息 = %q", message)
	}
	if reloads != 1 {
		t.Fatalf("新增失败不应触发重载, reloads = %d", reloads)
	}

	// 重载失败：同步本身成功也要把重载错误返回，并保留 dirty 状态。
	writeCookieCloudLocalPayload(t, dir, "it-key", map[string]any{
		".a.example": []map[string]any{
			{"domain": ".a.example", "name": "token", "value": "abc2"},
		},
	})
	failingReload := newCookieCloudSyncer(cookieCloudTestConfig(dir), store, nil, nil, nil, nil,
		func(context.Context) error {
			return errors.New("重载失败")
		})
	if _, err := failingReload.SyncCookieCloud(ctx); err == nil {
		t.Fatal("重载失败时 SyncCookieCloud 应报错")
	}
	assertCookieCloudSiteCookieIntegration(t, store, "a.example", "token=abc2")

	// 本地文件缺失时返回 CookieCloud 请求错误。
	missing := newCookieCloudSyncer(cookieCloudTestConfig(t.TempDir()), store, nil, nil, nil, nil, nil)
	if _, err := missing.SyncCookieCloud(ctx); err == nil {
		t.Fatal("缺少本地载荷时 SyncCookieCloud 应报错")
	}
}

func assertCookieCloudSiteCookieIntegration(t *testing.T, store *database.Store, domain, want string) {
	t.Helper()
	sites, err := store.ListSites(t.Context())
	if err != nil {
		t.Fatalf("ListSites() error = %v", err)
	}
	for _, site := range sites {
		if !strings.EqualFold(site.Domain, domain) {
			continue
		}
		runtimeSite, err := store.RuntimeSiteByID(t.Context(), site.ID)
		if err != nil {
			t.Fatalf("RuntimeSiteByID(%s) error = %v", domain, err)
		}
		if got := stringConfigValue(runtimeSite.Config, "cookie"); got != want {
			t.Fatalf("%s 的 cookie = %q, want %q", domain, got, want)
		}
		return
	}
	t.Fatalf("未找到站点 %s", domain)
}
