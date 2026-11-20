package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

const (
	// cleanupSweepKind 是定期清理任务。V2 `scheduler.py` 的清理类任务在 Go 侧的落点。
	cleanupSweepKind = "cleanup_sweep"
	// queueCleanupSweep 单独一条队列，避免清理任务挤占业务队列。
	queueCleanupSweep = "cleanup_sweep"
	// cleanupSweepInterval：默认 6 小时一次，足够清掉陈旧的已发布记录。
	cleanupSweepInterval = 6 * time.Hour
	// cleanupSweepRetention：保留最近 30 天的历史。
	cleanupSweepRetention = 30 * 24 * time.Hour
	// cleanupSweepPeriodicID：River 周期任务 ID。
	cleanupSweepPeriodicID = "cleanup_sweep_periodic"
)

// CleanupSweepArgs 是清理任务的入参：只带截止时间，便于重放与测试。
type CleanupSweepArgs struct {
	Cutoff time.Time `json:"cutoff"`
}

// Kind 实现 river.JobArgs。
func (CleanupSweepArgs) Kind() string { return cleanupSweepKind }

// CleanupSweepResult 是一次清理的实际删除行数，只用于日志与自检。
type CleanupSweepResult struct {
	OutboxRows int64
}

type cleanupSweepWorker struct {
	river.WorkerDefaults[CleanupSweepArgs]

	db     *pgxpool.Pool
	logger *slog.Logger
}

// Work 清理「已发布且超过保留期」的业务通知发件箱行。
//
// 只删已发布的：未发布的行代表尚未投递的事实，任何情况下都不能删。
// 清理失败要让 River 记录并重试，不做静默忽略。
func (w *cleanupSweepWorker) Work(ctx context.Context, job *river.Job[CleanupSweepArgs]) error {
	if w.db == nil {
		return nil
	}
	cutoff := job.Args.Cutoff
	if cutoff.IsZero() {
		cutoff = time.Now().UTC().Add(-cleanupSweepRetention)
	}
	result, err := CleanupOldRecords(ctx, w.db, cutoff)
	if err != nil {
		w.log().Warn("清理任务失败", "cutoff", cutoff.Format(time.RFC3339), "err", err)
		return err
	}
	if result.OutboxRows > 0 {
		w.log().Info("清理任务完成", "cutoff", cutoff.Format(time.RFC3339), "outbox_rows", result.OutboxRows)
	}
	return nil
}

func (w *cleanupSweepWorker) log() *slog.Logger {
	if w.logger != nil {
		return w.logger
	}
	return slog.Default()
}

// CleanupOldRecords 删除已发布且早于 cutoff 的发件箱行。
// 抽成普通函数便于单测直接调用，不依赖 River 运行时。
func CleanupOldRecords(ctx context.Context, db *pgxpool.Pool, cutoff time.Time) (CleanupSweepResult, error) {
	tag, err := db.Exec(ctx, `
		DELETE FROM business_notification_outbox
		WHERE published_at IS NOT NULL AND created_at < $1`, cutoff)
	if err != nil {
		return CleanupSweepResult{}, err
	}
	return CleanupSweepResult{OutboxRows: tag.RowsAffected()}, nil
}

// registerCleanupSweepWorker 注册清理 worker。返回 false 表示本进程不启用。
func registerCleanupSweepWorker(workers *river.Workers, db *pgxpool.Pool, logger *slog.Logger) (bool, error) {
	if db == nil {
		return false, nil
	}
	// river.AddWorker 在重复注册时 panic：kind 唯一性由本文件常量保证。
	river.AddWorker(workers, &cleanupSweepWorker{db: db, logger: logger})
	return true, nil
}

// cleanupSweepPeriodicJob 每 6 小时插入一次清理任务。
func cleanupSweepPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(cleanupSweepInterval),
		func() (river.JobArgs, *river.InsertOpts) {
			return CleanupSweepArgs{Cutoff: time.Now().UTC().Add(-cleanupSweepRetention)}, nil
		},
		&river.PeriodicJobOpts{ID: cleanupSweepPeriodicID},
	)
}

// EnqueueCleanupSweep 立即插入一轮过期数据清理任务。
func (m *Manager) EnqueueCleanupSweep(ctx context.Context) error {
	if ctx == nil {
		return errors.New("清理上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.client == nil || !m.cleanupSweepEnabled {
		return errors.New("数据清理任务未启用")
	}
	opts := river.InsertOpts{Queue: queueCleanupSweep, MaxAttempts: 3}
	if _, err := m.client.Insert(ctx, CleanupSweepArgs{Cutoff: time.Now().UTC().Add(-cleanupSweepRetention)}, &opts); err != nil {
		return err
	}
	return nil
}
