package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type telegramSearchAdapterFake struct {
	name      string
	resources []indexer.Resource
	err       error
	mu        sync.Mutex
	query     indexer.Query
	calls     int
}

func (a *telegramSearchAdapterFake) Name() string { return a.name }

func (a *telegramSearchAdapterFake) Search(_ context.Context, query indexer.Query) ([]indexer.Resource, error) {
	a.mu.Lock()
	a.query = query
	a.calls++
	a.mu.Unlock()
	return a.resources, a.err
}

func (a *telegramSearchAdapterFake) lastQuery() indexer.Query {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.query
}

func newTelegramServicesTest(t *testing.T, adapters ...indexer.Adapter) (*telegramServices, *indexer.Coordinator) {
	t.Helper()
	coordinator, err := indexer.NewCoordinator(2)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	t.Cleanup(coordinator.Close)
	return newTelegramServices(coordinator, indexer.NewRegistry(adapters), nil, ""), coordinator
}

func TestTelegramServicesSearchTitlesGuards(t *testing.T) {
	t.Parallel()

	var nilServices *telegramServices
	if _, err := nilServices.SearchTitles(t.Context(), "keyword", 10); err == nil || err.Error() != "搜索服务未就绪" {
		t.Fatalf("nil services error = %v", err)
	}
	if _, err := (&telegramServices{}).SearchTitles(t.Context(), "keyword", 10); err == nil || err.Error() != "搜索服务未就绪" {
		t.Fatalf("缺依赖 error = %v", err)
	}

	services, _ := newTelegramServicesTest(t)
	// 一个站点都没启用时如实说「没有启用的站点」，不要返回空结果让用户以为搜过了。
	if _, err := services.SearchTitles(t.Context(), "keyword", 10); err == nil || err.Error() != "没有启用的站点" {
		t.Fatalf("空注册表 error = %v", err)
	}
}

func TestTelegramServicesSearchTitlesMapsResults(t *testing.T) {
	t.Parallel()

	adapter := &telegramSearchAdapterFake{
		name: "fake-site",
		resources: []indexer.Resource{
			{Site: " A站 ", Title: " 片名 A ", SizeBytes: 100, Seeders: 10, DetailURL: " https://a/1 "},
			{Site: "B站", Title: "片名 B", SizeBytes: 200, Seeders: 5, DetailURL: "https://b/2"},
			{Site: "A站", Title: "   ", DetailURL: "https://a/blank"},
		},
	}
	services, _ := newTelegramServicesTest(t, adapter)

	results, err := services.SearchTitles(t.Context(), " 片名 ", 2)
	if err != nil {
		t.Fatalf("SearchTitles() error = %v", err)
	}
	// 关键词两端空白被裁剪，分页固定从第 1 页开始。
	query := adapter.lastQuery()
	if query.Keyword != "片名" || query.Page != 1 || query.Limit != 2 {
		t.Fatalf("查询 = %+v", query)
	}
	if len(results) != 2 {
		t.Fatalf("结果 = %#v", results)
	}
	// 按做种数倒序：A(10) 在 B(5) 前；站点名由协调器统一成适配器名。
	if results[0].Title != "片名 A" || results[0].Site != "fake-site" || results[0].Size != 100 || results[0].Seeders != 10 {
		t.Fatalf("结果[0] = %+v", results[0])
	}
	if results[0].DetailURL != "https://a/1" {
		t.Fatalf("详情链接未裁剪 = %q", results[0].DetailURL)
	}
	if results[1].Title != "片名 B" || results[1].Site != "fake-site" {
		t.Fatalf("结果[1] = %+v", results[1])
	}
}

func TestTelegramServicesSearchTitlesClampsLimit(t *testing.T) {
	t.Parallel()

	adapter := &telegramSearchAdapterFake{name: "fake-site"}
	services, _ := newTelegramServicesTest(t, adapter)
	for _, tt := range []struct{ requested, want int }{
		{requested: 0, want: telegramSearchLimit},
		{requested: -3, want: telegramSearchLimit},
		{requested: telegramSearchLimit + 100, want: telegramSearchLimit},
		{requested: 5, want: 5},
	} {
		if _, err := services.SearchTitles(t.Context(), "k", tt.requested); err != nil {
			t.Fatalf("requested=%d error = %v", tt.requested, err)
		}
		if got := adapter.lastQuery().Limit; got != tt.want {
			t.Fatalf("requested=%d 下发给站点的 limit = %d, want %d", tt.requested, got, tt.want)
		}
	}
}

