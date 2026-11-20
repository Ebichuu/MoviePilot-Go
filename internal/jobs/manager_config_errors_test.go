package jobs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件覆盖 Manager 构造期与 River 迁移入口的失败返回：
// 受控并发配置非法、处理器缺失，以及数据库不可达时的迁移失败。

func unreachableJobsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(),
		"postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("创建不可达连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestNewManagerWithConfigRejectsInvalidConcurrency(t *testing.T) {
	pool := unreachableJobsPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := duplicateRegistrationHandler{}

	cases := []struct {
		name   string
		config ManagerConfig
	}{
		{"下载并发为负", ManagerConfig{DownloadHandler: handler, DownloadMaxWorkers: -1}},
		{"下载并发缺少处理器", ManagerConfig{DownloadMaxWorkers: 2}},
		{"整理并发为负", ManagerConfig{OrganizationHandler: handler, OrganizationMaxWorkers: -1}},
		{"媒体入库并发为负", ManagerConfig{MediaIndexHandler: handler, MediaIndexMaxWorkers: -1}},
		{"刮削并发为负", ManagerConfig{ScrapeHandler: handler, ScrapeMaxWorkers: -1}},
		{"通知并发为负", ManagerConfig{NotificationHandler: handler, NotificationMaxWorkers: -1}},
		{"RSS 轮询并发为负", ManagerConfig{RSSPollHandler: handler, RSSPollMaxWorkers: -1}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewManagerWithConfig(pool, logger, testCase.config); err == nil {
				t.Fatal("非法并发配置应报错")
			}
		})
	}

	if _, err := NewManagerWithConfig(nil, logger, ManagerConfig{}); err == nil {
		t.Fatal("缺少连接池应报错")
	}
}

func TestMigrateReportsUnreachableDatabase(t *testing.T) {
	pool := unreachableJobsPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("不可达数据库迁移应报错")
	}
}
