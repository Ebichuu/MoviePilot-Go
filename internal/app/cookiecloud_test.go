package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/cookiecloud"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type cookieCloudSiteTesterAdapterFake struct {
	name string
	err  error
}

func (a *cookieCloudSiteTesterAdapterFake) Name() string { return a.name }

func (a *cookieCloudSiteTesterAdapterFake) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

func (a *cookieCloudSiteTesterAdapterFake) TestSite(context.Context) error { return a.err }

// TestCookieCloudConnectivityTesterUsesAdapterProbe 覆盖「已有专用适配器」的分支：
// 能用适配器探测时不要退化成通用首页/登录页探测。
func TestCookieCloudConnectivityTesterUsesAdapterProbe(t *testing.T) {
	t.Parallel()

	coordinator, err := indexer.NewCoordinator(2)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	t.Cleanup(coordinator.Close)

	newTester := func(probeErr error) *cookieCloudConnectivityTester {
		return &cookieCloudConnectivityTester{
			store:  &database.Store{},
			search: coordinator,
			buildAdapter: func(database.Site) (indexer.Adapter, error) {
				return &cookieCloudSiteTesterAdapterFake{name: "fake", err: probeErr}, nil
			},
		}
	}
	site := cookiecloud.Site{Name: "站点", URL: "https://a.example", Adapter: "Unit3D"}

	success, message, err := newTester(nil).Test(t.Context(), site)
	if err != nil || !success || message == "" {
		t.Fatalf("探测成功 = (%t, %q, %v)", success, message, err)
	}

	// 适配器探测失败：失败标记为 false，对外只给脱敏原因（底层错误细节不外泄）。
	success, message, err = newTester(errors.New("登录态失效")).Test(t.Context(), site)
	if success || err == nil || message != "站点连接失败" {
		t.Fatalf("探测失败 = (%t, %q, %v)", success, message, err)
	}

	// 适配器能构造但不实现 SiteTester、又没有 HTTP 客户端兜底时，如实报未初始化。
	tester := &cookieCloudConnectivityTester{
		store: &database.Store{},
		buildAdapter: func(database.Site) (indexer.Adapter, error) {
			return &telegramSearchAdapterFake{name: "fake"}, nil
		},
	}
	if success, message, err := tester.Test(t.Context(), site); success ||
		message != "站点 HTTP 客户端未初始化" || err == nil {
		t.Fatalf("无探测能力 = (%t, %q, %v)", success, message, err)
	}
}

// TestCookieCloudRSSResolverReadsAdvertisedFeed 覆盖 RSS 声明的解析与失败脱敏。
func TestCookieCloudRSSResolverReadsAdvertisedFeed(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"></head></html>`))
	}))
	t.Cleanup(server.Close)

	client, err := indexer.NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	client.SetAllowPrivateHosts(true)
	resolver := cookieCloudRSSResolver{siteHTTP: client}

	feed, reason := resolver.RSSLink(t.Context(), cookiecloud.Site{URL: server.URL}, "")
	if reason != "" {
		t.Fatalf("RSS 解析失败: %q", reason)
	}
	if feed != server.URL+"/feed.xml" {
		t.Fatalf("feed = %q", feed)
	}

	// 站点不可达：返回空链接 + 稳定原因，不把底层网络错误细节带出去。
	feed, reason = resolver.RSSLink(t.Context(), cookiecloud.Site{URL: "http://127.0.0.1:1/"}, "")
	if feed != "" || reason != "读取站点 RSS 声明失败" {
		t.Fatalf("不可达站点 = (%q, %q)", feed, reason)
	}
}
