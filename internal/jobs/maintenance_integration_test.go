//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// 这两个任务的核心是 SQL：清理只删「已发布且过期」的行，备份只导出未加密行。
// 这两条纪律用 mock 验证不了，只能跑真实 PostgreSQL。
const maintenanceTestDatabaseEnv = "MOVIEPILOT_TEST_DATABASE_URL"

func maintenanceTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(maintenanceTestDatabaseEnv))
	if dsn == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", maintenanceTestDatabaseEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试数据库失败: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("测试数据库不可用: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCleanupOldRecordsDeletesOnlyPublishedExpiredRows(t *testing.T) {
	pool := maintenanceTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var username string
	if err := pool.QueryRow(ctx, `SELECT username FROM users ORDER BY username LIMIT 1`).Scan(&username); err != nil {
		t.Skipf("测试库没有可用用户，跳过: %v", err)
	}
	var eventID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM notification_events ORDER BY id LIMIT 1`).Scan(&eventID); err != nil {
		t.Skipf("测试库没有可用通知事件，跳过: %v", err)
	}

	suffix := uuid.NewString()
	expiredKey := "itest-cleanup-expired-" + suffix
	freshKey := "itest-cleanup-fresh-" + suffix
	pendingKey := "itest-cleanup-pending-" + suffix
	insert := `
		INSERT INTO business_notification_outbox
			(event_key, content_type, username, context, published_at, event_id, created_at)
		VALUES ($1, 'downloadAdded', $2, '{}'::jsonb, $3, $4, $5)`
	_, err := pool.Exec(ctx, insert, expiredKey, username, time.Now().UTC().Add(-40*24*time.Hour), eventID, time.Now().UTC().Add(-40*24*time.Hour))
	if err != nil {
		t.Fatalf("插入过期已发布行失败: %v", err)
	}
	_, err = pool.Exec(ctx, insert, freshKey, username, time.Now().UTC(), eventID, time.Now().UTC())
	if err != nil {
		t.Fatalf("插入新鲜已发布行失败: %v", err)
	}
	// 未发布的行代表尚未投递的事实，任何情况下都不能删。
	_, err = pool.Exec(ctx, insert, pendingKey, username, nil, nil, time.Now().UTC().Add(-40*24*time.Hour))
	if err != nil {
		t.Fatalf("插入未发布行失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx,
			`DELETE FROM business_notification_outbox WHERE event_key = ANY($1)`,
			[]string{expiredKey, freshKey, pendingKey})
	})

	result, err := CleanupOldRecords(ctx, pool, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if result.OutboxRows < 1 {
		t.Fatalf("应至少删除 1 行，实际 %d", result.OutboxRows)
	}

	remaining := func(key string) bool {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM business_notification_outbox WHERE event_key = $1`, key).Scan(&count); err != nil {
			t.Fatalf("查询 %s 失败: %v", key, err)
		}
		return count > 0
	}
	if remaining(expiredKey) {
		t.Fatal("过期的已发布行应被删除")
	}
	if !remaining(freshKey) {
		t.Fatal("未过期的已发布行不应被删除")
	}
	if !remaining(pendingKey) {
		t.Fatal("未发布的行即使很旧也不能删")
	}
}