func TestTelegramServicesSearchTitlesSkipsBlankTitles(t *testing.T) {
	t.Parallel()

	adapter := &telegramSearchAdapterFake{name: "fake-site", resources: []indexer.Resource{
		{Title: " ", DetailURL: "https://a/blank"},
		{Title: "片名", DetailURL: "https://a/real"},
		{Title: "", DetailURL: "https://a/empty"},
	}}
	services, _ := newTelegramServicesTest(t, adapter)
	results, err := services.SearchTitles(t.Context(), "k", 10)
	if err != nil {
		t.Fatalf("SearchTitles() error = %v", err)
	}
	// 空标题对 Telegram 用户没有意义，直接跳过而不是发一条只有站点的消息。
	if len(results) != 1 || results[0].Title != "片名" {
		t.Fatalf("结果 = %#v", results)
	}
}

func TestTelegramServicesSearchTitlesBlankOnlyReturnsEmpty(t *testing.T) {
	t.Parallel()

	adapter := &telegramSearchAdapterFake{name: "fake-site", resources: []indexer.Resource{{Title: " "}}}
	services, _ := newTelegramServicesTest(t, adapter)
	results, err := services.SearchTitles(t.Context(), "k", 10)
	if err != nil {
		t.Fatalf("SearchTitles() error = %v", err)
	}
	if results == nil || len(results) != 0 {
		t.Fatalf("结果 = %#v", results)
	}
}

func TestTelegramServicesSearchTitlesTreatsAdapterFailureAsEmpty(t *testing.T) {
	t.Parallel()

	// 单站点失败在协调器里变成事件级错误，不阻断其他站点；这里只有一个站点，
	// 结果是空列表 + nil 错误（机器人回「没搜到」，而不是整条命令报错）。
	adapter := &telegramSearchAdapterFake{name: "fake-site", err: errors.New("站点不可达")}
	services, _ := newTelegramServicesTest(t, adapter)
	results, err := services.SearchTitles(t.Context(), "k", 10)
	if err != nil {
		t.Fatalf("SearchTitles() error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("结果 = %#v", results)
	}
}

func TestTelegramServicesSearchTitlesReportsClosedCoordinator(t *testing.T) {
	t.Parallel()

	coordinator, err := indexer.NewCoordinator(1)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	coordinator.Close()
	services := newTelegramServices(coordinator, indexer.NewRegistry([]indexer.Adapter{
		&telegramSearchAdapterFake{name: "fake-site"},
	}), nil, "")
	// 协调器已关闭时要如实报错，不能假装「没搜到」。
	if _, err := services.SearchTitles(t.Context(), "k", 10); err == nil {
		t.Fatal("SearchTitles() error = nil")
	}
}

func TestTelegramServicesReadOnlyGuards(t *testing.T) {
	t.Parallel()

	var nilServices *telegramServices
	if _, err := nilServices.Subscriptions(t.Context()); err == nil || err.Error() != "订阅查询未就绪" {
		t.Fatalf("nil services 订阅 error = %v", err)
	}
	if _, err := nilServices.Sites(t.Context()); err == nil || err.Error() != "站点查询未就绪" {
		t.Fatalf("nil services 站点 error = %v", err)
	}
	// 缺存储或没有操作账号时同样如实拒绝，不去猜一个用户名查订阅。
	if _, err := (&telegramServices{}).Subscriptions(t.Context()); err == nil || err.Error() != "订阅查询未就绪" {
		t.Fatalf("缺存储 error = %v", err)
	}
	if _, err := (&telegramServices{username: "  "}).Subscriptions(t.Context()); err == nil || err.Error() != "订阅查询未就绪" {
		t.Fatalf("空账号 error = %v", err)
	}
	if _, err := (&telegramServices{}).Sites(t.Context()); err == nil || err.Error() != "站点查询未就绪" {
		t.Fatalf("缺存储站点 error = %v", err)
	}
}

func TestStartTelegramBotsRequiresDependencies(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := &notificationRuntime{}
	if got := startTelegramBots(nil, logger, runtime, nil, nil, nil); got != nil {
		t.Fatalf("nil ctx = %#v", got)
	}
	if got := startTelegramBots(t.Context(), nil, runtime, nil, nil, nil); got != nil {
		t.Fatalf("nil logger = %#v", got)
	}
	if got := startTelegramBots(t.Context(), logger, nil, nil, nil, nil); got != nil {
		t.Fatalf("nil runtime = %#v", got)
	}
}

func TestStartTelegramBotsReturnsManagerWithoutChannels(t *testing.T) {
	// 没有 Telegram 通道时也要返回管理器（它会空转等配置更新），
	// 但不能真的发起轮询请求。
	ctx, cancel := context.WithCancel(t.Context())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := startTelegramBots(ctx, logger, &notificationRuntime{}, nil, nil, nil)
	if manager == nil {
		t.Fatal("manager = nil")
	}
	cancel()
}
