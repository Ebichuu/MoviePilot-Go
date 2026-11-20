package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/cookiecloud"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

// cookieCloudSyncer 把 V2 CookieCloud 同步接到 Go 站点存储。
type cookieCloudSyncer struct {
	helper       *cookiecloud.Helper
	store        *database.Store
	blacklist    []string
	search       *indexer.Coordinator
	siteHTTP     *indexer.HTTPClient
	buildAdapter func(database.Site) (indexer.Adapter, error)
	reloadSites  func(context.Context) error
}

func newCookieCloudSyncer(
	cfg config.Config,
	store *database.Store,
	cloudClient *http.Client,
	search *indexer.Coordinator,
	siteHTTP *indexer.HTTPClient,
	buildAdapter func(database.Site) (indexer.Adapter, error),
	reloadSites func(context.Context) error,
) *cookieCloudSyncer {
	if store == nil ||
		strings.TrimSpace(cfg.CookieCloudKey) == "" ||
		strings.TrimSpace(cfg.CookieCloudPassword) == "" {
		return nil
	}
	helper := cookiecloud.NewHelper(cookiecloud.Config{
		Server:      cfg.CookieCloudHost,
		Key:         cfg.CookieCloudKey,
		Password:    cfg.CookieCloudPassword,
		EnableLocal: cfg.CookieCloudEnableLocal,
		LocalDir:    cfg.CookieCloudCookiePath,
	}, cloudClient)
	return &cookieCloudSyncer{
		helper: helper, store: store, blacklist: cfg.CookieCloudBlacklist,
		search: search, siteHTTP: siteHTTP, buildAdapter: buildAdapter, reloadSites: reloadSites,
	}
}

// SyncCookieCloud 复刻 V2 `SiteChain.remote_sync`，返回 V2 结果消息。
func (s *cookieCloudSyncer) SyncCookieCloud(ctx context.Context) (string, error) {
	if s == nil || s.helper == nil || s.store == nil {
		return "", errors.New("CookieCloud 未配置")
	}
	siteStore := &cookieCloudSiteStore{store: s.store, siteHTTP: s.siteHTTP}
	directory := &cookieCloudSiteDirectory{store: s.store}
	service, err := cookiecloud.NewSyncService(
		s.helper,
		siteStore,
		&cookieCloudConnectivityTester{
			store: s.store, search: s.search, siteHTTP: s.siteHTTP, buildAdapter: s.buildAdapter,
		},
		directory,
		cookieCloudRSSResolver{siteHTTP: s.siteHTTP},
		nil, // 当前 Go 运行时没有 V2 SiteUpdated 的消费者总线。
	)
	if err != nil {
		return "", err
	}
	result, syncErr := service.Sync(ctx, cookiecloud.SyncOptions{Blacklist: s.blacklist})
	reloadErr := siteStore.reloadIfChanged(ctx, s.reloadSites)
	if syncErr != nil || reloadErr != nil {
		return "", errors.Join(syncErr, reloadErr)
	}
	return result.Message, nil
}

// cookieCloudSiteDirectory reads the currently maintained config metadata. It
// intentionally does not ship a guessed replacement for V2's external schema.
type cookieCloudSiteDirectory struct {
	store *database.Store
}

