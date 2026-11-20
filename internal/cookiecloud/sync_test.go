package cookiecloud

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type syncStoreFake struct {
	sites      map[string]Site
	updated    map[string]string
	rssUpdates map[string]string
	added      []Site
	siteErr    error
	cookieErr  error
	rssErr     error
	addErr     error
	siteCalls  int
}

func newSyncStoreFake() *syncStoreFake {
	return &syncStoreFake{
		sites:      map[string]Site{},
		updated:    map[string]string{},
		rssUpdates: map[string]string{},
	}
}

func (f *syncStoreFake) SiteByDomain(_ context.Context, domain string) (Site, bool, error) {
	f.siteCalls++
	if f.siteErr != nil {
		return Site{}, false, f.siteErr
	}
	site, ok := f.sites[domain]
	return site, ok, nil
}

func (f *syncStoreFake) UpdateCookie(_ context.Context, domain, cookie string) error {
	if f.cookieErr != nil {
		return f.cookieErr
	}
	f.updated[domain] = cookie
	return nil
}

func (f *syncStoreFake) UpdateRSS(_ context.Context, domain, rss string) error {
	if f.rssErr != nil {
		return f.rssErr
	}
	f.rssUpdates[domain] = rss
	return nil
}

func (f *syncStoreFake) AddSite(_ context.Context, site Site) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, site)
	return nil
}

type syncTesterFake struct {
	reachable map[string]bool
	failFor   map[string]bool
	err       error
	calls     []string
}

func (f *syncTesterFake) Test(_ context.Context, site Site) (bool, string, error) {
	f.calls = append(f.calls, site.Domain)
	if f.err != nil {
		return false, "", f.err
	}
	if f.failFor[site.Domain] {
		return false, "", errors.New("连接超时")
	}
	return f.reachable[site.Domain], "", nil
}

type syncIndexerFake struct {
	indexers map[string]Indexer
	err      error
}

func (f *syncIndexerFake) IndexerByDomain(_ context.Context, domain string) (Indexer, bool, error) {
	if f.err != nil {
		return Indexer{}, false, f.err
	}
	indexer, ok := f.indexers[domain]
	return indexer, ok, nil
}

type syncRSSFake struct {
	links map[string]string
	err   error
}

func (f *syncRSSFake) RSSLink(_ context.Context, site Site, _ string) (string, string) {
	if f.err != nil {
		return "", "生成失败"
	}
	return f.links[site.Domain], ""
}

type syncNotifierFake struct {
	domains []string
	err     error
}

func (f *syncNotifierFake) SiteUpdated(_ context.Context, domain string) error {
	f.domains = append(f.domains, domain)
	return f.err
}

// newSyncHelper 构造一个本地模式的 Helper，其载荷包含给定的「域名 → cookie 值」。
func newSyncHelper(t *testing.T, cookies map[string]string) *Helper {
	t.Helper()
	cookieData := map[string]any{}
	for domain, value := range cookies {
		cookieData[domain] = []any{
			map[string]any{"domain": domain, "name": "sid", "value": value},
		}
	}
	encoded, err := json.Marshal(map[string]any{"cookie_data": cookieData})
	if err != nil {
		t.Fatalf("编码载荷失败: %v", err)
	}
	dir := t.TempDir()
	writeLocalPayload(t, dir, encryptedPayload(t, string(encoded)))
	return NewHelper(Config{EnableLocal: true, LocalDir: dir, Key: "key", Password: "pass"}, nil)
}

func newSyncService(t *testing.T, cookies map[string]string, store SiteStore, tester ConnectivityTester,
	indexers IndexerResolver, rss RSSResolver, events SiteUpdatedNotifier) *SyncService {
	t.Helper()
	service, err := NewSyncService(newSyncHelper(t, cookies), store, tester, indexers, rss, events)
	if err != nil {
		t.Fatalf("创建同步服务失败: %v", err)
	}
	return service
}

