package jobs

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

func TestRegisterNotificationWorkerBranches(t *testing.T) {
	if maxWorkers, err := registerNotificationWorker(river.NewWorkers(), ManagerConfig{}); err != nil || maxWorkers != 0 {
		t.Fatalf("无处理器时 = (%d, %v)", maxWorkers, err)
	}
	missingHandler := ManagerConfig{NotificationMaxWorkers: 3}
	if _, err := registerNotificationWorker(river.NewWorkers(), missingHandler); err == nil {
		t.Fatal("有并发配置但缺处理器应报错")
	}
	handler := notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return nil })
	if _, err := registerNotificationWorker(river.NewWorkers(), ManagerConfig{
		NotificationHandler: handler, NotificationMaxWorkers: -1,
	}); err == nil {
		t.Fatal("负并发应报错")
	}
	if maxWorkers, err := registerNotificationWorker(river.NewWorkers(), ManagerConfig{NotificationHandler: handler}); err != nil || maxWorkers != notificationDefaultMaxWorkers {
		t.Fatalf("默认并发 = (%d, %v)", maxWorkers, err)
	}
	if maxWorkers, err := registerNotificationWorker(river.NewWorkers(), ManagerConfig{
		NotificationHandler: handler, NotificationMaxWorkers: 1000,
	}); err != nil || maxWorkers != notificationMaxWorkersLimit {
		t.Fatalf("并发上限 = (%d, %v)", maxWorkers, err)
	}
	workers := river.NewWorkers()
	if _, err := registerNotificationWorker(workers, ManagerConfig{NotificationHandler: handler}); err != nil {
		t.Fatalf("首次注册 = %v", err)
	}
	if _, err := registerNotificationWorker(workers, ManagerConfig{NotificationHandler: handler}); err == nil {
		t.Fatal("重复注册应报错")
	}
}

func TestRegisterScrapeWorkerBranches(t *testing.T) {
	if maxWorkers, err := registerScrapeWorker(river.NewWorkers(), ManagerConfig{}); err != nil || maxWorkers != 0 {
		t.Fatalf("无处理器时 = (%d, %v)", maxWorkers, err)
	}
	if _, err := registerScrapeWorker(river.NewWorkers(), ManagerConfig{ScrapeMaxWorkers: 2}); err == nil {
		t.Fatal("有并发配置但缺处理器应报错")
	}
	handler := scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return nil })
	if _, err := registerScrapeWorker(river.NewWorkers(), ManagerConfig{
		ScrapeHandler: handler, ScrapeMaxWorkers: -2,
	}); err == nil {
		t.Fatal("负并发应报错")
	}
	if maxWorkers, err := registerScrapeWorker(river.NewWorkers(), ManagerConfig{ScrapeHandler: handler}); err != nil || maxWorkers != scrapeDefaultMaxWorkers {
		t.Fatalf("默认并发 = (%d, %v)", maxWorkers, err)
	}
	if maxWorkers, err := registerScrapeWorker(river.NewWorkers(), ManagerConfig{
		ScrapeHandler: handler, ScrapeMaxWorkers: 999,
	}); err != nil || maxWorkers != scrapeMaxWorkersLimit {
		t.Fatalf("并发上限 = (%d, %v)", maxWorkers, err)
	}
	workers := river.NewWorkers()
	if _, err := registerScrapeWorker(workers, ManagerConfig{ScrapeHandler: handler}); err != nil {
		t.Fatalf("首次注册 = %v", err)
	}
	if _, err := registerScrapeWorker(workers, ManagerConfig{ScrapeHandler: handler}); err == nil {
		t.Fatal("重复注册应报错")
	}
}

func TestRegisterSweepWorkersBranches(t *testing.T) {
	notificationSweep := notificationSweepHandlerFunc(func(context.Context) error { return nil })
	scrapeSweep := scrapeSweepHandlerFunc(func(context.Context) error { return nil })
	userData := fakeSiteUserDataRefresh{}

	cases := []struct {
		name     string
		register func(*river.Workers) (bool, error)
	}{
		{"通知恢复", func(w *river.Workers) (bool, error) {
			return registerNotificationSweepWorker(w, notificationSweep)
		}},
		{"NFO 恢复", func(w *river.Workers) (bool, error) {
			return registerScrapeSweepWorker(w, scrapeSweep)
		}},
		{"站点用户数据", func(w *river.Workers) (bool, error) {
			return registerSiteUserDataRefreshWorker(w, userData)
		}},
	}
	// 未配置处理器时不注册、也不报错。
	nilWorkers := river.NewWorkers()
	if enabled, err := registerNotificationSweepWorker(nilWorkers, nil); enabled || err != nil {
		t.Fatalf("通知恢复 nil handler = (%v, %v)", enabled, err)
	}
	if enabled, err := registerScrapeSweepWorker(nilWorkers, nil); enabled || err != nil {
		t.Fatalf("NFO 恢复 nil handler = (%v, %v)", enabled, err)
	}
	if enabled, err := registerSiteUserDataRefreshWorker(nilWorkers, nil); enabled || err != nil {
		t.Fatalf("站点用户数据 nil handler = (%v, %v)", enabled, err)
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			workers := river.NewWorkers()
			if enabled, err := testCase.register(workers); !enabled || err != nil {
				t.Fatalf("首次注册 = (%v, %v)", enabled, err)
			}
			if enabled, err := testCase.register(workers); enabled || err == nil {
				t.Fatalf("重复注册 = (%v, %v)", enabled, err)
			}
		})
	}
}

