//go:build integration

package v2import

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

// TestImportIntoPostgres 在真实 PostgreSQL 上跑一遍迁移，确认数据真的落地并且可重复执行。
//
// 需要一个"可以随便写"的库（迁移由测试自己执行）：
//
//	$env:MOVIEPILOT_TEST_DATABASE_URL='postgres://user:pass@127.0.0.1:5432/moviepilot_backup_test?sslmode=disable'
//	go test -tags integration ./internal/v2import -run TestImportIntoPostgres
func TestImportIntoPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("MOVIEPILOT_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("未设置 MOVIEPILOT_TEST_DATABASE_URL，跳过 PostgreSQL 集成测试")
	}
	ctx := t.Context()
	if err := database.Migrate(ctx, config.Config{DatabaseURL: dsn}); err != nil {
		t.Fatalf("迁移测试库: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库: %v", err)
	}
	t.Cleanup(pool.Close)
	target := &database.DB{Pool: pool}

	// 清掉上一次可能留下的同名数据，保证断言稳定。
	// 这里用 TRUNCATE ... CASCADE：users 被订阅、通知出队列等多张表用外键引用，
	// 逐个 DELETE 会踩一堆外键约束；测试库本身就是一次性的。
	for _, statement := range []string{
		`TRUNCATE TABLE users, subscriptions, site_userdata, site_statistics CASCADE`,
		`DELETE FROM settings WHERE key = 'proxy'`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("准备测试数据（%s）: %v", statement, err)
		}
	}

	fixture := newV2Fixture(t)
	report, err := Run(ctx, Options{
		SQLitePath: fixture,
		DB:         target,
		BackupDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if report.BackupPath == "" {
		t.Fatal("写库迁移应先做整库备份")
	}
	if report.Written() == 0 {
		t.Fatalf("没有写入任何数据: %+v", report.Tables)
	}

	var (
		hash        string
		superUser   bool
		permissions string
	)
	if err := pool.QueryRow(ctx,
		`SELECT password_hash, is_superuser, permissions::text FROM users WHERE username = 'admin'`).
		Scan(&hash, &superUser, &permissions); err != nil {
		t.Fatalf("读取迁移后的用户: %v", err)
	}
	if !strings.HasPrefix(hash, "$2b$") {
		t.Fatalf("密码摘要应保持 bcrypt 格式，实际 %q", hash)
	}
	if !superUser {
		t.Fatal("管理员标记应保留")
	}
	if !strings.Contains(permissions, "pris") {
		t.Fatalf("权限 JSON = %s", permissions)
	}

	var settingValue string
	if err := pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key = 'proxy'`).Scan(&settingValue); err != nil {
		t.Fatalf("读取迁移后的设置: %v", err)
	}
	if !strings.Contains(settingValue, "enable") {
		t.Fatalf("设置值 = %s", settingValue)
	}

	var (
		mediaSource string
		mediaID     string
		state       string
		v2State     string
		configText  string
	)
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(media_source,''), COALESCE(media_id,''), internal_state,
		       COALESCE(v2_state,''), config::text
		FROM subscriptions WHERE title = '沙丘'`).
		Scan(&mediaSource, &mediaID, &state, &v2State, &configText); err != nil {
		t.Fatalf("读取迁移后的订阅: %v", err)
	}
	if mediaSource != "tmdb" || mediaID != "438631" {
		t.Fatalf("订阅媒体标识 = (%s, %s)", mediaSource, mediaID)
	}
	if state != "paused" {
		t.Fatalf("订阅默认应导入为 paused，实际 %s", state)
	}
	if v2State != "R" {
		t.Fatalf("应保留 V2 原始状态，实际 %s", v2State)
	}
	if !strings.Contains(configText, "rules") {
		t.Fatalf("V2 原始筛选规则应保留在 config.v2: %s", configText)
	}

	var upload float64
	if err := pool.QueryRow(ctx, `SELECT upload FROM site_userdata WHERE domain = 'haidan.cc'`).Scan(&upload); err != nil {
		t.Fatalf("读取迁移后的站点数据: %v", err)
	}
	if upload != 1.5 {
		t.Fatalf("上传量 = %v", upload)
	}

	// 幂等：再跑一遍不应产生重复行。
	second, err := Run(ctx, Options{SQLitePath: fixture, DB: target, SkipBackup: true})
	if err != nil {
		t.Fatalf("第二次迁移失败: %v", err)
	}
	if second.Written() != 0 {
		t.Fatalf("重复迁移不应再写入，实际 %d 行", second.Written())
	}

	var subscriptionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM subscriptions WHERE title IN ('沙丘', '漫长的季节')`).
		Scan(&subscriptionCount); err != nil {
		t.Fatalf("统计订阅: %v", err)
	}
	if subscriptionCount != 2 {
		t.Fatalf("订阅行数 = %d，期望 2", subscriptionCount)
	}
}