func TestNewSyncServiceRequiresHelperAndStore(t *testing.T) {
	store := newSyncStoreFake()
	if _, err := NewSyncService(nil, store, nil, nil, nil, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("缺少 helper = %v", err)
	}
	if _, err := NewSyncService(&Helper{}, nil, nil, nil, nil, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("缺少 store = %v", err)
	}
	service := newSyncService(t, map[string]string{"a.example.org": "1"}, store, nil, nil, nil, nil)
	if service == nil {
		t.Fatal("服务不应为空")
	}
}

func TestSyncPropagatesDownloadFailure(t *testing.T) {
	missing := NewHelper(Config{EnableLocal: true, LocalDir: t.TempDir(), Key: "key", Password: "pass"}, nil)
	service, err := NewSyncService(missing, newSyncStoreFake(), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := service.Sync(context.Background(), SyncOptions{}); !errors.Is(err, ErrRequest) {
		t.Fatalf("载荷缺失 = %v", err)
	}
}

func TestSyncKeepsReachableSiteCookieUntouched(t *testing.T) {
	domain := URLDomain("a.example.org")
	store := newSyncStoreFake()
	store.sites[domain] = Site{ID: 1, Domain: domain, Active: true, Public: true, Cookie: "sid=old"}
	tester := &syncTesterFake{reachable: map[string]bool{domain: true}}
	notifier := &syncNotifierFake{}
	service := newSyncService(t, map[string]string{"a.example.org": "new"}, store, tester, nil, nil, notifier)

	result, err := service.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// 站点连通时不能覆盖已有 Cookie（否则会把用户刚登录的会话冲掉）。
	if len(store.updated) != 0 {
		t.Fatalf("连通站点被覆盖: %+v", store.updated)
	}
	if len(store.added) != 0 {
		t.Fatalf("不应新增站点: %+v", store.added)
	}
	if len(notifier.domains) != 1 || notifier.domains[0] != domain {
		t.Fatalf("事件 = %v", notifier.domains)
	}
	if result.Updated != 0 || result.Added != 0 || result.Failed != 0 {
		t.Fatalf("计数 = %+v", result)
	}
	if result.Message != "更新了0个站点，新增了0个站点" {
		t.Fatalf("消息 = %q", result.Message)
	}
}

func TestSyncUpdatesCookieForUnreachableActiveSite(t *testing.T) {
	domain := URLDomain("b.example.net")
	store := newSyncStoreFake()
	store.sites[domain] = Site{ID: 2, Domain: domain, Active: true, Public: true, Cookie: "sid=old"}
	tester := &syncTesterFake{reachable: map[string]bool{domain: false}}
	notifier := &syncNotifierFake{}
	service := newSyncService(t, map[string]string{"b.example.net": "new"}, store, tester, nil, nil, notifier)

	result, err := service.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if store.updated[domain] != "sid=new" {
		t.Fatalf("Cookie 未更新: %+v", store.updated)
	}
	if result.Updated != 1 || result.Added != 0 {
		t.Fatalf("计数 = %+v", result)
	}
	if len(notifier.domains) != 1 {
		t.Fatalf("事件 = %v", notifier.domains)
	}
	// 探测出错但上下文未取消：仍按「不通」处理并更新 Cookie，不能整体失败。
	if tester.err != nil {
		t.Fatal("不应记录测试错误")
	}
}

func TestSyncGeneratesRSSForReachablePrivateSite(t *testing.T) {
	domain := URLDomain("c.example.net")
	store := newSyncStoreFake()
	store.sites[domain] = Site{ID: 3, Domain: domain, Active: true, Public: false, RSS: ""}
	tester := &syncTesterFake{reachable: map[string]bool{domain: true}}
	rss := &syncRSSFake{links: map[string]string{domain: "https://c.example.net/rss?passkey=x"}}
	notifier := &syncNotifierFake{}
	service := newSyncService(t, map[string]string{"c.example.net": "new"}, store, tester, nil, rss, notifier)

	if _, err := service.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// 私有站且 RSS 为空：连通时应顺手补上 RSS 地址。
	if store.rssUpdates[domain] != "https://c.example.net/rss?passkey=x" {
		t.Fatalf("RSS 未更新: %+v", store.rssUpdates)
	}
	if len(store.updated) != 0 {
		t.Fatalf("不应更新 Cookie: %+v", store.updated)
	}

	// 已有 RSS 时不再重复生成。
	withRSS := newSyncStoreFake()
	withRSS.sites[domain] = Site{ID: 3, Domain: domain, Active: true, Public: false, RSS: "https://c.example.net/rss"}
	repeat := newSyncService(t, map[string]string{"c.example.net": "new"}, withRSS, tester, nil, rss, notifier)
	if _, err := repeat.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(withRSS.rssUpdates) != 0 {
		t.Fatalf("已有 RSS 不应重写: %+v", withRSS.rssUpdates)
	}

	// 公开站不生成 RSS。
	public := newSyncStoreFake()
	public.sites[domain] = Site{ID: 3, Domain: domain, Active: true, Public: true}
	publicService := newSyncService(t, map[string]string{"c.example.net": "new"}, public, tester, nil, rss, notifier)
	if _, err := publicService.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(public.rssUpdates) != 0 {
		t.Fatalf("公开站不应生成 RSS: %+v", public.rssUpdates)
	}
}

func TestSyncAddsKnownIndexerSite(t *testing.T) {
	domain := URLDomain("d.example.net")
	store := newSyncStoreFake()
	indexers := &syncIndexerFake{indexers: map[string]Indexer{
		domain: {
			Name: "D站", Domain: "d.example.net", Adapter: "nexusphp", Public: true,
			Config: map[string]any{"passkey": "abc"},
		},
	}}
	tester := &syncTesterFake{reachable: map[string]bool{domain: true}}
	notifier := &syncNotifierFake{}
	service := newSyncService(t, map[string]string{"d.example.net": "1"}, store, tester, indexers, nil, notifier)

	result, err := service.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(store.added) != 1 {
		t.Fatalf("新增站点数 = %d", len(store.added))
	}
	site := store.added[0]
	if site.Name != "D站" || site.Domain != domain || site.Adapter != "nexusphp" {
		t.Fatalf("新增站点 = %+v", site)
	}
	// 裸域名要补 https://，否则后续请求会因缺协议失败。
	if site.URL != "https://d.example.net" {
		t.Fatalf("站点 URL = %q", site.URL)
	}
	if site.Cookie != "sid=1" {
		t.Fatalf("Cookie = %q", site.Cookie)
	}
	if site.RSS != "" {
		t.Fatalf("公开站不应带 RSS: %q", site.RSS)
	}
	if result.Added != 1 || result.Updated != 0 || result.Failed != 0 {
		t.Fatalf("计数 = %+v", result)
	}
	if len(notifier.domains) != 1 {
		t.Fatalf("事件 = %v", notifier.domains)
	}
	// 配置必须是拷贝：改站点配置不能污染索引器定义。
	site.Config["passkey"] = "mutated"
	if indexers.indexers[domain].Config["passkey"] != "abc" {
		t.Fatal("站点配置未做拷贝")
	}
}

func TestSyncAddsPrivateIndexerWithRSS(t *testing.T) {
	domain := URLDomain("e.example.net")
	store := newSyncStoreFake()
	indexers := &syncIndexerFake{indexers: map[string]Indexer{
		domain: {Name: "E站", Domain: "e.example.net", Adapter: "mtorrent", Public: false},
	}}
	tester := &syncTesterFake{reachable: map[string]bool{domain: true}}
	rss := &syncRSSFake{links: map[string]string{domain: "https://e.example.net/rss"}}
	service := newSyncService(t, map[string]string{"e.example.net": "1"}, store, tester, indexers, rss, nil)

	if _, err := service.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(store.added) != 1 || store.added[0].RSS != "https://e.example.net/rss" {
		t.Fatalf("新增站点 = %+v", store.added)
	}
}

func TestSyncCountsFailuresWithoutAborting(t *testing.T) {
	domain := URLDomain("f.example.net")
	store := newSyncStoreFake()
	// 适配器为空：无法建站，计一次失败。
	emptyAdapter := &syncIndexerFake{indexers: map[string]Indexer{
		domain: {Name: "F站", Domain: "f.example.net", Adapter: "  "},
	}}
	tester := &syncTesterFake{reachable: map[string]bool{domain: true}}
	service := newSyncService(t, map[string]string{"f.example.net": "1"}, store, tester, emptyAdapter, nil, nil)
	result, err := service.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result.Failed != 1 || result.Added != 0 || len(store.added) != 0 {
		t.Fatalf("计数 = %+v", result)
	}
	if !strings.Contains(result.Message, "1个站点添加失败，下次同步时将重试，也可以手动添加") {
		t.Fatalf("消息 = %q", result.Message)
	}

	// 探测不通：同样计失败且不新增。
	unreachable := newSyncStoreFake()
	indexers := &syncIndexerFake{indexers: map[string]Indexer{
		domain: {Name: "F站", Domain: "f.example.net", Adapter: "nexusphp"},
	}}
	silent := newSyncService(t, map[string]string{"f.example.net": "1"}, unreachable,
		&syncTesterFake{reachable: map[string]bool{}}, indexers, nil, nil)
	result, err = silent.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result.Failed != 1 || len(unreachable.added) != 0 {
		t.Fatalf("计数 = %+v", result)
	}

	// 入库失败：计失败但不中断（其它站点还会继续同步）。
	failing := newSyncStoreFake()
	failing.addErr = errors.New("写库失败")
	broken := newSyncService(t, map[string]string{"f.example.net": "1"}, failing, tester, indexers, nil, nil)
	result, err = broken.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("计数 = %+v", result)
	}
}

