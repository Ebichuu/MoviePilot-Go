//go:build integration

package jobs

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 这个集成测试补的是单测覆盖不到的部分：三个手工入队入口真正的 River Insert。
// 单测只能验证「未启用/无客户端」的拦截分支；能不能插进 river_job 表、
// 参数有没有原样落库，只有连真库才能证明。
func TestPluginEnqueueMethodsWithRiver(t *testing.T) {
	pool := newRSSRiverIntegrationPool(t)
	ctx := t.Context()

	manager, err := NewManagerWithConfig(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), ManagerConfig{
		SiteSigninHandler:         fakeSiteSigninHandler{},
		SubscriptionSearchHandler: fakeSubscriptionSearch{},
	})
	if err != nil {
		t.Fatalf("NewManagerWithConfig() error = %v", err)
	}

	// 每个入口插两次：手工触发要的是「点一次就有一轮」，不参与周期任务的唯一约束。
	for range 2 {
		if err := manager.EnqueueSiteSigninSweep(ctx); err != nil {
			t.Fatalf("EnqueueSiteSigninSweep() error = %v", err)
		}
		if err := manager.EnqueueBackupSweep(ctx, "  /tmp/backup  ", 3); err != nil {
			t.Fatalf("EnqueueBackupSweep() error = %v", err)
		}
		if err := manager.EnqueueSubscriptionSearchSweep(ctx); err != nil {
			t.Fatalf("EnqueueSubscriptionSearchSweep() error = %v", err)
		}
	}

	counts := map[string]int{}
	rows, err := pool.Query(ctx, `SELECT kind, count(*) FROM river_job GROUP BY kind`)
	if err != nil {
		t.Fatalf("统计任务行: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatalf("扫描统计: %v", err)
		}
		counts[kind] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历统计: %v", err)
	}
	for _, kind := range []string{siteSigninSweepKind, backupSweepKind, subscriptionSearchKind} {
		if counts[kind] == 0 {
			t.Fatalf("%s 未入队: %v", kind, counts)
		}
	}

	// 备份参数要原样落库：Keep 决定 worker 保留几份，目录要去掉首尾空格。
	var args string
	if err := pool.QueryRow(ctx,
		`SELECT args::text FROM river_job WHERE kind=$1 LIMIT 1`, backupSweepKind).Scan(&args); err != nil {
		t.Fatalf("读取备份任务参数: %v", err)
	}
	if !strings.Contains(args, `"/tmp/backup"`) || !strings.Contains(args, `"keep": 3`) && !strings.Contains(args, `"keep":3`) {
		t.Fatalf("备份任务参数 = %s", args)
	}

	// 热更新：插件改了间隔只换周期任务，不重启 River 客户端，也不会多注册一份。
	if err := manager.SetSiteSigninSweepInterval(3 * time.Hour); err != nil {
		t.Fatalf("SetSiteSigninSweepInterval() error = %v", err)
	}
	if err := manager.SetBackupSweepInterval(6 * time.Hour); err != nil {
		t.Fatalf("SetBackupSweepInterval() error = %v", err)
	}
	// 换过周期后，手工入队仍然可用（周期任务与手工任务互不影响）。
	if err := manager.EnqueueSiteSigninSweep(ctx); err != nil {
		t.Fatalf("换周期后入队 error = %v", err)
	}

	// 队列故障必须报「入队失败」而不是假装成功：关掉连接池后再插一次。
	brokenPool := newRSSRiverIntegrationPool(t)
	brokenManager, err := NewManagerWithConfig(brokenPool, slog.New(slog.NewTextHandler(io.Discard, nil)), ManagerConfig{
		SiteSigninHandler:         fakeSiteSigninHandler{},
		SubscriptionSearchHandler: fakeSubscriptionSearch{},
	})
	if err != nil {
		t.Fatalf("NewManagerWithConfig() error = %v", err)
	}
	brokenPool.Close()

	if err := brokenManager.EnqueueSiteSigninSweep(ctx); !errors.Is(err, ErrSiteSigninSweepExecution) {
		t.Fatalf("签到入队失败 error = %v", err)
	}
	if err := brokenManager.EnqueueBackupSweep(ctx, "", 0); !errors.Is(err, ErrBackupSweepExecution) {
		t.Fatalf("备份入队失败 error = %v", err)
	}
	if err := brokenManager.EnqueueSubscriptionSearchSweep(ctx); !errors.Is(err, ErrSubscriptionSearchExecution) {
		t.Fatalf("订阅搜索入队失败 error = %v", err)
	}
}
