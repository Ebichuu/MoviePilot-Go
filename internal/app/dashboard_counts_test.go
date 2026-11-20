package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

type stubMediaCounter struct {
	counts database.DashboardMediaCounts
	err    error
	calls  int
}

func (s *stubMediaCounter) DashboardMediaCounts(context.Context) (database.DashboardMediaCounts, error) {
	s.calls++
	return s.counts, s.err
}

type stubTransferReader struct {
	stats     downloader.TransferStats
	err       error
	calls     int
	sawNilCtx bool
}

func (s *stubTransferReader) TransferStats(ctx context.Context) (downloader.TransferStats, error) {
	s.calls++
	s.sawNilCtx = ctx == nil
	return s.stats, s.err
}

func TestNewDashboardRuntimeFillsStartTime(t *testing.T) {
	runtime := newDashboardRuntime(&stubMediaCounter{}, &stubTransferReader{}, time.Time{})
	if runtime.started.IsZero() {
		t.Fatal("零值开始时间应回落到当前时间")
	}
	if time.Since(runtime.started) > time.Minute {
		t.Fatalf("开始时间不合理: %v", runtime.started)
	}
	// 显式时间必须原样保留（运行时长依赖它）。
	fixed := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	if got := newDashboardRuntime(nil, nil, fixed).started; !got.Equal(fixed) {
		t.Fatalf("开始时间 = %v", got)
	}
}

func TestDashboardRuntimeStatistic(t *testing.T) {
	counter := &stubMediaCounter{counts: database.DashboardMediaCounts{
		MovieCount: 11, TVCount: 22, EpisodeCount: 33, UserCount: 44,
		MovieCountMonth: 5, TVCountMonth: 6,
	}}
	statistic, err := newDashboardRuntime(counter, nil, time.Now()).Statistic(context.Background())
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if counter.calls != 1 {
		t.Fatalf("调用次数 = %d", counter.calls)
	}
	if statistic.MovieCount != 11 || statistic.TVCount != 22 || statistic.EpisodeCount != 33 ||
		statistic.UserCount != 44 || statistic.MovieCountMonth != 5 || statistic.TVCountMonth != 6 {
		t.Fatalf("统计映射错误: %+v", statistic)
	}

	// 数据源不可用必须报错，不能用零值冒充「库里没有内容」。
	var nilRuntime *dashboardRuntime
	if _, err := nilRuntime.Statistic(context.Background()); err == nil {
		t.Fatal("nil 运行时应报错")
	}
	if _, err := newDashboardRuntime(nil, nil, time.Now()).Statistic(context.Background()); err == nil {
		t.Fatal("缺少统计源应报错")
	}
	// 数据源自身的错误要原样上抛，便于上层区分「库挂了」和「没数据」。
	boom := errors.New("数据库不可用")
	if _, err := newDashboardRuntime(&stubMediaCounter{err: boom}, nil, time.Now()).
		Statistic(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("错误 = %v", err)
	}
}

func TestDashboardRuntimeStorageAndDownloader(t *testing.T) {
	reader := &stubTransferReader{stats: downloader.TransferStats{
		UsedSpace: 300, FreeSpace: 700, DownloadSpeed: 1024, UploadSpeed: 2048,
		DownloadSize: 4096, UploadSize: 8192,
	}}
	runtime := newDashboardRuntime(nil, reader, time.Now())

	storage, err := runtime.Storage(context.Background())
	if err != nil {
		t.Fatalf("空间统计失败: %v", err)
	}
	// 与 V2 `_build_storage` 一致：总量 = 已用 + 可用。
	if storage.UsedStorage != 300 || storage.TotalStorage != 1000 {
		t.Fatalf("空间 = %+v", storage)
	}

	info, err := runtime.Downloader(context.Background())
	if err != nil {
		t.Fatalf("下载器统计失败: %v", err)
	}
	if info.DownloadSpeed != 1024 || info.UploadSpeed != 2048 ||
		info.DownloadSize != 4096 || info.UploadSize != 8192 || info.FreeSpace != 700 {
		t.Fatalf("下载器信息 = %+v", info)
	}

	// 没有下载器数据源时两个接口都必须报错，而不是返回 0 速度。
	empty := newDashboardRuntime(nil, nil, time.Now())
	if _, err := empty.Storage(context.Background()); err == nil {
		t.Fatal("缺少下载器源时空间统计应报错")
	}
	if _, err := empty.Downloader(context.Background()); err == nil {
		t.Fatal("缺少下载器源时速率统计应报错")
	}
}

func TestDashboardRuntimeTransferStatsDefaultsContext(t *testing.T) {
	reader := &stubTransferReader{}
	runtime := newDashboardRuntime(nil, reader, time.Now())
	// nil 上下文不能直接传给下游（下游可能立刻解引用），必须补成 Background。
	if _, err := runtime.Downloader(nil); err != nil {
		t.Fatalf("nil 上下文应被兜住: %v", err)
	}
	if reader.sawNilCtx {
		t.Fatal("下游不应收到 nil 上下文")
	}
}

func TestDashboardRuntimeStoragePropagatesReaderError(t *testing.T) {
	boom := errors.New("下载器不可达")
	runtime := newDashboardRuntime(nil, &stubTransferReader{err: boom}, time.Now())
	if _, err := runtime.Storage(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("空间统计错误 = %v", err)
	}
	if _, err := runtime.Downloader(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("下载器统计错误 = %v", err)
	}
}

func TestNewCookieCloudSiteDirectory(t *testing.T) {
	// store 未初始化时必须返回 nil，让站点分类接口回退到 V2 的空分类语义。
	if got := newCookieCloudSiteDirectory(nil); got != nil {
		t.Fatalf("store 未初始化应返回 nil，实际 %T", got)
	}
	if got := newCookieCloudSiteDirectory(&database.Store{}); got == nil {
		t.Fatal("store 存在时应返回目录端口")
	}
}
