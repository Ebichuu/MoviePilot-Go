package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
)

// downloadClientStatDouble 在普通下载器替身上增加全局传输统计能力。
type downloadClientStatDouble struct {
	downloadClientTestDouble
	stats    downloader.TransferStats
	statErr  error
	statCall atomic.Int32
}

func (c *downloadClientStatDouble) TransferStats(context.Context) (downloader.TransferStats, error) {
	c.statCall.Add(1)
	return c.stats, c.statErr
}

// downloadClientHistoryDouble 增加「已完成任务」能力。
type downloadClientHistoryDouble struct {
	downloadClientTestDouble
	completed []downloader.Task
	err       error
	onList    func(context.Context) ([]downloader.Task, error)
	calls     atomic.Int32
}

func (c *downloadClientHistoryDouble) ListCompleted(ctx context.Context) ([]downloader.Task, error) {
	c.calls.Add(1)
	if c.onList != nil {
		return c.onList(ctx)
	}
	return c.completed, c.err
}

// downloadClientControlDouble 增加单任务查询与控制能力。
type downloadClientControlDouble struct {
	downloadClientTestDouble
	task       downloader.Task
	found      bool
	inspectErr error
	controlErr error
	hashes     []string
	actions    []downloader.TaskControlAction
}

func (c *downloadClientControlDouble) InspectControlTask(_ context.Context, hash string) (downloader.Task, bool, error) {
	c.hashes = append(c.hashes, hash)
	return c.task, c.found, c.inspectErr
}

func (c *downloadClientControlDouble) ControlTask(_ context.Context, hash string, action downloader.TaskControlAction) error {
	c.hashes = append(c.hashes, hash)
	c.actions = append(c.actions, action)
	return c.controlErr
}

func TestDownloadRuntimeTransferStatsAggregates(t *testing.T) {
	t.Parallel()

	first := &downloadClientStatDouble{stats: downloader.TransferStats{
		DownloadSpeed: 100, UploadSpeed: 50, DownloadSize: 1000, UploadSize: 500,
		FreeSpace: 300, UsedSpace: 100,
	}}
	second := &downloadClientStatDouble{stats: downloader.TransferStats{
		DownloadSpeed: 200, UploadSpeed: 25, DownloadSize: 2000, UploadSize: 250,
		FreeSpace: 200, UsedSpace: 400,
	}}
	// 统计失败的下载器只跳过它自己，不能拖垮整张卡片。
	failing := &downloadClientStatDouble{statErr: errors.New("downloader unavailable")}
	// 没有统计能力的下载器（例如还没接入的类型）直接跳过。
	plain := &downloadClientTestDouble{}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "qb-1", client: first},
		{name: "qb-2", client: second},
		{name: "qb-3", client: failing},
		{name: "plain", client: plain},
	}}

	stats, err := runtime.TransferStats(t.Context())
	if err != nil {
		t.Fatalf("TransferStats() error = %v", err)
	}
	// 速率与累计流量按下载器相加；剩余/已用空间取最大值（多下载器常指向同一块盘）。
	want := downloader.TransferStats{
		DownloadSpeed: 300, UploadSpeed: 75, DownloadSize: 3000, UploadSize: 750,
		FreeSpace: 300, UsedSpace: 400,
	}
	if stats != want {
		t.Fatalf("TransferStats() = %+v, want %+v", stats, want)
	}
	if first.statCall.Load() != 1 || second.statCall.Load() != 1 || failing.statCall.Load() != 1 {
		t.Fatalf("统计调用次数 first=%d second=%d failing=%d",
			first.statCall.Load(), second.statCall.Load(), failing.statCall.Load())
	}
}

func TestDownloadRuntimeTransferStatsGuards(t *testing.T) {
	t.Parallel()

	var nilRuntime *downloadRuntime
	if _, err := nilRuntime.TransferStats(t.Context()); err == nil || err.Error() != "下载器统计不可用" {
		t.Fatalf("nil runtime error = %v", err)
	}
	runtime := &downloadRuntime{clients: []downloadClientEntry{{name: "qb", client: &downloadClientStatDouble{}}}}
	if _, err := runtime.TransferStats(nil); err == nil || err.Error() != "下载器统计不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}

	// 已取消的上下文原样返回，调度层据此区分「取消」与「没有下载器」。
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	reader := &downloadClientStatDouble{}
	runtime = &downloadRuntime{clients: []downloadClientEntry{{name: "qb", client: reader}}}
	if _, err := runtime.TransferStats(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 error = %v", err)
	}
	if reader.statCall.Load() != 0 {
		t.Fatalf("取消后仍调用了下载器统计")
	}

	// 只有失败读数（或没有读数）时报错，而不是返回一张全 0 的卡片。
	allFailing := &downloadRuntime{clients: []downloadClientEntry{
		{name: "qb", client: &downloadClientStatDouble{statErr: errors.New("boom")}},
		{name: "plain", client: &downloadClientTestDouble{}},
	}}
	if _, err := allFailing.TransferStats(t.Context()); err == nil || err.Error() != "没有可用的下载器统计" {
		t.Fatalf("无可用统计 error = %v", err)
	}
	if _, err := (&downloadRuntime{}).TransferStats(t.Context()); err == nil || err.Error() != "没有可用的下载器统计" {
		t.Fatalf("空运行时 error = %v", err)
	}
}