func TestSyncSkipsUnknownAndBlacklistedDomains(t *testing.T) {
	store := newSyncStoreFake()
	unknown := &syncIndexerFake{indexers: map[string]Indexer{
		URLDomain("g.example.net"): {Name: "G站", Domain: "g.example.net", Adapter: "nexusphp"},
		URLDomain("h.example.net"): {Name: "H站", Domain: "h.example.net", Adapter: "nexusphp"},
		URLDomain("i.example.net"): {Name: "I站", Domain: "i.example.net", Adapter: "nexusphp"},
	}}
	tester := &syncTesterFake{reachable: map[string]bool{
		URLDomain("g.example.net"): true,
		URLDomain("h.example.net"): true,
		URLDomain("i.example.net"): true,
	}}
	service := newSyncService(t, map[string]string{
		"g.example.net": "1",
		"h.example.net": "1",
		"i.example.net": "1",
		"z.example.net": "1", // 未知索引器
	}, store, tester, unknown, nil, nil)

	// 黑名单可以写 CookieCloud 域名、索引器域名或扩展域名。
	result, err := service.Sync(context.Background(), SyncOptions{
		Blacklist: []string{" g.example.net ", "", "h.example.net"},
	})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(store.added) != 1 || store.added[0].Domain != URLDomain("i.example.net") {
		t.Fatalf("新增站点 = %+v", store.added)
	}
	if result.Added != 1 || result.Failed != 0 {
		t.Fatalf("计数 = %+v", result)
	}
}