func (d *cookieCloudSiteDirectory) entries(ctx context.Context) ([]indexer.SiteDirectoryEntry, error) {
	if d == nil || d.store == nil || ctx == nil {
		return nil, errors.New("站点目录未初始化")
	}
	sites, err := d.store.ListSites(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]indexer.SiteDirectoryEntry, 0, len(sites))
	for _, site := range sites {
		entry := indexer.SiteDirectoryEntryFromConfig(site.Name, site.Domain, site.Adapter, site.Config)
		if strings.TrimSpace(site.Domain) != "" && !strings.EqualFold(strings.TrimSpace(site.Domain), strings.TrimSpace(entry.Domain)) {
			entry.ExtDomains = append(entry.ExtDomains, site.Domain)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (d *cookieCloudSiteDirectory) directory(ctx context.Context) (*indexer.SiteDirectory, error) {
	entries, err := d.entries(ctx)
	if err != nil {
		return nil, err
	}
	return indexer.NewSiteDirectory(entries), nil
}

func (d *cookieCloudSiteDirectory) SiteCategories(
	ctx context.Context,
	domain string,
) ([]map[string]any, bool, error) {
	directory, err := d.directory(ctx)
	if err != nil {
		return nil, false, err
	}
	return directory.SiteCategories(ctx, domain)
}

func (d *cookieCloudSiteDirectory) IndexerByDomain(
	ctx context.Context,
	domain string,
) (cookiecloud.Indexer, bool, error) {
	directory, err := d.directory(ctx)
	if err != nil {
		return cookiecloud.Indexer{}, false, err
	}
	entry, found := directory.Lookup(domain)
	if !found {
		return cookiecloud.Indexer{}, false, nil
	}
	return cookiecloud.Indexer{
		Name: entry.Name, Domain: entry.Domain, Adapter: entry.Adapter,
		ExtDomains: append([]string(nil), entry.ExtDomains...), Public: entry.Public,
		Config: entry.Config,
	}, true, nil
}

// cookieCloudConnectivityTester uses the live saved adapter for existing sites
// and the maintained adapter definition or homepage probe for candidate sites.
type cookieCloudConnectivityTester struct {
	store        *database.Store
	search       *indexer.Coordinator
	siteHTTP     *indexer.HTTPClient
	buildAdapter func(database.Site) (indexer.Adapter, error)
}

func (t *cookieCloudConnectivityTester) Test(
	ctx context.Context,
	site cookiecloud.Site,
) (bool, string, error) {
	if t == nil || ctx == nil || t.store == nil {
		return false, "站点连通性测试未初始化", errors.New("站点连通性测试未初始化")
	}
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	definition := database.Site{
		ID: site.ID, Name: site.Name, Domain: site.Domain, URL: site.URL,
		Adapter: site.Adapter, Enabled: true, Config: cloneCookieCloudConfig(site.Config),
	}
	if definition.Config == nil {
		definition.Config = make(map[string]any)
	}
	if site.Cookie != "" {
		definition.Config["cookie"] = site.Cookie
	}
	if t.buildAdapter != nil {
		adapter, err := t.buildAdapter(definition)
		if err == nil && adapter != nil {
			if _, supported := adapter.(indexer.SiteTester); supported && t.search != nil {
				result := t.search.TestSite(ctx, adapter)
				if result.Success {
					return true, result.Message, nil
				}
				return false, result.Message, errors.New(result.Message)
			}
		}
	}
	if t.siteHTTP == nil {
		return false, "站点 HTTP 客户端未初始化", errors.New("站点 HTTP 客户端未初始化")
	}
	var err error
	if site.Public {
		err = t.siteHTTP.TestSiteAvailability(ctx, site.URL, site.Cookie, site.UA)
	} else {
		err = t.siteHTTP.TestSiteLoginPage(ctx, site.URL, site.Cookie, site.UA)
	}
	if err != nil {
		return false, "站点连接或登录验证失败", err
	}
	return true, "连接成功", nil
}

type cookieCloudRSSResolver struct {
	siteHTTP *indexer.HTTPClient
}

func (r cookieCloudRSSResolver) RSSLink(ctx context.Context, site cookiecloud.Site, cookie string) (string, string) {
	if r.siteHTTP == nil {
		return "", "站点 HTTP 客户端未初始化"
	}
	feedURL, err := r.siteHTTP.DiscoverSiteRSS(ctx, site.URL, cookie, site.UA)
	if err != nil {
		return "", "读取站点 RSS 声明失败"
	}
	return feedURL, ""
}

type cookieCloudSiteStore struct {
	store    *database.Store
	siteHTTP *indexer.HTTPClient
	mu       sync.Mutex
	dirty    bool
}

func (a *cookieCloudSiteStore) SiteByDomain(
	ctx context.Context,
	domain string,
) (cookiecloud.Site, bool, error) {
	sites, err := a.store.ListSites(ctx)
	if err != nil {
		return cookiecloud.Site{}, false, err
	}
	for _, site := range sites {
		if !siteMatchesCookieDomain(site, domain) {
			continue
		}
		runtimeSite, err := a.store.RuntimeSiteByID(ctx, site.ID)
		if err != nil {
			return cookiecloud.Site{}, false, err
		}
		return cookieCloudSiteFromDatabase(runtimeSite), true, nil
	}
	return cookiecloud.Site{}, false, nil
}

func (a *cookieCloudSiteStore) UpdateCookie(ctx context.Context, domain, cookie string) error {
	return a.updateConfig(ctx, domain, "cookie", cookie, true)
}

func (a *cookieCloudSiteStore) UpdateRSS(ctx context.Context, domain, rss string) error {
	return a.updateConfig(ctx, domain, "rss", rss, false)
}

func (a *cookieCloudSiteStore) AddSite(ctx context.Context, site cookiecloud.Site) error {
	if a == nil || a.store == nil || strings.TrimSpace(site.Adapter) == "" {
		return errors.New("新增站点定义不完整")
	}
	config := cloneCookieCloudConfig(site.Config)
	if config == nil {
		config = make(map[string]any)
	}
	config["cookie"] = site.Cookie
	config["public"] = site.Public
	if site.RSS != "" {
		config["rss"] = site.RSS
	}
	if a.siteHTTP != nil {
		iconURL, iconBase64, iconErr := a.siteHTTP.DiscoverSiteFavicon(ctx, site.URL, site.Cookie, site.UA)
		if iconErr == nil && (iconURL != "" || iconBase64 != "") {
			config["_site_icon"] = map[string]any{"url": iconURL, "base64": iconBase64}
		}
	}
	_, err := a.store.UpsertSite(ctx, database.Site{
		Name: site.Name, Domain: site.Domain, URL: site.URL, Adapter: site.Adapter,
		Enabled: true, Config: config,
	})
	if err != nil {
		return err
	}
	a.markDirty()
	return nil
}

func (a *cookieCloudSiteStore) updateConfig(
	ctx context.Context,
	domain string,
	key string,
	value string,
	refreshIcon bool,
) error {
	if a == nil || a.store == nil {
		return errors.New("站点存储未初始化")
	}
	sites, err := a.store.ListSites(ctx)
	if err != nil {
		return err
	}
	for _, site := range sites {
		if !siteMatchesCookieDomain(site, domain) {
			continue
		}
		runtimeSite, err := a.store.RuntimeSiteByID(ctx, site.ID)
		if err != nil {
			return err
		}
		if runtimeSite.Config == nil {
			runtimeSite.Config = make(map[string]any)
		}
		deleteCookieCloudConfigKey(runtimeSite.Config, key)
		runtimeSite.Config[key] = value
		if err := a.store.UpdateSite(ctx, runtimeSite); err != nil {
			return err
		}
		a.markDirty()
		if refreshIcon {
			a.cacheSiteIcon(ctx, runtimeSite)
		}
		return nil
	}
	return nil
}

func (a *cookieCloudSiteStore) cacheSiteIcon(ctx context.Context, site database.Site) {
	if a == nil || a.siteHTTP == nil {
		return
	}
	iconURL, iconBase64, err := a.siteHTTP.DiscoverSiteFavicon(
		ctx, site.URL, stringConfigValue(site.Config, "cookie"), stringConfigValue(site.Config, "ua"),
	)
	if err != nil || iconURL == "" && iconBase64 == "" {
		return
	}
	_ = a.store.UpdateSiteIcon(ctx, site.Domain, iconURL, iconBase64)
}

func (a *cookieCloudSiteStore) markDirty() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.dirty = true
	a.mu.Unlock()
}

func (a *cookieCloudSiteStore) reloadIfChanged(ctx context.Context, reload func(context.Context) error) error {
	if a == nil || reload == nil {
		return nil
	}
	a.mu.Lock()
	dirty := a.dirty
	a.mu.Unlock()
	if !dirty {
		return nil
	}
	if ctx == nil {
		return errors.New("站点重载上下文无效")
	}
	if err := reload(ctx); err != nil {
		a.mu.Lock()
		a.dirty = true
		a.mu.Unlock()
		return err
	}
	a.mu.Lock()
	a.dirty = false
	a.mu.Unlock()
	return nil
}

func cookieCloudSiteFromDatabase(site database.Site) cookiecloud.Site {
	return cookiecloud.Site{
		ID: site.ID, Name: site.Name, URL: site.URL, Domain: site.Domain, Adapter: site.Adapter,
		Config: cloneCookieCloudConfig(site.Config), Cookie: stringConfigValue(site.Config, "cookie"),
		RSS: stringConfigValue(site.Config, "rss"), Public: configBoolValue(site.Config, "public"),
		Active: site.Enabled, UA: stringConfigValue(site.Config, "ua"),
		Proxy: configBoolValue(site.Config, "proxy"), Timeout: configIntValue(site.Config, "timeout", 15),
	}
}

func siteMatchesCookieDomain(site database.Site, domain string) bool {
	if cookiecloud.URLDomain(site.Domain) == domain || cookiecloud.URLDomain(site.URL) == domain {
		return true
	}
	entry := indexer.SiteDirectoryEntryFromConfig(site.Name, site.Domain, site.Adapter, site.Config)
	for _, candidate := range append([]string{entry.Domain}, entry.ExtDomains...) {
		if cookiecloud.URLDomain(candidate) == domain {
			return true
		}
	}
	return false
}

func cloneCookieCloudConfig(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func deleteCookieCloudConfigKey(config map[string]any, key string) {
	for existing := range config {
		if strings.EqualFold(strings.TrimSpace(existing), key) {
			delete(config, existing)
		}
	}
}

func stringConfigValue(config map[string]any, key string) string {
	for existing, value := range config {
		if !strings.EqualFold(strings.TrimSpace(existing), key) {
			continue
		}
		if text, ok := value.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func configBoolValue(config map[string]any, key string) bool {
	for existing, value := range config {
		if !strings.EqualFold(strings.TrimSpace(existing), key) {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed
		case int:
			return typed != 0
		case int64:
			return typed != 0
		case float64:
			return typed != 0
		case string:
			return typed == "1" || strings.EqualFold(strings.TrimSpace(typed), "true")
		}
	}
	return false
}

func configIntValue(config map[string]any, key string, fallback int) int {
	for existing, value := range config {
		if !strings.EqualFold(strings.TrimSpace(existing), key) {
			continue
		}
		switch typed := value.(type) {
		case int:
			return typed
		case int64:
			return int(typed)
		case float64:
			return int(typed)
		case string:
			var parsed int
			if _, err := fmt.Sscan(strings.TrimSpace(typed), &parsed); err == nil {
				return parsed
			}
		}
	}
	return fallback
}

var _ httpapi.SiteCategoryCatalog = (*cookieCloudSiteDirectory)(nil)
var _ cookiecloud.SiteStore = (*cookieCloudSiteStore)(nil)
var _ cookiecloud.ConnectivityTester = (*cookieCloudConnectivityTester)(nil)
var _ cookiecloud.IndexerResolver = (*cookieCloudSiteDirectory)(nil)
var _ cookiecloud.RSSResolver = cookieCloudRSSResolver{}