func TestDownloadRuntimeListDownloadHistoryAggregates(t *testing.T) {
	t.Parallel()

	first := &downloadClientHistoryDouble{completed: []downloader.Task{
		{Hash: "A", Title: "电影 A"}, {Hash: "B", Title: "电影 B"},
	}}
	second := &downloadClientHistoryDouble{completed: []downloader.Task{{Hash: "C", Title: "剧集 C"}}}
	// 未实现 CompletedTaskLister 的下载器不参与历史汇总，也不伪造空记录。
	plain := &downloadClientTestDouble{}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "客厅 qB", client: first},
		{name: "Archive", client: second},
		{name: "legacy", client: plain},
	}}

	tasks, err := runtime.ListDownloadHistory(t.Context(), "  archive ")
	if err != nil {
		t.Fatalf("ListDownloadHistory() error = %v", err)
	}
	want := []httpapi.NamedDownloadTask{{Downloader: "Archive", Task: downloader.Task{Hash: "C", Title: "剧集 C"}}}
	if len(tasks) != 1 || tasks[0] != want[0] {
		t.Fatalf("ListDownloadHistory() = %#v, want %#v", tasks, want)
	}
	if first.calls.Load() != 0 || second.calls.Load() != 1 {
		t.Fatalf("查询次数 first=%d second=%d", first.calls.Load(), second.calls.Load())
	}

	all, err := runtime.ListDownloadHistory(t.Context(), "")
	if err != nil {
		t.Fatalf("ListDownloadHistory(全部) error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("全部历史 = %#v", all)
	}
	// 名称未命中时返回空切片而不是 nil。
	none, err := runtime.ListDownloadHistory(t.Context(), "missing")
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("未命中 = %#v %v", none, err)
	}
}

func TestDownloadRuntimeListDownloadHistoryGuardsAndFailure(t *testing.T) {
	t.Parallel()

	var nilRuntime *downloadRuntime
	if _, err := nilRuntime.ListDownloadHistory(t.Context(), ""); err == nil || err.Error() != "下载历史查询不可用" {
		t.Fatalf("nil runtime error = %v", err)
	}
	if _, err := (&downloadRuntime{}).ListDownloadHistory(nil, ""); err == nil || err.Error() != "下载历史查询不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (&downloadRuntime{}).ListDownloadHistory(cancelled, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 error = %v", err)
	}

	// 下游错误不向上泄漏（可能含地址或凭据），只给出稳定文案。
	failing := &downloadRuntime{clients: []downloadClientEntry{{
		name: "qb", client: &downloadClientHistoryDouble{err: errors.New("http://user:password-secret@host")},
	}}}
	if _, err := failing.ListDownloadHistory(t.Context(), ""); err == nil || err.Error() != "查询下载历史失败" {
		t.Fatalf("下游失败 error = %v", err)
	} else if strings.Contains(err.Error(), "password-secret") {
		t.Fatalf("错误泄漏下游细节: %v", err)
	}

	// 查询途中被取消：把真实的取消原因交回去，而不是报「查询失败」。
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	midCancel := &downloadRuntime{clients: []downloadClientEntry{{
		name: "qb", client: &downloadClientHistoryDouble{onList: func(context.Context) ([]downloader.Task, error) {
			cancel()
			return nil, errors.New("transport closed")
		}},
	}}}
	if _, err := midCancel.ListDownloadHistory(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消中的错误 = %v", err)
	}
}

