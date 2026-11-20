//go:build integration

package database

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestSubscriptionV2StateMigrationPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)

	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("读取隔离 schema: %v", err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("设置 goose PostgreSQL 方言: %v", err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 6); err != nil {
		t.Fatalf("回退到迁移 006: %v", err)
	}

	dataType, nullable := subscriptionV2StateColumnMetadata(t, store)
	if dataType != "character" || nullable != "NO" {
		t.Fatalf("回退后 v2_state 元数据 = (%q, %q), want (character, NO)", dataType, nullable)
	}

	insertSubscriptionIntegrationUsers(t, store, "v2-state-migration-owner")
	var subscriptionID int64
	if err := store.db.Pool.QueryRow(ctx, `
		INSERT INTO subscriptions (username, media_type, title, internal_state, v2_state, completed_at, config)
		VALUES ($1, 'movie', 'V2 state migration fixture', 'completed', '', now(), '{}'::jsonb)
		RETURNING id`, "v2-state-migration-owner").Scan(&subscriptionID); err != nil {
		t.Fatalf("写入 CHAR(1) 空状态 fixture: %v", err)
	}

	if err := goose.UpToContext(ctx, migrationDB, "migrations", 7); err != nil {
		t.Fatalf("重新执行迁移 007: %v", err)
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 7); err != nil {
		t.Fatalf("重复执行 goose 迁移流程: %v", err)
	}

	dataType, nullable = subscriptionV2StateColumnMetadata(t, store)
	if dataType != "text" || nullable != "NO" {
		t.Fatalf("迁移后 v2_state 元数据 = (%q, %q), want (text, NO)", dataType, nullable)
	}

	var emptyState string
	var characterLength int
	var octetLength int
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT v2_state, length(v2_state), octet_length(v2_state)
		FROM subscriptions
		WHERE id=$1`, subscriptionID).Scan(&emptyState, &characterLength, &octetLength); err != nil {
		t.Fatalf("读取迁移后的空状态: %v", err)
	}
	if emptyState != "" || characterLength != 0 || octetLength != 0 {
		t.Fatalf("迁移后空状态 = %q length=%d octets=%d, want strict empty string", emptyState, characterLength, octetLength)
	}

	allowedStates := []struct {
		name  string
		state string
	}{
		{name: "允许空字符串", state: ""},
		{name: "允许 N", state: "N"},
		{name: "允许 R", state: "R"},
		{name: "允许 P", state: "P"},
		{name: "允许 S", state: "S"},
	}
	for _, tt := range allowedStates {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.db.Pool.Exec(ctx, `UPDATE subscriptions SET v2_state=$1 WHERE id=$2`, tt.state, subscriptionID); err != nil {
				t.Fatalf("写入合法状态 %q: %v", tt.state, err)
			}
			var got string
			if err := store.db.Pool.QueryRow(ctx, `SELECT v2_state FROM subscriptions WHERE id=$1`, subscriptionID).Scan(&got); err != nil {
				t.Fatalf("读取合法状态 %q: %v", tt.state, err)
			}
			if got != tt.state {
				t.Fatalf("合法状态往返 = %q, want %q", got, tt.state)
			}
		})
	}

	invalidStates := []struct {
		name  string
		state string
	}{
		{name: "拒绝未知状态", state: "X"},
		{name: "拒绝空格", state: " "},
		{name: "拒绝小写状态", state: "r"},
		{name: "拒绝多字符状态", state: "RR"},
	}
	for _, tt := range invalidStates {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.db.Pool.Exec(ctx, `UPDATE subscriptions SET v2_state=$1 WHERE id=$2`, tt.state, subscriptionID)
			var postgresError *pgconn.PgError
			if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
				t.Fatalf("写入非法状态 %q error = %v, want check violation 23514", tt.state, err)
			}
			if postgresError.ConstraintName != "subscriptions_v2_state_check" {
				t.Fatalf("非法状态命中约束 %q, want subscriptions_v2_state_check", postgresError.ConstraintName)
			}
		})
	}
}

func openSubscriptionV2StateMigrationDB(t *testing.T, schema string) (*sql.DB, string) {
	t.Helper()

	databaseURL := strings.TrimSpace(os.Getenv(siteStatisticsTestDatabaseEnv))
	migrationConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("解析迁移连接: %v", err)
	}
	migrationConfig.RuntimeParams["search_path"] = schema
	migrationConfig.RuntimeParams["application_name"] = "moviepilot-subscription-v2-state-migration-test"
	migrationDSN := stdlib.RegisterConnConfig(migrationConfig)
	migrationDB, err := sql.Open("pgx", migrationDSN)
	if err != nil {
		stdlib.UnregisterConnConfig(migrationDSN)
		t.Fatalf("打开迁移连接: %v", err)
	}
	t.Cleanup(func() {
		if err := migrationDB.Close(); err != nil {
			t.Errorf("关闭迁移连接: %v", err)
		}
		stdlib.UnregisterConnConfig(migrationDSN)
	})
	if err := migrationDB.PingContext(t.Context()); err != nil {
		t.Fatalf("连接迁移 schema %q: %v", schema, err)
	}
	return migrationDB, migrationDSN
}

func subscriptionV2StateColumnMetadata(t *testing.T, store *Store) (dataType, nullable string) {
	t.Helper()
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema=current_schema()
		  AND table_name='subscriptions'
		  AND column_name='v2_state'`).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("读取 subscriptions.v2_state 元数据: %v", err)
	}
	return dataType, nullable
}
