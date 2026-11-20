//go:build integration

package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

const (
	migrateTestDatabaseEnv = "MOVIEPILOT_TEST_DATABASE_URL"
	latestGooseVersion     = 30
	latestRiverVersion     = 6
)

func TestMigratePostgresAppliesLatestGooseAndRiverSchemas(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv(migrateTestDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("未设置 %s，跳过 PostgreSQL 集成测试", migrateTestDatabaseEnv)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析 %s: %v", migrateTestDatabaseEnv, err)
	}
	adminConfig.MaxConns = 2
	adminConfig.MinConns = 0
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("创建 PostgreSQL 管理连接池: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if err := adminPool.Ping(ctx); err != nil {
		t.Fatalf("连接 PostgreSQL 集成测试数据库: %v", err)
	}

	schema := "moviepilot_app_migrate_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("创建隔离 schema %q: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("清理隔离 schema %q: %v", schema, err)
		}
	})

	scopedURL := databaseURLWithSearchPath(t, databaseURL, schema)
	cfg := config.Config{DatabaseURL: scopedURL, PostgresMaxConns: 24}
	if err := Migrate(ctx, cfg); err != nil {
		t.Fatalf("首次迁移隔离 schema %q: %v", schema, err)
	}
	if err := Migrate(ctx, cfg); err != nil {
		t.Fatalf("幂等重跑隔离 schema %q: %v", schema, err)
	}

	verifyConfig, err := pgxpool.ParseConfig(scopedURL)
	if err != nil {
		t.Fatalf("解析校验连接池: %v", err)
	}
	verifyConfig.MaxConns = 2
	verifyConfig.MinConns = 0
	verifyPool, err := pgxpool.NewWithConfig(ctx, verifyConfig)
	if err != nil {
		t.Fatalf("创建校验连接池: %v", err)
	}
	t.Cleanup(verifyPool.Close)

	gooseRows, err := verifyPool.Query(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (version_id) version_id, is_applied
			FROM goose_db_version
			WHERE version_id BETWEEN 1 AND $1
			ORDER BY version_id, id DESC
		)
		SELECT version_id
		FROM latest
		WHERE is_applied
		ORDER BY version_id`, latestGooseVersion)
	if err != nil {
		t.Fatalf("读取 Goose 已应用版本: %v", err)
	}
	gooseVersions, err := pgx.CollectRows(gooseRows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("扫描 Goose 已应用版本: %v", err)
	}
	if err := requireContiguousVersions(gooseVersions, latestGooseVersion); err != nil {
		t.Fatalf("Goose 已应用版本无效: %v", err)
	}

	riverMigrator, err := rivermigrate.New(riverpgxv5.New(verifyPool), nil)
	if err != nil {
		t.Fatalf("创建 River 校验迁移器: %v", err)
	}
	existing, err := riverMigrator.ExistingVersions(ctx)
	if err != nil {
		t.Fatalf("读取 River main 已应用版本: %v", err)
	}
	riverVersions := migrationVersions(existing)
	if err := requireContiguousVersions(riverVersions, latestRiverVersion); err != nil {
		t.Fatalf("River main 已应用版本无效: %v", err)
	}
}

func databaseURLWithSearchPath(t *testing.T, databaseURL, schema string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("解析 PostgreSQL URL: %v", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		t.Fatalf("%s 必须使用 postgres URL，当前 scheme = %q", migrateTestDatabaseEnv, parsed.Scheme)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	query.Set("application_name", "moviepilot-app-migrate-integration")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func migrationVersions(migrations []rivermigrate.Migration) []int64 {
	versions := make([]int64, len(migrations))
	for i, migration := range migrations {
		versions[i] = int64(migration.Version)
	}
	return versions
}

func requireContiguousVersions(versions []int64, latest int) error {
	if len(versions) != latest {
		return fmt.Errorf("版本集合 = %v, want 1..%d", versions, latest)
	}
	for i, version := range versions {
		want := int64(i + 1)
		if version != want {
			return fmt.Errorf("版本集合 = %v, want 1..%d", versions, latest)
		}
	}
	return nil
}