func TestDownloadRuntimeListDownloadHistoryCapsAggregate(t *testing.T) {
	t.Parallel()

	completed := make([]downloader.Task, 2500)
	for index := range completed {
		completed[index] = downloader.Task{Hash: fmt.Sprintf("H%04d", index)}
	}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "first", client: &downloadClientHistoryDouble{completed: completed}},
		{name: "second", client: &downloadClientHistoryDouble{completed: []downloader.Task{{Hash: "EXTRA"}}}},
	}}
	tasks, err := runtime.ListDownloadHistory(t.Context(), "")
	if err != nil {
		t.Fatalf("ListDownloadHistory() error = %v", err)
	}
	// 上限 2000：超出部分直接截断，且不再向后续下载器发查询。
	if len(tasks) != 2000 {
		t.Fatalf("历史条数 = %d, want 2000", len(tasks))
	}
}

func TestDownloadRuntimeFindDownloadTask(t *testing.T) {
	t.Parallel()

	controller := &downloadClientControlDouble{
		task: downloader.Task{Hash: "HASH-1", Title: "电影"}, found: true,
	}
	plain := &downloadClientTestDouble{}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "qb", client: controller},
		{name: "Backup", isDefault: true, client: plain},
	}}

	task, found, err := runtime.FindDownloadTask(t.Context(), "qb", "HASH-1")
	if err != nil || !found {
		t.Fatalf("FindDownloadTask() = found %v err %v", found, err)
	}
	// 返回的实例名必须是命中的那个，前端据此决定控制哪台下载器。
	if task.Downloader != "qb" || task.Task.Hash != "HASH-1" {
		t.Fatalf("FindDownloadTask() = %#v", task)
	}
	if len(controller.hashes) != 1 || controller.hashes[0] != "HASH-1" {
		t.Fatalf("查询 hash = %#v", controller.hashes)
	}

	// 未指定名字时选默认下载器；它没有单任务能力时不报错，只是「找不到」。
	if _, found, err := runtime.FindDownloadTask(t.Context(), "  ", "HASH-1"); err != nil || found {
		t.Fatalf("默认无控制器 = found %v err %v", found, err)
	}
	if _, found, err := runtime.FindDownloadTask(t.Context(), "nope", "HASH-1"); err != nil || found {
		t.Fatalf("未知下载器 = found %v err %v", found, err)
	}

	// 控制器说没有这个 hash：这是正常的「未命中」，不是错误。
	missing := &downloadRuntime{clients: []downloadClientEntry{{name: "qb", client: &downloadClientControlDouble{}}}}
	if _, found, err := missing.FindDownloadTask(t.Context(), "qb", "HASH-404"); err != nil || found {
		t.Fatalf("未命中 = found %v err %v", found, err)
	}

	// 控制器查询失败原样交给调用方，由 HTTP 边界决定文案。
	boom := errors.New("qbittorrent 下载任务查询失败")
	broken := &downloadRuntime{clients: []downloadClientEntry{{
		name: "qb", client: &downloadClientControlDouble{inspectErr: boom},
	}}}
	if _, found, err := broken.FindDownloadTask(t.Context(), "qb", "HASH-1"); !errors.Is(err, boom) || found {
		t.Fatalf("查询失败 = found %v err %v", found, err)
	}
}

func TestDownloadRuntimeFindDownloadTaskGuards(t *testing.T) {
	t.Parallel()

	var nilRuntime *downloadRuntime
	if _, _, err := nilRuntime.FindDownloadTask(t.Context(), "qb", "H"); err == nil || err.Error() != "下载任务查询不可用" {
		t.Fatalf("nil runtime error = %v", err)
	}
	runtime := &downloadRuntime{clients: []downloadClientEntry{{name: "qb", client: &downloadClientControlDouble{}}}}
	if _, _, err := runtime.FindDownloadTask(nil, "qb", "H"); err == nil || err.Error() != "下载任务查询不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := runtime.FindDownloadTask(cancelled, "qb", "H"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 error = %v", err)
	}
}

