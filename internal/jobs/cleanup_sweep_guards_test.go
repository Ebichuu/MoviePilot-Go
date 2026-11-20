package jobs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// 本文件覆盖清理任务 worker 的失败传播与日志选择。

func cleanupSweepUnavailablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCleanupSweepWorkerReportsFailures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := cleanupSweepUnavailablePool(t)

	worker := &cleanupSweepWorker{db: pool, logger: logger}
	if err := worker.Work(context.Background(), &river.Job[CleanupSweepArgs]{}); err == nil {
		t.Fatal("连接不可用应返回错误")
	}

	if err := (&cleanupSweepWorker{}).Work(context.Background(), &river.Job[CleanupSweepArgs]{}); err != nil {
		t.Fatalf("缺少连接时应静默跳过，实际 %v", err)
	}
}

func TestCleanupOldRecordsReportsFailures(t *testing.T) {
	pool := cleanupSweepUnavailablePool(t)
	if _, err := CleanupOldRecords(context.Background(), pool, time.Now().UTC()); err == nil {
		t.Fatal("连接不可用应报错")
	}
}

func TestCleanupSweepWorkerLogger(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := (&cleanupSweepWorker{logger: logger}).log(); got != logger {
		t.Fatal("应返回注入的日志器")
	}
	if got := (&cleanupSweepWorker{}).log(); got == nil {
		t.Fatal("未注入时应回退到默认日志器")
	}
}
