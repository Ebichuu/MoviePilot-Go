package downloader

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNoTransferStats 表示当前没有任何可用的下载器。
//
// 这是「还没配置下载器」的正常状态，不是故障：仪表盘按它回零值，
// 不应该每个轮询周期都刷一条 WARN。
var ErrNoTransferStats = errors.New("没有可用的下载器统计")

// TaskObservationState is a closed set used by persistent download
// reconciliation. Unknown and missing tasks are intentionally distinct from
// completed tasks.
type TaskObservationState string

const (
	TaskObservationActive    TaskObservationState = "active"
	TaskObservationPaused    TaskObservationState = "paused"
	TaskObservationCompleted TaskObservationState = "completed"
	TaskObservationError     TaskObservationState = "error"
	TaskObservationMissing   TaskObservationState = "missing"
	TaskObservationUnknown   TaskObservationState = "unknown"
)

// TaskObservation is the minimal result needed to advance a download intent.
type TaskObservation struct {
	Hash  string
	State TaskObservationState
}

// TaskObserver reads a bounded batch of downloader tasks by normalized hash.
// Implementations return one observation for every requested unique hash.
type TaskObserver interface {
	ObserveTasks(ctx context.Context, hashes []string) (map[string]TaskObservation, error)
}

// ReconciliationTarget is the secret-free persistent identity needed by a
// periodic status sweep.
type ReconciliationTarget struct {
	ID           uuid.UUID
	Username     string
	DownloaderID string
	State        IntentState
	TorrentHash  string
	UpdatedAt    time.Time
}

// TransferStats 是仪表盘「下载器」卡片需要的汇总数值，单位与 V2
// `DownloaderInfo` 保持一致：速率为字节/秒，流量与空间为字节。
type TransferStats struct {
	DownloadSpeed float64
	UploadSpeed   float64
	DownloadSize  float64
	UploadSize    float64
	FreeSpace     float64
	// UsedSpace 是下载器当前任务在下载盘上占用的总字节数，用于仪表盘存储卡片。
	UsedSpace float64
}

// TransferStatReader 是下载器的可选能力：能给出全局传输汇总的适配器实现它。
// 仪表盘只汇总实现了该接口的下载器，其余类型按「无统计」跳过。
type TransferStatReader interface {
	TransferStats(ctx context.Context) (TransferStats, error)
}
