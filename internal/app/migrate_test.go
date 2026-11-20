package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

type stubRiverMigrationConnection struct {
	migrate func(context.Context) error
	close   func()
}

func (c stubRiverMigrationConnection) Migrate(ctx context.Context) error {
	return c.migrate(ctx)
}

func (c stubRiverMigrationConnection) Close() {
	c.close()
}

func TestMigrate(t *testing.T) {
	tests := []struct {
		name       string
		failStage  string
		wantStages []string
		wantError  string
		wantClosed bool
	}{
		{
			name:       "全部迁移按顺序完成",
			wantStages: []string{"goose", "open_pool", "river"},
			wantClosed: true,
		},
		{
			name:       "Goose 失败后停止",
			failStage:  "goose",
			wantStages: []string{"goose"},
			wantError:  "执行 Goose 迁移",
		},
		{
			name:       "连接池失败后停止",
			failStage:  "open_pool",
			wantStages: []string{"goose", "open_pool"},
			wantError:  "打开 River 迁移连接池",
		},
		{
			name:       "River 失败时关闭连接池",
			failStage:  "river",
			wantStages: []string{"goose", "open_pool", "river"},
			wantError:  "执行 River 全量迁移",
			wantClosed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stageErr := errors.New("stage failed")
			var stages []string
			closed := false
			steps := migrationSteps{
				goose: func(ctx context.Context, cfg config.Config) error {
					stages = append(stages, "goose")
					if tt.failStage == "goose" {
						return stageErr
					}
					return nil
				},
				openRiverPool: func(ctx context.Context, cfg config.Config) (riverMigrationConnection, error) {
					stages = append(stages, "open_pool")
					if cfg.PostgresMaxConns != migrationMaxPoolConns {
						t.Fatalf("迁移连接池上限 = %d, want %d", cfg.PostgresMaxConns, migrationMaxPoolConns)
					}
					if tt.failStage == "open_pool" {
						return nil, stageErr
					}
					return stubRiverMigrationConnection{
						migrate: func(ctx context.Context) error {
							stages = append(stages, "river")
							if tt.failStage == "river" {
								return stageErr
							}
							return nil
						},
						close: func() { closed = true },
					}, nil
				},
			}

			err := migrate(t.Context(), config.Config{PostgresMaxConns: 24}, steps)
			if !reflect.DeepEqual(stages, tt.wantStages) {
				t.Fatalf("执行阶段 = %v, want %v", stages, tt.wantStages)
			}
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("Migrate() error = %v", err)
				}
			} else {
				if !errors.Is(err, stageErr) {
					t.Fatalf("Migrate() error = %v, want wrapped stage error", err)
				}
				if !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Migrate() error = %q, want stage %q", err, tt.wantError)
				}
			}
			if closed != tt.wantClosed {
				t.Fatalf("连接池关闭状态 = %t, want %t", closed, tt.wantClosed)
			}
		})
	}
}

func TestMigrateRespectsTotalBudgetAndParentCancellation(t *testing.T) {
	t.Run("无父截止时间时总预算不超过六十秒", func(t *testing.T) {
		const expectedMigrationTimeout = 60 * time.Second
		if migrationTimeout != expectedMigrationTimeout {
			t.Fatalf("migrationTimeout = %s, want %s", migrationTimeout, expectedMigrationTimeout)
		}
		startedBefore := time.Now()
		steps := successfulMigrationSteps(t, func(ctx context.Context) {
			observedAt := time.Now()
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("迁移上下文没有截止时间")
			}
			minimumDeadline := startedBefore.Add(expectedMigrationTimeout)
			maximumDeadline := observedAt.Add(expectedMigrationTimeout)
			if deadline.Before(minimumDeadline) || deadline.After(maximumDeadline) {
				t.Fatalf("迁移截止时间 = %v, want [%v, %v]", deadline, minimumDeadline, maximumDeadline)
			}
		})
		if err := migrate(t.Context(), config.Config{PostgresMaxConns: 24}, steps); err != nil {
			t.Fatalf("Migrate() error = %v", err)
		}
	})

	t.Run("父截止时间贯穿全部阶段", func(t *testing.T) {
		parentCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		parentDeadline, ok := parentCtx.Deadline()
		if !ok {
			t.Fatal("父上下文没有截止时间")
		}
		assertDeadline := func(stage string, ctx context.Context) {
			t.Helper()
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(parentDeadline) {
				t.Fatalf("%s 截止时间 = %v, %t; want %v", stage, deadline, ok, parentDeadline)
			}
		}
		steps := migrationSteps{
			goose: func(ctx context.Context, cfg config.Config) error {
				assertDeadline("Goose", ctx)
				return nil
			},
			openRiverPool: func(ctx context.Context, cfg config.Config) (riverMigrationConnection, error) {
				assertDeadline("打开 River 连接池", ctx)
				return stubRiverMigrationConnection{
					migrate: func(ctx context.Context) error {
						assertDeadline("River", ctx)
						return nil
					},
					close: func() {},
				}, nil
			},
		}
		if err := migrate(parentCtx, config.Config{PostgresMaxConns: 24}, steps); err != nil {
			t.Fatalf("Migrate() error = %v", err)
		}
	})

	t.Run("父上下文取消传入 Goose 并停止后续阶段", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var stages []string
		steps := migrationSteps{
			goose: func(ctx context.Context, cfg config.Config) error {
				stages = append(stages, "goose")
				return ctx.Err()
			},
			openRiverPool: func(ctx context.Context, cfg config.Config) (riverMigrationConnection, error) {
				stages = append(stages, "open_pool")
				return stubRiverMigrationConnection{
					migrate: func(ctx context.Context) error {
						stages = append(stages, "river")
						return nil
					},
					close: func() {},
				}, nil
			},
		}
		err := migrate(ctx, config.Config{PostgresMaxConns: 24}, steps)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Migrate() error = %v, want context.Canceled", err)
		}
		if !reflect.DeepEqual(stages, []string{"goose"}) {
			t.Fatalf("取消后的执行阶段 = %v, want [goose]", stages)
		}
	})
}

func TestBoundedMigrationPoolSize(t *testing.T) {
	tests := []struct {
		name       string
		configured int32
		want       int32
	}{
		{name: "无效下限防御", configured: 0, want: 1},
		{name: "保留较小上限", configured: 2, want: 2},
		{name: "限制较大上限", configured: 24, want: migrationMaxPoolConns},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boundedMigrationPoolSize(tt.configured); got != tt.want {
				t.Fatalf("boundedMigrationPoolSize(%d) = %d, want %d", tt.configured, got, tt.want)
			}
		})
	}
}

func successfulMigrationSteps(t *testing.T, inspectContext func(context.Context)) migrationSteps {
	t.Helper()
	return migrationSteps{
		goose: func(ctx context.Context, cfg config.Config) error {
			inspectContext(ctx)
			return nil
		},
		openRiverPool: func(ctx context.Context, cfg config.Config) (riverMigrationConnection, error) {
			return stubRiverMigrationConnection{
				migrate: func(ctx context.Context) error { return nil },
				close:   func() {},
			}, nil
		},
	}
}