func TestBackupSettingsSkipsEncryptedRows(t *testing.T) {
	pool := maintenanceTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := uuid.NewString()
	plainKey := "itest-backup-plain-" + suffix
	secretKey := "itest-backup-secret-" + suffix
	_, err := pool.Exec(ctx,
		`INSERT INTO settings (key, value, encrypted) VALUES ($1, '{"plain":true}'::jsonb, false), ($2, '{"secret":true}'::jsonb, true)`,
		plainKey, secretKey)
	if err != nil {
		t.Fatalf("插入配置项失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM settings WHERE key = ANY($1)`, []string{plainKey, secretKey})
	})

	dir := t.TempDir()
	result, err := BackupSettings(ctx, pool, dir, 3)
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	if result.Path == "" {
		t.Fatal("备份应产出文件路径")
	}
	if result.ExportedRows < 1 {
		t.Fatalf("应至少导出 1 行，实际 %d", result.ExportedRows)
	}
	if result.SkippedEncrypt < 1 {
		t.Fatalf("应至少跳过 1 个加密项，实际 %d", result.SkippedEncrypt)
	}
	if info, err := os.Stat(result.Path); err != nil || info.Size() == 0 {
		t.Fatalf("备份文件不可用: %v", err)
	}

	payload, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("读取备份文件失败: %v", err)
	}
	var decoded struct {
		Schema   string `json:"schema"`
		Settings []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("备份文件不是合法 JSON: %v", err)
	}
	if decoded.Schema != "moviepilot-go/settings-backup/1" {
		t.Fatalf("schema = %q", decoded.Schema)
	}
	keys := make(map[string]bool, len(decoded.Settings))
	for _, item := range decoded.Settings {
		keys[item.Key] = true
	}
	if !keys[plainKey] {
		t.Fatal("未加密项应出现在备份里")
	}
	if keys[secretKey] {
		t.Fatal("加密项不得写入备份文件")
	}
}

// worker 层：零 cutoff 用默认保留期，真实跑一遍不应报错。
func TestCleanupSweepWorkerRunsAgainstRealDatabase(t *testing.T) {
	pool := maintenanceTestPool(t)
	worker := &cleanupSweepWorker{db: pool}
	if err := worker.Work(context.Background(), &river.Job[CleanupSweepArgs]{}); err != nil {
		t.Fatalf("清理 worker 执行失败: %v", err)
	}
}

// worker 层：没有配置备份目录时应当「跳过」而不是伪造一个空备份。
func TestBackupSweepWorkerIntegrationSkipsWithoutDirectory(t *testing.T) {
	pool := maintenanceTestPool(t)
	t.Setenv("MOVIEPILOT_BACKUP_DIR", "")
	worker := &backupSweepWorker{db: pool}
	if err := worker.Work(context.Background(), &river.Job[BackupSweepArgs]{}); err != nil {
		t.Fatalf("未配置目录时应安静跳过: %v", err)
	}
}

func TestBackupSweepWorkerWritesIntoConfiguredDirectory(t *testing.T) {
	pool := maintenanceTestPool(t)
	dir := t.TempDir()
	t.Setenv("MOVIEPILOT_BACKUP_KEEP", "2")
	worker := &backupSweepWorker{db: pool}
	if err := worker.Work(context.Background(), &river.Job[BackupSweepArgs]{Args: BackupSweepArgs{Dir: dir}}); err != nil {
		t.Fatalf("备份 worker 执行失败: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, backupFilePrefix+"*.json"))
	if err != nil {
		t.Fatalf("列备份文件失败: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("应产出 1 份备份，实际 %d", len(matches))
	}
}

// 注册函数：有连接池就注册，没连接池就不注册。
func TestMaintenanceWorkersRegisterOnlyWithDatabase(t *testing.T) {
	workers := river.NewWorkers()
	if enabled, err := registerCleanupSweepWorker(workers, nil, nil); err != nil || enabled {
		t.Fatalf("没有数据库时不应注册清理任务: %v %v", enabled, err)
	}
	if enabled, err := registerBackupSweepWorker(workers, nil, nil); err != nil || enabled {
		t.Fatalf("没有数据库时不应注册备份任务: %v %v", enabled, err)
	}

	pool := maintenanceTestPool(t)
	if enabled, err := registerCleanupSweepWorker(river.NewWorkers(), pool, nil); err != nil || !enabled {
		t.Fatalf("有数据库时应注册清理任务: %v %v", enabled, err)
	}
	if enabled, err := registerBackupSweepWorker(river.NewWorkers(), pool, nil); err != nil || !enabled {
		t.Fatalf("有数据库时应注册备份任务: %v %v", enabled, err)
	}
}
