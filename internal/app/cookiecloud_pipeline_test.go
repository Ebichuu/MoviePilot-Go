package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/cookiecloud"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

func TestSyncCookieCloudRequiresConfiguration(t *testing.T) {
	var nilSyncer *cookieCloudSyncer
	if _, err := nilSyncer.SyncCookieCloud(context.Background()); err == nil {
		t.Fatal("nil 同步器应报未配置")
	}
	if _, err := (&cookieCloudSyncer{}).SyncCookieCloud(context.Background()); err == nil {
		t.Fatal("缺 helper 的同步器应报未配置")
	}
	if _, err := (&cookieCloudSyncer{helper: cookiecloud.NewHelper(cookiecloud.Config{}, nil)}).
		SyncCookieCloud(context.Background()); err == nil {
		t.Fatal("缺 store 的同步器应报未配置")
	}
}

func TestCookieCloudSiteDirectoryGuards(t *testing.T) {
	cases := map[string]*cookieCloudSiteDirectory{
		"nil 目录":  nil,
		"缺 store": {},
	}
	for name, directory := range cases {
		if _, err := directory.entries(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "未初始化") {
			t.Fatalf("%s entries 错误 = %v", name, err)
		}
		if _, err := directory.directory(context.Background()); err == nil {
			t.Fatalf("%s directory 应报错", name)
		}
		if _, _, err := directory.SiteCategories(context.Background(), "example.com"); err == nil {
			t.Fatalf("%s SiteCategories 应报错", name)
		}
		if _, _, err := directory.IndexerByDomain(context.Background(), "example.com"); err == nil {
			t.Fatalf("%s IndexerByDomain 应报错", name)
		}
	}

	// 上下文缺失同样视为未初始化（否则会拿 nil ctx 去打数据库）。
	usable := &cookieCloudSiteDirectory{store: &database.Store{}}
	if _, err := usable.entries(nil); err == nil {
		t.Fatal("nil 上下文应报错")
	}
}

func TestCookieCloudConnectivityTesterGuards(t *testing.T) {
	var nilTester *cookieCloudConnectivityTester
	if ok, message, err := nilTester.Test(context.Background(), cookiecloud.Site{}); ok || err == nil || message == "" {
		t.Fatalf("nil 测试器应返回未初始化: %v %q %v", ok, message, err)
	}
	if ok, _, err := (&cookieCloudConnectivityTester{}).Test(context.Background(), cookiecloud.Site{}); ok || err == nil {
		t.Fatal("缺 store 的测试器应报错")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tester := &cookieCloudConnectivityTester{store: &database.Store{}}
	if _, _, err := tester.Test(cancelled, cookiecloud.Site{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文错误 = %v，期望 context.Canceled", err)
	}
}

func TestCookieCloudRSSResolverGuard(t *testing.T) {
	feedURL, message := cookieCloudRSSResolver{}.RSSLink(context.Background(), cookiecloud.Site{}, "")
	if feedURL != "" {
		t.Fatalf("未初始化时不应给出地址: %q", feedURL)
	}
	if !strings.Contains(message, "未初始化") {
		t.Fatalf("提示 = %q", message)
	}
}

func TestCookieCloudAddSiteGuards(t *testing.T) {
	if err := (&cookieCloudSiteStore{}).AddSite(context.Background(), cookiecloud.Site{Adapter: "mteam"}); err == nil {
		t.Fatal("缺 store 时不应新增站点")
	}
	// 适配器为空就等于没有抓取定义，直接拒绝。
	if err := (&cookieCloudSiteStore{store: &database.Store{}}).
		AddSite(context.Background(), cookiecloud.Site{Name: "x"}); err == nil ||
		!strings.Contains(err.Error(), "不完整") {
		t.Fatalf("缺适配器错误 = %v", err)
	}
	var nilStore *cookieCloudSiteStore
	if err := nilStore.AddSite(context.Background(), cookiecloud.Site{Adapter: "mteam"}); err == nil {
		t.Fatal("nil store 不应新增站点")
	}
}

func TestCookieCloudCacheSiteIconGuard(t *testing.T) {
	// 没有站点 HTTP 客户端时静默返回：图标是装饰，不该让同步失败。
	(&cookieCloudSiteStore{}).cacheSiteIcon(context.Background(), database.Site{})
	var nilStore *cookieCloudSiteStore
	nilStore.cacheSiteIcon(context.Background(), database.Site{})
}

func TestCookieCloudReloadIfChanged(t *testing.T) {
	store := &cookieCloudSiteStore{}

	// 没有 reload 回调时永远不做事。
	if err := store.reloadIfChanged(context.Background(), nil); err != nil {
		t.Fatalf("无回调应返回 nil: %v", err)
	}
	// 不脏就不重载。
	calls := 0
	if err := store.reloadIfChanged(context.Background(), func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("未变更应返回 nil: %v", err)
	}
	if calls != 0 {
		t.Fatalf("未变更时不应调用重载，实际 %d 次", calls)
	}

	// 脏 + 缺上下文：报错且必须保持脏，等下次带上下文的重试。
	store.markDirty()
	if err := store.reloadIfChanged(nil, func(context.Context) error {
		calls++
		return nil
	}); err == nil {
		t.Fatal("脏且缺上下文应报错")
	}
	if !store.dirty {
		t.Fatal("重载失败后必须保持脏标记，否则站点永远不再刷新")
	}

	// 脏 + 成功：清脏。
	if err := store.reloadIfChanged(context.Background(), func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	if store.dirty {
		t.Fatal("重载成功后应清除脏标记")
	}
	if calls != 1 {
		t.Fatalf("重载调用次数 = %d", calls)
	}

	// 脏 + 回调报错：错误上抛且保持脏。
	store.markDirty()
	if err := store.reloadIfChanged(context.Background(), func(context.Context) error {
		return errors.New("重载失败")
	}); err == nil || err.Error() != "重载失败" {
		t.Fatalf("回调错误未上抛: %v", err)
	}
	if !store.dirty {
		t.Fatal("回调失败后必须保持脏标记")
	}

	// nil store 安全。
	var nilStore *cookieCloudSiteStore
	if err := nilStore.reloadIfChanged(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("nil store 应返回 nil: %v", err)
	}
}