func TestSyncUsesIndexerDomainWhenGroupingMatches(t *testing.T) {
	// CookieCloud 归组域名与索引器域名一致时，用索引器域名建站。
	domain := URLDomain("j.example.net")
	store := newSyncStoreFake()
	indexers := &syncIndexerFake{indexers: map[string]Indexer{
		domain: {Name: "J站", Domain: "j.example.net", Adapter: "nexusphp", ExtDomains: []string{"j2.example.net"}},
	}}
	service := newSyncService(t, map[string]string{"j.example.net": "1"}, store,
		&syncTesterFake{reachable: map[string]bool{domain: true}}, indexers, nil, nil)
	if _, err := service.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(store.added) != 1 || store.added[0].Domain != domain {
		t.Fatalf("新增站点 = %+v", store.added)
	}
}

func TestSyncTreatsInactiveExistingSiteAsUnknown(t *testing.T) {
	domain := URLDomain("k.example.net")
	store := newSyncStoreFake()
	// 站点存在但被禁用：按「未知」处理，已知索引器时应重新新增。
	store.sites[domain] = Site{ID: 9, Domain: domain, Active: false}
	indexers := &syncIndexerFake{indexers: map[string]Indexer{
		domain: {Name: "K站", Domain: "k.example.net", Adapter: "nexusphp"},
	}}
	notifier := &syncNotifierFake{}
	service := newSyncService(t, map[string]string{"k.example.net": "1"}, store,
		&syncTesterFake{reachable: map[string]bool{domain: true}}, indexers, nil, notifier)
	result, err := service.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result.Added != 1 || len(store.added) != 1 {
		t.Fatalf("计数 = %+v 新增 = %+v", result, store.added)
	}
	if len(store.updated) != 0 {
		t.Fatalf("禁用站点不应更新 Cookie: %+v", store.updated)
	}
	if len(notifier.domains) != 1 {
		t.Fatalf("事件 = %v", notifier.domains)
	}
}