func TestDownloadRuntimeControlDownloadTask(t *testing.T) {
	t.Parallel()

	controller := &downloadClientControlDouble{}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "qb", isDefault: true, client: controller},
		{name: "plain", client: &downloadClientTestDouble{}},
	}}

	if err := runtime.ControlDownloadTask(t.Context(), "qb", "HASH-1", downloader.TaskControlStop); err != nil {
		t.Fatalf("ControlDownloadTask() error = %v", err)
	}
	if len(controller.actions) != 1 || controller.actions[0] != downloader.TaskControlStop ||
		len(controller.hashes) != 1 || controller.hashes[0] != "HASH-1" {
		t.Fatalf("控制调用 = %#v / %#v", controller.hashes, controller.actions)
	}

	// 未给名字时落到默认下载器。
	if err := runtime.ControlDownloadTask(t.Context(), "", "HASH-2", downloader.TaskControlDelete); err != nil {
		t.Fatalf("ControlDownloadTask(默认) error = %v", err)
	}
	if len(controller.actions) != 2 || controller.actions[1] != downloader.TaskControlDelete {
		t.Fatalf("默认控制调用 = %#v", controller.actions)
	}

	// 没有控制器能力 / 名字不存在 / 运行时缺能力：统一「控制不可用」，不静默成功。
	if err := runtime.ControlDownloadTask(t.Context(), "plain", "H", downloader.TaskControlStart); err == nil ||
		err.Error() != "下载任务控制不可用" {
		t.Fatalf("无控制器 error = %v", err)
	}
	if err := runtime.ControlDownloadTask(t.Context(), "nope", "H", downloader.TaskControlStart); err == nil ||
		err.Error() != "下载任务控制不可用" {
		t.Fatalf("未知下载器 error = %v", err)
	}
	if err := (&downloadRuntime{}).ControlDownloadTask(t.Context(), "", "H", downloader.TaskControlStart); err == nil ||
		err.Error() != "下载任务控制不可用" {
		t.Fatalf("空运行时 error = %v", err)
	}

	// 下游拒绝原样返回（例如 hash 非法），调用方需要区分「不支持」与「执行失败」。
	rejected := errors.New("下载任务控制参数无效")
	failing := &downloadRuntime{clients: []downloadClientEntry{{
		name: "qb", client: &downloadClientControlDouble{controlErr: rejected},
	}}}
	if err := failing.ControlDownloadTask(t.Context(), "qb", "bad", downloader.TaskControlStop); !errors.Is(err, rejected) {
		t.Fatalf("下游拒绝 error = %v", err)
	}
}

func TestDownloadRuntimeControlDownloadTaskGuards(t *testing.T) {
	t.Parallel()

	var nilRuntime *downloadRuntime
	if err := nilRuntime.ControlDownloadTask(t.Context(), "qb", "H", downloader.TaskControlStop); err == nil ||
		err.Error() != "下载任务控制不可用" {
		t.Fatalf("nil runtime error = %v", err)
	}
	runtime := &downloadRuntime{clients: []downloadClientEntry{{name: "qb", client: &downloadClientControlDouble{}}}}
	if err := runtime.ControlDownloadTask(nil, "qb", "H", downloader.TaskControlStop); err == nil ||
		err.Error() != "下载任务控制不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runtime.ControlDownloadTask(cancelled, "qb", "H", downloader.TaskControlStop); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 error = %v", err)
	}
}

func TestDownloadRuntimeCloseIdleConnections(t *testing.T) {
	t.Parallel()

	var nilRuntime *downloadRuntime
	nilRuntime.CloseIdleConnections()

	first := &downloadClientTestDouble{}
	second := &downloadClientTestDouble{}
	runtime := &downloadRuntime{clients: []downloadClientEntry{
		{name: "qb-1", client: first}, {name: "qb-2", client: second},
	}}
	runtime.CloseIdleConnections()
	runtime.CloseIdleConnections()
	if first.closed.Load() != 1 || second.closed.Load() != 1 {
		t.Fatalf("关闭次数 first=%d second=%d", first.closed.Load(), second.closed.Load())
	}
	if len(runtime.clients) != 0 {
		t.Fatalf("关闭后仍保留客户端: %#v", runtime.clients)
	}
}

