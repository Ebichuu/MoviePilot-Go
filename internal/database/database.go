package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB 是运行期唯一的 PostgreSQL 连接池。
type DB struct {
	Pool *pgxpool.Pool
}

// Migrate 使用一次性迁移连接更新数据库；正常请求只使用 pgxpool。
func Migrate(ctx context.Context, cfg config.Config) error {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("打开迁移连接: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("迁移前连接数据库: %w", err)
	}
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("设置 goose 方言: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("执行数据库迁移: %w", err)
	}
	return nil
}

// Open 创建带上限、预检和生命周期约束的运行时连接池。
func Open(ctx context.Context, cfg config.Config) (*DB, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("解析数据库地址: %w", err)
	}
	poolConfig.MaxConns = cfg.PostgresMaxConns
	poolConfig.MinConns = 1
	poolConfig.MaxConnIdleTime = 5 * time.Minute
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.HealthCheckPeriod = 30 * time.Second
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-go"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("创建数据库连接池: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close 释放连接池。
func (db *DB) Close() {
	if db != nil && db.Pool != nil {
		db.Pool.Close()
	}
}

// Ping 检查数据库是否能接受请求。
func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.Pool == nil {
		return errors.New("数据库连接池未初始化")
	}
	return db.Pool.Ping(ctx)
}

// Stats 返回连接池统计信息，供 readiness 和 metrics 使用。
func (db *DB) Stats() *pgxpool.Stat {
	if db == nil || db.Pool == nil {
		return nil
	}
	return db.Pool.Stat()
}
