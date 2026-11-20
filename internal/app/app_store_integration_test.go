//go:build integration

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/cookiecloud"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/siteflow"
)

const appStoreIntegrationSchemaPrefix = "moviepilot_app_store_it_"

// newAppIntegrationStore 在独立 schema 里跑完整迁移，返回真实 *database.Store。
// 复用 migrate_integration_test.go 的 databaseURLWithSearchPath，保证两处隔离方式一致。
func newAppIntegrationStore(t *testing.T) *database.Store {
	t.Helper()

	databaseURL := strings.TrimSpace(os.Getenv(migrateTestDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", migrateTestDatabaseEnv)
	}

	setupCtx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析 %s: %v", migrateTestDatabaseEnv, err)
	}
	adminConfig.MaxConns = 2
	adminConfig.MinConns = 0
	adminPool, err := pgxpool.NewWithConfig(setupCtx, adminConfig)
	if err != nil {
		t.Fatalf("创建 PostgreSQL 管理连接池: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if err := adminPool.Ping(setupCtx); err != nil {
		t.Fatalf("连接 PostgreSQL 集成测试数据库: %v", err)
	}

	schema := appStoreIntegrationSchemaPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(setupCtx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("创建隔离 schema %q: %v", schema, err)
	}

	var storePool *pgxpool.Pool
	t.Cleanup(func() {
		if storePool != nil {
			storePool.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("清理隔离 schema %q: %v", schema, err)
		}
	})

	if err := Migrate(setupCtx, config.Config{DatabaseURL: databaseURLWithSearchPath(t, databaseURL, schema)}); err != nil {
		t.Fatalf("在隔离 schema %q 执行迁移: %v", schema, err)
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析业务连接池配置: %v", err)
	}
	poolConfig.MaxConns = 8
	poolConfig.MinConns = 0
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-app-store-integration"
	storePool, err = pgxpool.NewWithConfig(setupCtx, poolConfig)
	if err != nil {
		t.Fatalf("创建隔离业务连接池: %v", err)
	}
	if err := storePool.Ping(setupCtx); err != nil {
		t.Fatalf("连接隔离 schema %q: %v", schema, err)
	}
	var currentSchema string
	if err := storePool.QueryRow(setupCtx, "SELECT current_schema()").Scan(&currentSchema); err != nil {
		t.Fatalf("校验当前 schema: %v", err)
	}
	if currentSchema != schema {
		t.Fatalf("业务连接当前 schema = %q, want %q", currentSchema, schema)
	}

	box, err := auth.NewSecretBox("moviepilot-app-store-integration-key")
	if err != nil {
		t.Fatalf("创建敏感配置加密器: %v", err)
	}
	return database.NewStore(&database.DB{Pool: storePool}, box)
}

func appIntegrationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustUpsertAppSite(t *testing.T, store *database.Store, site database.Site) int {
	t.Helper()
	id, err := store.UpsertSite(t.Context(), site)
	if err != nil {
		t.Fatalf("新增站点 %q: %v", site.Name, err)
	}
	return id
}

func TestCookieCloudSiteStoreIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	id := mustUpsertAppSite(t, store, database.Site{
		Name: "集成站", Domain: "a.example", URL: "https://a.example", Adapter: "rss", Enabled: true,
		Config: map[string]any{"cookie": "old-cookie", "rss": "/feed", "public": true},
	})
	siteStore := &cookieCloudSiteStore{store: store}

	// 读：Cookie 从加密列解密后合并进运行时配置。
	site, found, err := siteStore.SiteByDomain(ctx, cookiecloud.URLDomain("a.example"))
	if err != nil || !found {
		t.Fatalf("SiteByDomain() = (%+v, %t, %v)", site, found, err)
	}
	if site.Cookie != "old-cookie" || site.RSS != "/feed" || !site.Public || !site.Active {
		t.Fatalf("站点映射 = %+v", site)
	}
	if _, found, err := siteStore.SiteByDomain(ctx, cookiecloud.URLDomain("missing.example")); err != nil || found {
		t.Fatalf("未知域名 = (found=%t, err=%v)", found, err)
	}
	// 公开视图里不允许出现明文 Cookie。
	publicSite, err := store.SiteByID(ctx, id)
	if err != nil {
		t.Fatalf("SiteByID() error = %v", err)
	}
	if _, exists := publicSite.Config["cookie"]; exists {
		t.Fatalf("公开配置泄漏 Cookie = %#v", publicSite.Config)
	}

	// 写：Cookie 覆盖旧值并落库（加密列）。
	if err := siteStore.UpdateCookie(ctx, "a.example", "uid=1&pass=2"); err != nil {
		t.Fatalf("UpdateCookie() error = %v", err)
	}
	runtimeSite, err := store.RuntimeSiteByID(ctx, id)
	if err != nil {
		t.Fatalf("RuntimeSiteByID() error = %v", err)
	}
	if runtimeSite.Config["cookie"] != "uid=1&pass=2" {
		t.Fatalf("Cookie 未落库 = %#v", runtimeSite.Config["cookie"])
	}
	// 未知域名是 no-op，不报错也不改任何站点。
	if err := siteStore.UpdateCookie(ctx, "missing.example", "x"); err != nil {
		t.Fatalf("未知域名 UpdateCookie() error = %v", err)
	}

	// RSS 用同一套 updateConfig：覆盖旧值而不是叠加。
	if err := siteStore.UpdateRSS(ctx, "a.example", "https://a.example/new-feed"); err != nil {
		t.Fatalf("UpdateRSS() error = %v", err)
	}
	runtimeSite, err = store.RuntimeSiteByID(ctx, id)
	if err != nil {
		t.Fatalf("RuntimeSiteByID() error = %v", err)
	}
	if runtimeSite.Config["rss"] != "https://a.example/new-feed" {
		t.Fatalf("RSS 未覆盖 = %#v", runtimeSite.Config["rss"])
	}

	// 写操作后允许重载一次，成功后脏标记清除。
	calls := 0
	reload := func(context.Context) error { calls++; return nil }
	if err := siteStore.reloadIfChanged(ctx, reload); err != nil || calls != 1 {
		t.Fatalf("reloadIfChanged() = (%v, calls=%d)", err, calls)
	}
	if err := siteStore.reloadIfChanged(ctx, reload); err != nil || calls != 1 {
		t.Fatalf("成功后重复重载 = (%v, calls=%d)", err, calls)
	}

	// AddSite 走 UpsertSite + markDirty；没有 HTTP 客户端时跳过图标抓取。
	if err := siteStore.AddSite(ctx, cookiecloud.Site{
		Name: "新站", Domain: "b.example", URL: "https://b.example", Adapter: "rss",
		Cookie: "cookie-b", Public: true, RSS: "/feed-b",
	}); err != nil {
		t.Fatalf("AddSite() error = %v", err)
	}
	if err := siteStore.reloadIfChanged(ctx, reload); err != nil || calls != 2 {
		t.Fatalf("AddSite 后重载 = (%v, calls=%d)", err, calls)
	}
	sites, err := store.ListSites(ctx)
	if err != nil {
		t.Fatalf("ListSites() error = %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("站点数 = %d", len(sites))
	}
	var added *database.Site
	for index := range sites {
		if sites[index].Domain == "b.example" {
			added = &sites[index]
		}
	}
	if added == nil || !added.Enabled {
		t.Fatalf("新增站点 = %#v", added)
	}
}

func TestWebPushSubscriptionStoreIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()
	webpush := &webPushSubscriptionStore{store: store}

	for _, subscription := range []database.WebPushSubscription{
		{Endpoint: "https://push.example/b", Username: "bob", P256dh: "p2", Auth: "a2", UserAgent: "UA2"},
		{Endpoint: "https://push.example/a", Username: "alice", P256dh: "p1", Auth: "a1", UserAgent: "UA1"},
	} {
		if err := store.UpsertWebPushSubscription(ctx, subscription); err != nil {
			t.Fatalf("保存订阅 %q: %v", subscription.Endpoint, err)
		}
	}

	subscriptions, err := webpush.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	// 按 endpoint 稳定排序，字段一一映射（UserAgent 不进入推送端口）。
	if len(subscriptions) != 2 || subscriptions[0].Endpoint != "https://push.example/a" ||
		subscriptions[0].Username != "alice" || subscriptions[0].P256dh != "p1" || subscriptions[0].Auth != "a1" {
		t.Fatalf("订阅列表 = %#v", subscriptions)
	}

	if err := webpush.Remove(ctx, "https://push.example/a"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	subscriptions, err = webpush.List(ctx)
	if err != nil {
		t.Fatalf("删除后 List() error = %v", err)
	}
	if len(subscriptions) != 1 || subscriptions[0].Endpoint != "https://push.example/b" {
		t.Fatalf("删除后订阅 = %#v", subscriptions)
	}
	// 删除不存在的 endpoint 不算错误（V2 同样幂等）。
	if err := webpush.Remove(ctx, "https://push.example/missing"); err != nil {
		t.Fatalf("删除不存在订阅 error = %v", err)
	}
}

func TestTelegramServicesReadStoreIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	enabledID := mustUpsertAppSite(t, store, database.Site{
		Name: "A站", Domain: "a.example", URL: "https://a.example", Adapter: "rss", Enabled: true,
		Config: map[string]any{"rss": "/feed"},
	})
	_ = enabledID
	mustUpsertAppSite(t, store, database.Site{
		Name: "", Domain: "b.example", URL: "https://b.example", Adapter: "rss", Enabled: false,
		Config: map[string]any{"rss": "/feed"},
	})

	services := newTelegramServices(nil, nil, store, "alice")
	lines, err := services.Sites(ctx)
	if err != nil {
		t.Fatalf("Sites() error = %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("站点行数 = %d", len(lines))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "A站") || !strings.Contains(joined, "已启用") ||
		!strings.Contains(joined, "已禁用") {
		t.Fatalf("站点行 = %q", joined)
	}
	// 没名字的站点用 ID 兜底，不能给用户一条只有一个点的空行。
	if !strings.Contains(joined, "站点 #") {
		t.Fatalf("缺少 ID 兜底 = %q", joined)
	}

	// 没有订阅时返回空列表而不是错误。
	subscriptions, err := services.Subscriptions(ctx)
	if err != nil {
		t.Fatalf("Subscriptions() error = %v", err)
	}
	if len(subscriptions) != 0 {
		t.Fatalf("订阅 = %#v", subscriptions)
	}
}

func TestTelegramOwnerUsernameIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	if got := telegramOwnerUsername(ctx, nil); got != "" {
		t.Fatalf("nil store = %q", got)
	}
	if got := telegramOwnerUsername(ctx, store); got != "" {
		t.Fatalf("空用户表 = %q", got)
	}

	if _, err := store.CreateUser(ctx, "normal", "hash", false, map[string]any{}); err != nil {
		t.Fatalf("创建普通用户: %v", err)
	}
	if _, err := store.CreateUser(ctx, "root", "hash", true, map[string]any{}); err != nil {
		t.Fatalf("创建超级管理员: %v", err)
	}
	// 优先代表超级管理员操作，避免机器人以普通用户权限发命令。
	if got := telegramOwnerUsername(ctx, store); got != "root" {
		t.Fatalf("操作账号 = %q", got)
	}
}

type siteUserDataAdapterFake struct {
	data indexer.SiteUserData
	err  error
}

func (a *siteUserDataAdapterFake) Name() string { return "fake-userdata" }

func (a *siteUserDataAdapterFake) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func (a *siteUserDataAdapterFake) FetchSiteUserData(context.Context) (indexer.SiteUserData, error) {
	return a.data, a.err
}

type siteUserDataSearchFake struct {
	data   indexer.SiteUserData
	err    error
	calls  int
	last   indexer.Adapter
	fetchM bool
}

func (s *siteUserDataSearchFake) FetchSiteUserData(
	_ context.Context,
	adapter indexer.Adapter,
	options ...indexer.SiteUserDataOptions,
) (indexer.SiteUserData, error) {
	s.calls++
	s.last = adapter
	if len(options) > 0 {
		s.fetchM = options[0].FetchMessages
	}
	return s.data, s.err
}

func TestSiteUserDataRefresherIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	siteID := mustUpsertAppSite(t, store, database.Site{
		Name: "A站", Domain: "a.example", URL: "https://a.example", Adapter: "rss", Enabled: true,
		Config: map[string]any{"rss": "/feed"},
	})

	refresher := &siteUserDataRefresher{store: store, logger: appIntegrationLogger()}
	sites, err := refresher.ListSitesForRefresh(ctx)
	if err != nil {
		t.Fatalf("ListSitesForRefresh() error = %v", err)
	}
	if len(sites) != 1 || sites[0].ID != siteID || sites[0].Name != "A站" || !sites[0].Active {
		t.Fatalf("批量站点 = %#v", sites)
	}
	batchSite := siteflow.BatchSite{ID: siteID, Name: "A站", Active: true}

	// 未配置适配器构建器：跳过（false, nil），不算失败。
	if _, ok, err := refresher.RefreshSiteUserData(ctx, batchSite); err != nil || ok {
		t.Fatalf("缺构建器 = (ok=%t, err=%v)", ok, err)
	}
	// 适配器不支持用户数据抓取：同样跳过。
	refresher.buildAdapter = func(database.Site) (indexer.Adapter, error) {
		return &telegramSearchAdapterFake{name: "no-userdata"}, nil
	}
	refresher.search = &siteUserDataSearchFake{}
	if _, ok, err := refresher.RefreshSiteUserData(ctx, batchSite); err != nil || ok {
		t.Fatalf("不支持抓取 = (ok=%t, err=%v)", ok, err)
	}

	// 正常路径：抓取 → 落库日快照 → 返回 true。
	profileUsername := "alice"
	search := &siteUserDataSearchFake{data: indexer.SiteUserData{
		Domain: "a.example", Username: &profileUsername, UserID: 42, Upload: 1024, Download: 512, Ratio: 2,
		Seeding: 3, Leeching: 1, Bonus: 1.5,
	}}
	refresher.buildAdapter = func(database.Site) (indexer.Adapter, error) {
		return &siteUserDataAdapterFake{data: search.data}, nil
	}
	refresher.search = search
	data, ok, err := refresher.RefreshSiteUserData(ctx, batchSite)
	if err != nil || !ok {
		t.Fatalf("RefreshSiteUserData() = (ok=%t, err=%v)", ok, err)
	}
	if data.Upload != 1024 || search.calls != 1 {
		t.Fatalf("抓取结果 = (%+v, calls=%d)", data, search.calls)
	}
	rows, err := store.SiteUserDataByDomain(ctx, "a.example", "")
	if err != nil {
		t.Fatalf("SiteUserDataByDomain() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Upload == nil || *rows[0].Upload != 1024 ||
		rows[0].Username == nil || *rows[0].Username != "alice" {
		t.Fatalf("落库快照 = %#v", rows)
	}

	// 抓取失败如实上抛，不写半条快照。
	search.err = errors.New("站点抓取失败")
	if _, ok, err := refresher.RefreshSiteUserData(ctx, batchSite); err == nil || ok {
		t.Fatalf("抓取失败 = (ok=%t, err=%v)", ok, err)
	}
	// 站点被删后报错（RuntimeSiteByID 找不到）。
	if _, ok, err := refresher.RefreshSiteUserData(ctx, siteflow.BatchSite{ID: siteID + 999}); err == nil || ok {
		t.Fatalf("未知站点 = (ok=%t, err=%v)", ok, err)
	}

	// 批量入口：站点列表 + 逐站刷新全流程。
	search.err = nil
	if err := refresher.HandleSiteUserDataRefresh(ctx); err != nil {
		t.Fatalf("HandleSiteUserDataRefresh() error = %v", err)
	}
	if search.calls < 2 {
		t.Fatalf("批量刷新未调用抓取: %d", search.calls)
	}
}

func TestLoadSiteAdaptersIntegration(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	mustUpsertAppSite(t, store, database.Site{
		Name: "RSS站", Domain: "a.example", URL: "https://a.example", Adapter: "rss", Enabled: true,
		Config: map[string]any{"rss": "/feed"},
	})
	// 不支持的适配器与未启用站点都不应出现在运行时适配器列表里。
	mustUpsertAppSite(t, store, database.Site{
		Name: "旧站", Domain: "b.example", URL: "https://b.example", Adapter: "python-legacy", Enabled: true,
		Config: map[string]any{},
	})
	mustUpsertAppSite(t, store, database.Site{
		Name: "停用站", Domain: "c.example", URL: "https://c.example", Adapter: "rss", Enabled: false,
		Config: map[string]any{"rss": "/feed"},
	})

	client, err := indexer.NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	adapters, err := loadSiteAdapters(ctx, store, client, appIntegrationLogger())
	if err != nil {
		t.Fatalf("loadSiteAdapters() error = %v", err)
	}
	if len(adapters) != 1 || adapters[0].Name() != "RSS站" {
		t.Fatalf("运行时适配器 = %#v", adapters)
	}
}
