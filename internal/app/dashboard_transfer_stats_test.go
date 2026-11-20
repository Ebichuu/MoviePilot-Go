package app

import (
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

// 一个下载器都没配置是正常状态：仪表盘回零值，不留下每 3 秒一条的 WARN。
func TestDashboardTransferStatsTreatsNoDownloaderAsEmpty(t *testing.T) {
	dashboard := newDashboardRuntime(nil, &stubTransferReader{err: downloader.ErrNoTransferStats}, time.Now())

	storage, err := dashboard.Storage(t.Context())
	if err != nil {
		t.Fatalf("没有下载器时 Storage() error = %v，应为零值而不是错误", err)
	}
	if storage.TotalStorage != 0 || storage.UsedStorage != 0 {
		t.Fatalf("storage = %+v，期望零值", storage)
	}

	info, err := dashboard.Downloader(t.Context())
	if err != nil {
		t.Fatalf("没有下载器时 Downloader() error = %v", err)
	}
	if info.DownloadSpeed != 0 || info.UploadSpeed != 0 || info.FreeSpace != 0 {
		t.Fatalf("downloader = %+v，期望零值", info)
	}
}

// 真的连不上下载器时仍然要上抛，让接口留 WARN，而不是把故障吞成 0。
func TestDashboardTransferStatsPropagatesRealFailures(t *testing.T) {
	dashboard := newDashboardRuntime(nil, &stubTransferReader{err: errors.New("连接下载器超时")}, time.Now())

	if _, err := dashboard.Storage(t.Context()); err == nil {
		t.Fatal("真实故障应当上抛")
	}
	if _, err := dashboard.Downloader(t.Context()); err == nil {
		t.Fatal("真实故障应当上抛")
	}
}

func TestDashboardTransferStatsPassesValuesThrough(t *testing.T) {
	reader := &stubTransferReader{stats: downloader.TransferStats{
		DownloadSpeed: 1024,
		UploadSpeed:   256,
		FreeSpace:     100,
		UsedSpace:     40,
	}}
	dashboard := newDashboardRuntime(nil, reader, time.Now())

	// 下载盘总量 = 已用 + 可用，与 V2 `_build_storage` 一致。
	storage, err := dashboard.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage() error = %v", err)
	}
	if storage.TotalStorage != 140 || storage.UsedStorage != 40 {
		t.Fatalf("storage = %+v", storage)
	}

	info, err := dashboard.Downloader(t.Context())
	if err != nil {
		t.Fatalf("Downloader() error = %v", err)
	}
	if info.DownloadSpeed != 1024 || info.UploadSpeed != 256 {
		t.Fatalf("downloader = %+v", info)
	}
}

// stats 为 nil（仪表盘没有接到下载器管理器）时仍然算故障。
func TestDashboardTransferStatsWithoutReader(t *testing.T) {
	dashboard := newDashboardRuntime(nil, nil, time.Now())
	if _, err := dashboard.Storage(t.Context()); err == nil {
		t.Fatal("没有读取器时应当报错")
	}
}
