//go:build integration

package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// 本文件覆盖备份任务在「目录不可建」「数据库不可用」两种环境故障下的返回路径。

func backupErrorPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestBackupSettingsRejectsUncreatableDir(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("写入占位文件: %v", err)
	}
	if _, err := BackupSettings(t.Context(), backupErrorPool(t), filepath.Join(blocker, "nested"), 0); err == nil {
		t.Fatal("父路径是文件时创建目录应失败")
	}
}

func TestBackupSettingsReportsQueryFailure(t *testing.T) {
	if _, err := BackupSettings(t.Context(), backupErrorPool(t), t.TempDir(), 0); err == nil {
		t.Fatal("数据库不可用时应返回错误")
	}
}

func TestBackupSweepWorkReportsFailure(t *testing.T) {
	worker := &backupSweepWorker{db: backupErrorPool(t)}
	err := worker.Work(context.Background(), &river.Job[BackupSweepArgs]{
		Args: BackupSweepArgs{Dir: t.TempDir()},
	})
	if err == nil {
		t.Fatal("备份失败时 Work 应返回错误")
	}
}

func TestPruneBackupsKeepsAllWhenKeepDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, backupFilePrefix+"20260101-000000.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("写入备份文件: %v", err)
	}
	pruneBackups(dir, 0)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("keep<=0 不应删除备份: %v", err)
	}

	if keep := backupKeep("not-a-number"); keep != 7 {
		t.Fatalf("非法保留份数应回退 7，得到 %d", keep)
	}
	if keep := backupKeep("-3"); keep != 7 {
		t.Fatalf("负数保留份数应回退 7，得到 %d", keep)
	}
	if keep := backupKeep(" 5 "); keep != 5 {
		t.Fatalf("合法保留份数 = %d, want 5", keep)
	}
}
