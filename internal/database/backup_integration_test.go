//go:build integration

package database

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// TestBackupRestoreRoundTrip 走真实 PostgreSQL：导出 → 删数据 → 恢复 → 数据回来。
//
// 需要一个"可以随便清空"的库（迁移由本测试自己执行）：
//
//	$env:MOVIEPILOT_TEST_DATABASE_URL='postgres://user:pass@127.0.0.1:5432/moviepilot_backup_test?sslmode=disable'
//	go test -tags integration ./internal/database -run TestBackupRestoreRoundTrip
func TestBackupRestoreRoundTrip(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(siteStatisticsTestDatabaseEnv))
	if dsn == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", siteStatisticsTestDatabaseEnv)
	}
	ctx := t.Context()
	if err := Migrate(ctx, config.Config{DatabaseURL: dsn}); err != nil {
		t.Fatalf("迁移测试库: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	t.Cleanup(pool.Close)
	db := &DB{Pool: pool}

	const sentinelKey = "backup-roundtrip-sentinel"
	if _, err := pool.Exec(ctx, `
		INSERT INTO settings (key, value, encrypted, updated_at)
		VALUES ($1, '"before"'::jsonb, false, now())
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = now()`, sentinelKey); err != nil {
		t.Fatalf("写入哨兵设置: %v", err)
	}

	var buffer bytes.Buffer
	manifest, err := BackupTo(ctx, db, &buffer, "test")
	if err != nil {
		t.Fatalf("导出备份: %v", err)
	}
	if len(manifest.Tables) == 0 {
		t.Fatal("备份清单里没有表")
	}
	if !containsString(manifest.Tables, "settings") {
		t.Fatalf("备份清单缺少 settings: %v", manifest.Tables)
	}
	if containsString(manifest.Tables, "river_job") {
		t.Fatal("运行时队列表不应进入备份")
	}
	if manifest.SchemaVersion <= 0 {
		t.Fatalf("结构版本 = %d，应从 goose_db_version 读到迁移版本", manifest.SchemaVersion)
	}

	// 删掉哨兵，模拟"数据丢了"。
	if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE key = $1`, sentinelKey); err != nil {
		t.Fatalf("删除哨兵设置: %v", err)
	}

	restored, err := RestoreFrom(ctx, db, bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatalf("恢复备份: %v", err)
	}
	if restored.SchemaVersion != manifest.SchemaVersion {
		t.Fatalf("恢复后的清单版本 = %d，期望 %d", restored.SchemaVersion, manifest.SchemaVersion)
	}

	var value string
	if err := pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key = $1`, sentinelKey).Scan(&value); err != nil {
		t.Fatalf("恢复后读哨兵设置: %v", err)
	}
	if !strings.Contains(value, "before") {
		t.Fatalf("恢复后的哨兵值 = %s", value)
	}

	// 自增序列必须被修正：新插入不能和恢复回来的 id 撞车。
	if _, err := pool.Exec(ctx, `INSERT INTO users (username, password_hash) VALUES ('backup-seq-check', 'x')`); err != nil {
		t.Fatalf("恢复后新增用户（序列应已修正）: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE username = 'backup-seq-check'`); err != nil {
		t.Fatalf("清理序列检查用户: %v", err)
	}
}

// TestRestoreRejectsForeignArchive 验证被篡改/不匹配的包不会写坏库。
func TestRestoreRejectsForeignArchive(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(siteStatisticsTestDatabaseEnv))
	if dsn == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", siteStatisticsTestDatabaseEnv)
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	t.Cleanup(pool.Close)
	db := &DB{Pool: pool}

	if _, err := RestoreFrom(ctx, db, strings.NewReader("不是压缩包")); err == nil {
		t.Fatal("非 gzip 输入应报错")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