func TestSyncPropagatesStoreAndLookupErrors(t *testing.T) {
	domain := URLDomain("l.example.net")
	cookies := map[string]string{"l.example.net": "1"}

	lookup := newSyncStoreFake()
	lookup.siteErr = errors.New("查询失败")
	service := newSyncService(t, cookies, lookup, nil, nil, nil, nil)
	if _, err := service.Sync(context.Background(), SyncOptions{}); err == nil {
		t.Fatal("查询失败应向上返回")
	}

	write := newSyncStoreFake()
	write.sites[domain] = Site{ID: 1, Domain: domain, Active: true}
	write.cookieErr = errors.New("写 Cookie 失败")
	service = newSyncService(t, cookies, write, &syncTesterFake{reachable: map[string]bool{}}, nil, nil, nil)
	if _, err := service.Sync(context.Background(), SyncOptions{}); err == nil {
		t.Fatal("写 Cookie 失败应向上返回")
	}

	indexerFail := newSyncStoreFake()
	service = newSyncService(t, cookies, indexerFail, nil, &syncIndexerFake{err: errors.New("索引器不可用")}, nil, nil)
	if _, err := service.Sync(context.Background(), SyncOptions{}); err == nil {
		t.Fatal("索引器查询失败应向上返回")
	}

	rssFail := newSyncStoreFake()
	rssFail.sites[domain] = Site{ID: 1, Domain: domain, Active: true, Public: false}
	rssFail.rssErr = errors.New("写 RSS 失败")
	service = newSyncService(t, cookies, rssFail, &syncTesterFake{reachable: map[string]bool{domain: true}},
		nil, &syncRSSFake{links: map[string]string{domain: "https://l.example.net/rss"}}, nil)
	if _, err := service.Sync(context.Background(), SyncOptions{}); err == nil {
		t.Fatal("写 RSS 失败应向上返回")
	}
}

func TestSyncStopsOnCancelledContext(t *testing.T) {
	store := newSyncStoreFake()
	service := newSyncService(t, map[string]string{"m.example.net": "1"}, store, nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Sync(ctx, SyncOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
	if store.siteCalls != 0 {
		t.Fatalf("取消后仍查询了 %d 次", store.siteCalls)
	}
}

func TestSyncWithoutOptionalCollaborators(t *testing.T) {
	// 没有探测器、解析器、RSS、事件通知时也要能跑完（全部依赖都是可选的）。
	store := newSyncStoreFake()
	store.sites[URLDomain("n.example.net")] = Site{ID: 1, Domain: URLDomain("n.example.net"), Active: true, Public: false}
	service := newSyncService(t, map[string]string{"n.example.net": "1"}, store, nil, nil, nil, nil)
	result, err := service.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// 没有探测器时视为「不通」→ 更新 Cookie。
	if result.Updated != 1 || store.updated[URLDomain("n.example.net")] != "sid=1" {
		t.Fatalf("计数 = %+v 更新 = %+v", result, store.updated)
	}
	if len(store.rssUpdates) != 0 {
		t.Fatalf("没有 RSS 解析器时不应写 RSS: %+v", store.rssUpdates)
	}
}
