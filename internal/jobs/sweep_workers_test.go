package jobs

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

func sweepLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestBackupSweepScheduleWiring(t *testing.T) {
	if got := (BackupSweepArgs{}).Kind(); got != backupSweepKind {
		t.Fatalf("kind = %q", got)
	}
	if backupSweepPeriodicJob() == nil {
		t.Fatal("备份周期任务不应为空")
	}
	// 每天一次；名字与队列分开，避免备份挤占业务队列。
	if backupSweepInterval != 24*time.Hour {
		t.Fatalf("备份间隔 = %v", backupSweepInterval)
	}
	if queueBackupSweep == queueCleanupSweep {
		t.Fatal("备份与清理不应共用队列")
	}
	if !strings.HasPrefix(backupFilePrefix, "settings-") {
		t.Fatalf("备份文件前缀 = %q", backupFilePrefix)
	}
}

func TestCleanupSweepScheduleWiring(t *testing.T) {
	if got := (CleanupSweepArgs{}).Kind(); got != cleanupSweepKind {
		t.Fatalf("kind = %q", got)
	}
	if cleanupSweepPeriodicJob() == nil {
		t.Fatal("清理周期任务不应为空")
	}
	if cleanupSweepInterval != 6*time.Hour {
		t.Fatalf("清理间隔 = %v", cleanupSweepInterval)
	}
	// 保留期必须远大于清理间隔，否则会删掉刚刚发布的行。
	if cleanupSweepRetention != 30*24*time.Hour {
		t.Fatalf("保留期 = %v", cleanupSweepRetention)
	}
	if cleanupSweepRetention <= cleanupSweepInterval {
		t.Fatal("保留期应远大于清理间隔")
	}
}

func TestBackupSweepWorkerSkipsWithoutDirectory(t *testing.T) {
	t.Setenv("MOVIEPILOT_BACKUP_DIR", "")
	var buf bytes.Buffer
	worker := &backupSweepWorker{logger: sweepLogger(&buf)}
	if err := worker.Work(context.Background(), &river.Job[BackupSweepArgs]{Args: BackupSweepArgs{}}); err != nil {
		t.Fatalf("未配置目录时不应报错: %v", err)
	}
	// 没配置目录就跳过，并且必须留下可见日志——不能假装备份成功。
	if !strings.Contains(buf.String(), "MOVIEPILOT_BACKUP_DIR") {
		t.Fatalf("缺少跳过日志: %s", buf.String())
	}
	if strings.Contains(buf.String(), "备份任务完成") {
		t.Fatalf("不应打印完成日志: %s", buf.String())
	}
}

func TestBackupSweepWorkerWithoutDatabase(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOVIEPILOT_BACKUP_DIR", dir)
	var buf bytes.Buffer
	worker := &backupSweepWorker{logger: sweepLogger(&buf)}

	// 没有数据库连接时直接返回，且不能凭空产生一个备份文件。
	if err := worker.Work(context.Background(), &river.Job[BackupSweepArgs]{Args: BackupSweepArgs{}}); err != nil {
		t.Fatalf("无数据库时不应报错: %v", err)
	}
	if err := worker.Work(context.Background(), &river.Job[BackupSweepArgs]{Args: BackupSweepArgs{Dir: dir}}); err != nil {
		t.Fatalf("无数据库时不应报错: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取备份目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("无数据库时不应写入文件: %v", entries)
	}
	if strings.Contains(buf.String(), "备份任务完成") {
		t.Fatalf("不应打印完成日志: %s", buf.String())
	}
}

func TestBackupSweepWorkerUsesJobDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "自定义", "备份")
	t.Setenv("MOVIEPILOT_BACKUP_DIR", t.TempDir())
	var buf bytes.Buffer
	worker := &backupSweepWorker{logger: sweepLogger(&buf)}
	// 入参目录优先于环境变量；这里没有数据库，所以只验证不会去碰环境目录。
	if err := worker.Work(context.Background(), &river.Job[BackupSweepArgs]{Args: BackupSweepArgs{Dir: dir}}); err != nil {
		t.Fatalf("无数据库时不应报错: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("无数据库时不应创建目录: %v", err)
	}
}

func TestCleanupSweepWorkerWithoutDatabase(t *testing.T) {
	var buf bytes.Buffer
	worker := &cleanupSweepWorker{logger: sweepLogger(&buf)}
	if err := worker.Work(context.Background(), &river.Job[CleanupSweepArgs]{Args: CleanupSweepArgs{}}); err != nil {
		t.Fatalf("无数据库时不应报错: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("无数据库时不应打印日志: %s", buf.String())
	}
}

func TestSweepWorkersSkipRegistrationWithoutDatabase(t *testing.T) {
	// 没有数据库的进程（例如只跑 API 的副本）不应启用这两个周期任务。
	workers := river.NewWorkers()
	if enabled, err := registerBackupSweepWorker(workers, nil, nil); enabled || err != nil {
		t.Fatalf("备份 worker 注册 = %v %v", enabled, err)
	}
	if enabled, err := registerCleanupSweepWorker(workers, nil, nil); enabled || err != nil {
		t.Fatalf("清理 worker 注册 = %v %v", enabled, err)
	}
}

func TestSweepWorkerLogFallsBackToDefault(t *testing.T) {
	if (&backupSweepWorker{}).log() == nil {
		t.Fatal("备份 worker 日志不应为 nil")
	}
	if (&cleanupSweepWorker{}).log() == nil {
		t.Fatal("清理 worker 日志不应为 nil")
	}
}