func TestManagerEnqueueMoreGuards(t *testing.T) {
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	requestID := uuid.New()
	disabled := &Manager{}

	t.Run("通知任务", func(t *testing.T) {
		if err := disabled.EnqueueNotification(ctx, uuid.Nil); !errors.Is(err, ErrInvalidNotificationArgs) {
			t.Fatalf("零值参数 error = %v", err)
		}
		if err := disabled.EnqueueNotification(nil, requestID); !errors.Is(err, ErrNotificationEnqueue) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueNotification(canceled, requestID); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueNotification(ctx, requestID); !errors.Is(err, ErrNotificationHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
	})

	t.Run("NFO 任务", func(t *testing.T) {
		if err := disabled.EnqueueScrape(ctx, uuid.Nil); !errors.Is(err, ErrInvalidScrapeArgs) {
			t.Fatalf("零值参数 error = %v", err)
		}
		if err := disabled.EnqueueScrape(nil, requestID); !errors.Is(err, ErrScrapeEnqueue) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueScrape(canceled, requestID); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueScrape(ctx, requestID); !errors.Is(err, ErrScrapeHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
	})

	t.Run("媒体入库确认", func(t *testing.T) {
		if err := disabled.EnqueueMediaIndex(ctx, MediaIndexArgs{}); !errors.Is(err, ErrInvalidMediaIndexArgs) {
			t.Fatalf("零值参数 error = %v", err)
		}
		if err := disabled.EnqueueMediaIndex(nil, MediaIndexArgs{RequestID: requestID}); !errors.Is(err, ErrMediaIndexEnqueue) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueMediaIndex(canceled, MediaIndexArgs{RequestID: requestID}); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueMediaIndex(ctx, MediaIndexArgs{RequestID: requestID}); !errors.Is(err, ErrMediaIndexHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
		if err := disabled.EnqueueMediaIndexSweep(nil); !errors.Is(err, ErrMediaIndexSweepExecution) {
			t.Fatalf("恢复任务 nil ctx error = %v", err)
		}
		if err := disabled.EnqueueMediaIndexSweep(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("恢复任务取消 error = %v", err)
		}
		if err := disabled.EnqueueMediaIndexSweep(ctx); !errors.Is(err, ErrMediaIndexSweepHandlerUnavailable) {
			t.Fatalf("恢复任务未启用 error = %v", err)
		}
	})

	t.Run("整理任务", func(t *testing.T) {
		if err := disabled.EnqueueOrganization(ctx, OrganizationArgs{}); !errors.Is(err, ErrInvalidOrganizationArgs) {
			t.Fatalf("零值参数 error = %v", err)
		}
		if err := disabled.EnqueueOrganization(nil, OrganizationArgs{BatchID: requestID}); !errors.Is(err, ErrOrganizationEnqueue) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueOrganization(canceled, OrganizationArgs{BatchID: requestID}); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueOrganization(ctx, OrganizationArgs{BatchID: requestID}); !errors.Is(err, ErrOrganizationHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
	})

	sweeps := []struct {
		name    string
		enqueue func(context.Context) error
		exec    error
		unavail error
	}{
		{"通知恢复", disabled.EnqueueNotificationSweep, ErrNotificationSweepExecution, ErrNotificationSweepUnavailable},
		{"NFO 恢复", disabled.EnqueueScrapeSweep, ErrScrapeSweepExecution, ErrScrapeSweepHandlerUnavailable},
		{"站点用户数据", disabled.EnqueueSiteUserDataRefresh, ErrSiteUserDataRefreshExecution, ErrSiteUserDataRefreshUnavailable},
		{"整理恢复", disabled.EnqueueOrganizationSweep, ErrOrganizationSweepExecution, ErrOrganizationSweepHandlerUnavailable},
	}
	for _, sweep := range sweeps {
		t.Run(sweep.name, func(t *testing.T) {
			if err := sweep.enqueue(nil); !errors.Is(err, sweep.exec) {
				t.Fatalf("nil ctx error = %v，期望 %v", err, sweep.exec)
			}
			if err := sweep.enqueue(canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("取消上下文 error = %v", err)
			}
			if err := sweep.enqueue(ctx); !errors.Is(err, sweep.unavail) {
				t.Fatalf("未启用 error = %v，期望 %v", err, sweep.unavail)
			}
		})
	}

	t.Run("媒体服务器同步", func(t *testing.T) {
		if err := disabled.EnqueueMediaServerSync(nil, ""); !errors.Is(err, ErrMediaServerSyncExecution) {
			t.Fatalf("nil ctx error = %v", err)
		}
		if err := disabled.EnqueueMediaServerSync(canceled, ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消上下文 error = %v", err)
		}
		if err := disabled.EnqueueMediaServerSync(ctx, ""); !errors.Is(err, ErrMediaServerSyncHandlerUnavailable) {
			t.Fatalf("未启用 error = %v", err)
		}
		if err := disabled.EnqueueMediaServerSyncFirstRun(nil, ""); !errors.Is(err, ErrMediaServerSyncExecution) {
			t.Fatalf("首轮同步 nil ctx error = %v", err)
		}
		if err := disabled.EnqueueMediaServerSyncFirstRun(ctx, ""); !errors.Is(err, ErrMediaServerSyncHandlerUnavailable) {
			t.Fatalf("首轮同步未启用 error = %v", err)
		}
		if MediaServerSyncFirstRunDelay <= 0 {
			t.Fatalf("首轮同步延迟 = %v", time.Duration(MediaServerSyncFirstRunDelay))
		}
	})
}

// TestManagerEnqueueReportsInsertFailure 用未启动的 River 客户端驱动
// 「Insert 失败 → 稳定错误」分支，无需真实数据库即可覆盖。
func TestManagerEnqueueReportsInsertFailure(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Skipf("构造连接池失败: %v", err)
	}
	defer pool.Close()
	manager, err := NewManagerWithConfig(pool, slog.Default(), ManagerConfig{
		NotificationHandler:        notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return nil }),
		NotificationSweepHandler:   notificationSweepHandlerFunc(func(context.Context) error { return nil }),
		ScrapeHandler:              scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return nil }),
		ScrapeSweepHandler:         scrapeSweepHandlerFunc(func(context.Context) error { return nil }),
		MediaIndexHandler:          mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return nil }),
		MediaIndexSweepHandler:     fakeMediaIndexSweep{},
		OrganizationHandler:        organizationHandlerFunc(func(context.Context, OrganizationArgs) error { return nil }),
		OrganizationSweepHandler:   fakeOrganizationSweep{},
		MediaServerSyncHandler:     mediaServerSyncHandlerFunc(func(context.Context, string) error { return nil }),
		SiteUserDataRefreshHandler: fakeSiteUserDataRefresh{},
	})
	if err != nil {
		t.Fatalf("构造管理器失败: %v", err)
	}
	ctx := context.Background()
	requestID := uuid.New()

	if err := manager.EnqueueNotification(ctx, requestID); !errors.Is(err, ErrNotificationEnqueue) {
		t.Fatalf("通知 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueScrape(ctx, requestID); !errors.Is(err, ErrScrapeEnqueue) {
		t.Fatalf("NFO Insert 失败 = %v", err)
	}
	if err := manager.EnqueueMediaIndex(ctx, MediaIndexArgs{RequestID: requestID}); !errors.Is(err, ErrMediaIndexEnqueue) {
		t.Fatalf("媒体入库 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueOrganization(ctx, OrganizationArgs{BatchID: requestID}); !errors.Is(err, ErrOrganizationEnqueue) {
		t.Fatalf("整理 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueNotificationSweep(ctx); !errors.Is(err, ErrNotificationSweepExecution) {
		t.Fatalf("通知恢复 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueScrapeSweep(ctx); !errors.Is(err, ErrScrapeSweepExecution) {
		t.Fatalf("NFO 恢复 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueMediaIndexSweep(ctx); !errors.Is(err, ErrMediaIndexSweepExecution) {
		t.Fatalf("媒体入库恢复 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueOrganizationSweep(ctx); !errors.Is(err, ErrOrganizationSweepExecution) {
		t.Fatalf("整理恢复 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueSiteUserDataRefresh(ctx); !errors.Is(err, ErrSiteUserDataRefreshExecution) {
		t.Fatalf("站点用户数据 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueMediaServerSync(ctx, ""); !errors.Is(err, ErrMediaServerSyncExecution) {
		t.Fatalf("媒体服务器同步 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueMediaServerSyncFirstRun(ctx, ""); !errors.Is(err, ErrMediaServerSyncExecution) {
		t.Fatalf("媒体服务器首轮同步 Insert 失败 = %v", err)
	}
	if err := manager.EnqueueProbe(ctx); err == nil {
		t.Fatal("探针 Insert 失败应返回错误")
	}
}

// TestRegisterSiteSigninSweepWorkerBranches 覆盖签到 worker 的启用判定。
func TestRegisterSiteSigninSweepWorkerBranches(t *testing.T) {
	if enabled, err := registerSiteSigninSweepWorker(river.NewWorkers(), nil, nil); enabled || err != nil {
		t.Fatalf("nil handler = (%v, %v)", enabled, err)
	}
	enabled, err := registerSiteSigninSweepWorker(river.NewWorkers(), fakeSiteSigninHandler{}, nil)
	if !enabled || err != nil {
		t.Fatalf("注册签到 worker = (%v, %v)", enabled, err)
	}
}