func TestDownloadRuntimeListDownloadingGuards(t *testing.T) {
	t.Parallel()

	var nilRuntime *downloadRuntime
	if _, err := nilRuntime.ListDownloading(t.Context(), ""); err == nil || err.Error() != "下载任务查询不可用" {
		t.Fatalf("nil runtime error = %v", err)
	}

	client := &downloadClientTestDouble{}
	runtime := &downloadRuntime{clients: []downloadClientEntry{{name: "qb", client: client}}}
	if _, err := runtime.ListDownloading(nil, ""); err == nil || err.Error() != "下载任务查询不可用" {
		t.Fatalf("nil ctx error = %v", err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runtime.ListDownloading(cancelled, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 error = %v", err)
	}
	// 取消发生在查询开始前，不能把请求再打到下载器上。
	if client.calls.Load() != 0 {
		t.Fatalf("取消后仍查询了 %d 次", client.calls.Load())
	}

	// 查询途中被取消：返回真实的取消原因，而不是「查询下载任务失败」。
	ctx, cancelQuery := context.WithCancel(t.Context())
	midCancel := &downloadRuntime{clients: []downloadClientEntry{{name: "qb", client: &downloadClientTestDouble{
		list: func(context.Context) ([]downloader.Task, error) {
			cancelQuery()
			return nil, errors.New("transport closed")
		},
	}}}}
	if _, err := midCancel.ListDownloading(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消中的错误 = %v", err)
	}
}

func TestLoadDownloadRuntimeLoadsEverySupportedType(t *testing.T) {
	t.Parallel()

	const (
		qbID           = "12e2fe2b-2ed7-4c6a-b5ba-1c1c0f7a3ad8"
		transmissionID = "58b08ab7-c5c1-43da-82af-16d3a3f4912e"
		rtorrentID     = "4b981b3b-0d3c-4919-868d-2dedf4693c80"
	)
	store := &downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{
		{
			ID: qbID, Name: "客厅 qB", Type: "qbittorrent", Enabled: true, Default: true,
			Config: map[string]any{"host": "http://127.0.0.1:8080", "username": "alice", "password": "secret"},
		},
		{
			ID: transmissionID, Name: "Transmission", Type: "TRANSMISSION", Enabled: true,
			Config: map[string]any{"host": "http://127.0.0.1:9091"},
		},
		{
			ID: rtorrentID, Name: "rTorrent", Type: "rtorrent", Enabled: true,
			Config: map[string]any{"host": "http://127.0.0.1:8080/RPC2"},
		},
	}}

	runtime, err := loadDownloadRuntime(t.Context(), store, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("loadDownloadRuntime() error = %v", err)
	}
	defer runtime.CloseIdleConnections()

	for _, id := range []string{qbID, transmissionID, rtorrentID} {
		if _, err := runtime.submitters.ResolveSubmitter(id); err != nil {
			t.Fatalf("下载器 %s 未注册: %v", id, err)
		}
	}
	// 客户端按设置顺序保留，默认下载器标记随设置走（控制接口按它挑实例）。
	if len(runtime.clients) != 3 {
		t.Fatalf("客户端数 = %d", len(runtime.clients))
	}
	wantNames := []string{"客厅 qB", "Transmission", "rTorrent"}
	defaults := []bool{true, false, false}
	for index, entry := range runtime.clients {
		if entry.name != wantNames[index] || entry.isDefault != defaults[index] {
			t.Fatalf("客户端[%d] = %+v", index, entry)
		}
	}
}

func TestLoadDownloadRuntimeRejectsBrokenClientConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		want string
	}{
		{name: "qBittorrent", kind: "qbittorrent", want: "qBittorrent 运行时配置无效"},
		{name: "Transmission", kind: "transmission", want: "Transmission 运行时配置无效"},
		{name: "rTorrent", kind: "rtorrent", want: "rTorrent 运行时配置无效"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{{
				ID: "12e2fe2b-2ed7-4c6a-b5ba-1c1c0f7a3ad8", Name: tt.name, Type: tt.kind,
				Enabled: true, Config: map[string]any{},
			}}}
			runtime, err := loadDownloadRuntime(t.Context(), store, time.Second, nil)
			if runtime != nil {
				t.Fatalf("配置非法时必须返回 nil，实际 %#v", runtime)
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadDownloadRuntimeGuards(t *testing.T) {
	t.Parallel()

	if _, err := loadDownloadRuntime(t.Context(), nil, time.Second, nil); err == nil ||
		err.Error() != "下载器设置存储不可用" {
		t.Fatalf("nil store error = %v", err)
	}

	// 下载器 ID 不是 UUID：注册表拒绝，启动必须失败而不是静默丢掉该下载器。
	store := &downloaderRuntimeTestStore{found: true, settings: []downloadconfig.Downloader{{
		ID: "not-a-uuid", Name: "broken", Type: "qbittorrent", Enabled: true,
		Config: map[string]any{"host": "http://127.0.0.1:8080"},
	}}}
	if _, err := loadDownloadRuntime(t.Context(), store, time.Second, nil); err == nil ||
		!strings.Contains(err.Error(), "初始化下载器注册表") {
		t.Fatalf("非法 ID error = %v", err)
	}

	var nilRuntime *downloadRuntime
	if err := nilRuntime.Reload(t.Context(), &downloaderRuntimeTestStore{}, time.Second, nil); err == nil ||
		err.Error() != "下载器运行时不可用" {
		t.Fatalf("nil runtime Reload error = %v", err)
	}
	if err := (&downloadRuntime{}).Reload(t.Context(), &downloaderRuntimeTestStore{}, time.Second, nil); err == nil ||
		err.Error() != "下载器运行时不可用" {
		t.Fatalf("缺注册表 Reload error = %v", err)
	}
}
