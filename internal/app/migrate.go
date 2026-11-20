package app

import (
	"context"
	"fmt"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

const (
	migrationTimeout      = 60 * time.Second
	migrationMaxPoolConns = 4
)

type migrationSteps struct {
	goose         func(context.Context, config.Config) error
	openRiverPool func(context.Context, config.Config) (riverMigrationConnection, error)
}

var productionMigrationSteps = migrationSteps{
	goose: database.Migrate,
	openRiverPool: func(ctx context.Context, cfg config.Config) (riverMigrationConnection, error) {
		db, err := database.Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return &productionRiverMigrationConnection{db: db}, nil
	},
}

type riverMigrationConnection interface {
	Migrate(context.Context) error
	Close()
}

type productionRiverMigrationConnection struct {
	db *database.DB
}

func (c *productionRiverMigrationConnection) Migrate(ctx context.Context) error {
	return jobs.Migrate(ctx, c.db.Pool)
}

func (c *productionRiverMigrationConnection) Close() {
	c.db.Close()
}

// Migrate 在一个总预算内依次应用 MoviePilot Goose 与 River 全量迁移。
func Migrate(ctx context.Context, cfg config.Config) error {
	return migrate(ctx, cfg, productionMigrationSteps)
}

func migrate(ctx context.Context, cfg config.Config, steps migrationSteps) error {
	migrationCtx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()

	if err := steps.goose(migrationCtx, cfg); err != nil {
		return fmt.Errorf("执行 Goose 迁移: %w", err)
	}

	poolConfig := cfg
	poolConfig.PostgresMaxConns = boundedMigrationPoolSize(cfg.PostgresMaxConns)
	connection, err := steps.openRiverPool(migrationCtx, poolConfig)
	if err != nil {
		return fmt.Errorf("打开 River 迁移连接池: %w", err)
	}
	defer connection.Close()

	if err := connection.Migrate(migrationCtx); err != nil {
		return fmt.Errorf("执行 River 全量迁移: %w", err)
	}
	return nil
}

func boundedMigrationPoolSize(configured int32) int32 {
	if configured < 1 {
		return 1
	}
	if configured > migrationMaxPoolConns {
		return migrationMaxPoolConns
	}
	return configured
}
