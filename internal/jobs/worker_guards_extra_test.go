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

// 本文件补齐「worker 入口闸门 + 管理器入队闸门」这些防御性分支：
// nil 上下文、已取消上下文、未配置处理器、nil job、非法参数与业务错误脱敏。

func canceledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestScrapeSweepWorkerGuards(t *testing.T) {
	t.Parallel()
	valid := &river.Job[ScrapeSweepArgs]{Args: ScrapeSweepArgs{Key: scrapeSweepKey}}
	worker := &scrapeSweepWorker{handler: scrapeSweepHandlerFunc(func(context.Context) error { return nil })}

	if err := worker.Work(nil, valid); !errors.Is(err, ErrScrapeSweepExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := worker.Work(canceledContext(t), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := (&scrapeSweepWorker{}).Work(context.Background(), valid); !errors.Is(err, ErrScrapeSweepHandlerUnavailable) {
		t.Fatalf("缺少处理器 = %v", err)
	}
	if err := worker.Work(context.Background(), nil); !errors.Is(err, ErrInvalidScrapeSweepArgs) {
		t.Fatalf("nil job = %v", err)
	}
	badArgs := &river.Job[ScrapeSweepArgs]{Args: ScrapeSweepArgs{Key: "other"}}
	if err := worker.Work(context.Background(), badArgs); !errors.Is(err, ErrInvalidScrapeSweepArgs) {
		t.Fatalf("非法参数 = %v", err)
	}
	failing := &scrapeSweepWorker{handler: scrapeSweepHandlerFunc(func(context.Context) error {
		return errors.New("explode")
	})}
	if err := failing.Work(context.Background(), valid); !errors.Is(err, ErrScrapeSweepExecution) {
		t.Fatalf("业务错误 = %v", err)
	}
	canceling := &scrapeSweepWorker{handler: scrapeSweepHandlerFunc(func(context.Context) error {
		return context.Canceled
	})}
	if err := canceling.Work(context.Background(), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("透传取消 = %v", err)
	}
}

func TestNotificationSweepWorkerGuards(t *testing.T) {
	t.Parallel()
	valid := &river.Job[NotificationSweepArgs]{Args: NotificationSweepArgs{Key: notificationSweepKey}}
	worker := &notificationSweepWorker{handler: notificationSweepHandlerFunc(func(context.Context) error { return nil })}

	if err := worker.Work(nil, valid); !errors.Is(err, ErrNotificationSweepExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := worker.Work(canceledContext(t), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := (&notificationSweepWorker{}).Work(context.Background(), valid); !errors.Is(err, ErrNotificationSweepUnavailable) {
		t.Fatalf("缺少处理器 = %v", err)
	}
	if err := worker.Work(context.Background(), nil); !errors.Is(err, ErrInvalidNotificationSweepArgs) {
		t.Fatalf("nil job = %v", err)
	}
	badArgs := &river.Job[NotificationSweepArgs]{Args: NotificationSweepArgs{Key: "other"}}
	if err := worker.Work(context.Background(), badArgs); !errors.Is(err, ErrInvalidNotificationSweepArgs) {
		t.Fatalf("非法参数 = %v", err)
	}
	failing := &notificationSweepWorker{handler: notificationSweepHandlerFunc(func(context.Context) error {
		return errors.New("explode")
	})}
	if err := failing.Work(context.Background(), valid); !errors.Is(err, ErrNotificationSweepExecution) {
		t.Fatalf("业务错误 = %v", err)
	}
	canceling := &notificationSweepWorker{handler: notificationSweepHandlerFunc(func(context.Context) error {
		return context.DeadlineExceeded
	})}
	if err := canceling.Work(context.Background(), valid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("透传超时 = %v", err)
	}
}

func TestSiteUserDataRefreshWorkerGuards(t *testing.T) {
	t.Parallel()
	valid := &river.Job[SiteUserDataRefreshArgs]{Args: SiteUserDataRefreshArgs{Key: siteUserDataRefreshKey}}
	worker := &siteUserDataRefreshWorker{handler: fakeSiteUserDataRefresh{}}

	if err := worker.Work(nil, valid); !errors.Is(err, ErrSiteUserDataRefreshExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := worker.Work(canceledContext(t), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := (&siteUserDataRefreshWorker{}).Work(context.Background(), valid); !errors.Is(err, ErrSiteUserDataRefreshUnavailable) {
		t.Fatalf("缺少处理器 = %v", err)
	}
	if err := worker.Work(context.Background(), nil); !errors.Is(err, ErrInvalidSiteUserDataRefreshArgs) {
		t.Fatalf("nil job = %v", err)
	}
	badArgs := &river.Job[SiteUserDataRefreshArgs]{Args: SiteUserDataRefreshArgs{Key: "other"}}
	if err := worker.Work(context.Background(), badArgs); !errors.Is(err, ErrInvalidSiteUserDataRefreshArgs) {
		t.Fatalf("非法参数 = %v", err)
	}
	if err := (&siteUserDataRefreshWorker{handler: fakeSiteUserDataRefresh{err: errors.New("boom")}}).
		Work(context.Background(), valid); !errors.Is(err, ErrSiteUserDataRefreshExecution) {
		t.Fatalf("业务错误 = %v", err)
	}
	if err := (&siteUserDataRefreshWorker{handler: fakeSiteUserDataRefresh{err: context.Canceled}}).
		Work(context.Background(), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("透传取消 = %v", err)
	}
}

func TestDownloadWorkerGuards(t *testing.T) {
	t.Parallel()
	valid := &river.Job[DownloadArgs]{Args: DownloadArgs{Username: "alice", IntentID: uuid.New()}}
	worker := &downloadWorker{handler: downloadHandlerFunc(func(context.Context, DownloadArgs) error { return nil })}

	if err := worker.Work(nil, valid); !errors.Is(err, ErrDownloadExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := worker.Work(canceledContext(t), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := (&downloadWorker{}).Work(context.Background(), valid); !errors.Is(err, ErrDownloadHandlerUnavailable) {
		t.Fatalf("缺少处理器 = %v", err)
	}
	if err := wrapDownloadExecutionError(nil); !errors.Is(err, ErrDownloadExecution) {
		t.Fatalf("空原因包装 = %v", err)
	}
}

func TestRSSSweepWorkerGuards(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	valid := &river.Job[RSSSweepArgs]{Args: RSSSweepArgs{Key: rssSweepKey, Cutoff: now.Add(-time.Minute)}}
	worker := &rssSweepWorker{
		handler:      fakeRSSSweepHandler{next: RSSSweepState{Initialized: true, SiteIDs: []int{1}}},
		now:          func() time.Time { return now },
		recordOutput: func(context.Context, RSSSweepState) error { return nil },
	}

	if err := worker.Work(nil, valid); !errors.Is(err, ErrRSSSweepExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := worker.Work(canceledContext(t), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := (&rssSweepWorker{now: func() time.Time { return now }}).Work(context.Background(), valid); !errors.Is(err, ErrRSSSweepHandlerUnavailable) {
		t.Fatalf("缺少处理器 = %v", err)
	}
	if err := worker.Work(context.Background(), nil); !errors.Is(err, ErrInvalidRSSSweepArgs) {
		t.Fatalf("nil job = %v", err)
	}
	badArgs := &river.Job[RSSSweepArgs]{Args: RSSSweepArgs{Key: rssSweepKey}}
	if err := worker.Work(context.Background(), badArgs); !errors.Is(err, ErrInvalidRSSSweepArgs) {
		t.Fatalf("非法参数 = %v", err)
	}

	// 状态落盘期间上下文被取消：返回稳定上下文错误而不是通用执行错误。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelingRecord := &rssSweepWorker{
		handler: fakeRSSSweepHandler{next: RSSSweepState{Initialized: true, SiteIDs: []int{1}}},
		now:     func() time.Time { return now },
		recordOutput: func(context.Context, RSSSweepState) error {
			cancel()
			return errors.New("db down")
		},
	}
	if err := cancelingRecord.Work(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("落盘期间取消 = %v", err)
	}

	// 处理器返回上下文错误时直接透传。
	ctxCanceledHandler := &rssSweepWorker{
		handler:      fakeRSSSweepHandler{err: context.Canceled},
		now:          func() time.Time { return now },
		recordOutput: func(context.Context, RSSSweepState) error { return nil },
	}
	if err := ctxCanceledHandler.Work(context.Background(), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("处理器取消 = %v", err)
	}
}

func TestRSSPollWorkerGuards(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	valid := &river.Job[RSSPollArgs]{Args: RSSPollArgs{SiteID: 1, Cutoff: now.Add(-time.Minute)}}
	worker := &rssPollWorker{
		handler: rssPollHandlerFunc(func(context.Context, RSSPollArgs) error { return nil }),
		now:     func() time.Time { return now },
	}

	if err := worker.Work(nil, valid); !errors.Is(err, ErrRSSPollExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := worker.Work(canceledContext(t), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := (&rssPollWorker{now: func() time.Time { return now }}).Work(context.Background(), valid); !errors.Is(err, ErrRSSPollHandlerUnavailable) {
		t.Fatalf("缺少处理器 = %v", err)
	}
	if err := worker.Work(context.Background(), nil); !errors.Is(err, ErrInvalidRSSPollArgs) {
		t.Fatalf("nil job = %v", err)
	}
	badArgs := &river.Job[RSSPollArgs]{Args: RSSPollArgs{Cutoff: now.Add(-time.Minute)}}
	if err := worker.Work(context.Background(), badArgs); !errors.Is(err, ErrInvalidRSSPollArgs) {
		t.Fatalf("非法参数 = %v", err)
	}
}

func TestRSSMatchWorkerGuards(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	valid := &river.Job[RSSMatchArgs]{Args: RSSMatchArgs{Key: rssMatchKey, Cutoff: now.Add(-time.Minute)}}
	worker := &rssMatchWorker{
		handler: rssMatchHandlerFunc(func(context.Context, RSSMatchArgs) error { return nil }),
		now:     func() time.Time { return now },
	}

	if err := worker.Work(nil, valid); !errors.Is(err, ErrRSSMatchExecution) {
		t.Fatalf("nil 上下文 = %v", err)
	}
	if err := worker.Work(canceledContext(t), valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 = %v", err)
	}
	if err := (&rssMatchWorker{now: func() time.Time { return now }}).Work(context.Background(), valid); !errors.Is(err, ErrRSSMatchHandlerUnavailable) {
		t.Fatalf("缺少处理器 = %v", err)
	}
	if err := worker.Work(context.Background(), nil); !errors.Is(err, ErrInvalidRSSMatchArgs) {
		t.Fatalf("nil job = %v", err)
	}
	badArgs := &river.Job[RSSMatchArgs]{Args: RSSMatchArgs{Cutoff: now.Add(-time.Minute)}}
	if err := worker.Work(context.Background(), badArgs); !errors.Is(err, ErrInvalidRSSMatchArgs) {
		t.Fatalf("非法参数 = %v", err)
	}
	canceling := &rssMatchWorker{
		handler: rssMatchHandlerFunc(func(context.Context, RSSMatchArgs) error { return context.DeadlineExceeded }),
		now:     func() time.Time { return now },
	}
	if err := canceling.Work(context.Background(), valid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("透传超时 = %v", err)
	}
}

func TestManagerFullConfigCoversQueueBranches(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Skipf("构造连接池失败: %v", err)
	}
	defer pool.Close()

	manager, err := NewManagerWithConfig(pool, slog.Default(), ManagerConfig{
		DownloadHandler:            downloadHandlerFunc(func(context.Context, DownloadArgs) error { return nil }),
		DownloadMaxWorkers:         2,
		DownloadReconcileHandler:   downloadReconcileHandlerFunc(func(context.Context, time.Time) error { return nil }),
		SubscriptionSearchHandler:  subscriptionSearchHandlerFunc(func(context.Context, time.Time) error { return nil }),
		OrganizationHandler:        organizationHandlerFunc(func(context.Context, OrganizationArgs) error { return nil }),
		OrganizationSweepHandler:   fakeOrganizationSweep{},
		OrganizationMaxWorkers:     2,
		MediaIndexHandler:          mediaIndexHandlerFunc(func(context.Context, MediaIndexArgs) error { return nil }),
		MediaIndexSweepHandler:     fakeMediaIndexSweep{},
		MediaIndexMaxWorkers:       2,
		ScrapeHandler:              scrapeJobHandlerFunc(func(context.Context, ScrapeArgs) error { return nil }),
		ScrapeSweepHandler:         scrapeSweepHandlerFunc(func(context.Context) error { return nil }),
		ScrapeMaxWorkers:           2,
		NotificationHandler:        notificationJobHandlerFunc(func(context.Context, NotificationArgs) error { return nil }),
		NotificationSweepHandler:   notificationSweepHandlerFunc(func(context.Context) error { return nil }),
		NotificationMaxWorkers:     2,
		SiteUserDataRefreshHandler: fakeSiteUserDataRefresh{},
		SiteSigninHandler:          fakeSiteSigninHandler{},
		MediaServerSyncHandler:     mediaServerSyncHandlerFunc(func(context.Context, string) error { return nil }),
		RSSSweepHandler:            fakeRSSSweepHandler{},
		RSSPollHandler:             rssPollHandlerFunc(func(context.Context, RSSPollArgs) error { return nil }),
		RSSPollMaxWorkers:          2,
		RSSMatchHandler:            rssMatchHandlerFunc(func(context.Context, RSSMatchArgs) error { return nil }),
	})
	if err != nil {
		t.Fatalf("构造完整配置的管理器: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)

	// 完整配置下所有队列与能力开关都应打开；借助不可达数据库覆盖入队失败分支。
	if err := manager.EnqueueDownload(ctx, DownloadArgs{Username: "alice", IntentID: uuid.New()}); !errors.Is(err, ErrDownloadEnqueue) {
		t.Fatalf("下载入队失败 = %v", err)
	}
	if err := manager.EnqueueRSSPoll(ctx, RSSPollArgs{SiteID: 1, Cutoff: now}); !errors.Is(err, ErrRSSPollExecution) {
		t.Fatalf("RSS 轮询入队失败 = %v", err)
	}
	if err := manager.EnqueueRSSSweep(ctx, now); !errors.Is(err, ErrRSSSweepExecution) {
		t.Fatalf("RSS 扫描入队失败 = %v", err)
	}
	if err := manager.EnqueueRSSMatch(ctx, now, []int{1, 2}); !errors.Is(err, ErrRSSMatchExecution) {
		t.Fatalf("RSS 匹配入队失败 = %v", err)
	}
	if err := manager.EnqueueOrganizationSweep(ctx); !errors.Is(err, ErrOrganizationSweepExecution) {
		t.Fatalf("整理恢复入队失败 = %v", err)
	}
	if err := manager.EnqueueOrganization(ctx, OrganizationArgs{BatchID: uuid.New()}); !errors.Is(err, ErrOrganizationEnqueue) {
		t.Fatalf("整理入队失败 = %v", err)
	}
}

func TestManagerEnqueueEntryGuards(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Skipf("构造连接池失败: %v", err)
	}
	defer pool.Close()
	manager, err := NewManagerWithConfig(pool, slog.Default(), ManagerConfig{
		DownloadHandler:          downloadHandlerFunc(func(context.Context, DownloadArgs) error { return nil }),
		RSSPollHandler:           rssPollHandlerFunc(func(context.Context, RSSPollArgs) error { return nil }),
		RSSSweepHandler:          fakeRSSSweepHandler{},
		RSSMatchHandler:          rssMatchHandlerFunc(func(context.Context, RSSMatchArgs) error { return nil }),
		OrganizationSweepHandler: fakeOrganizationSweep{},
	})
	if err != nil {
		t.Fatalf("构造管理器: %v", err)
	}
	downloadArgs := DownloadArgs{Username: "alice", IntentID: uuid.New()}
	now := time.Now().UTC().Add(-time.Minute)

	if err := manager.EnqueueDownload(nil, downloadArgs); !errors.Is(err, ErrDownloadEnqueue) {
		t.Fatalf("下载 nil 上下文 = %v", err)
	}
	if err := manager.EnqueueDownload(canceledContext(t), downloadArgs); !errors.Is(err, context.Canceled) {
		t.Fatalf("下载取消上下文 = %v", err)
	}
	if err := manager.EnqueueDownloadTx(context.Background(), nil, downloadArgs); !errors.Is(err, ErrDownloadEnqueue) {
		t.Fatalf("下载事务 nil = %v", err)
	}
	if err := manager.EnqueueDownloadTx(nil, nil, downloadArgs); !errors.Is(err, ErrDownloadEnqueue) {
		t.Fatalf("下载事务 nil 上下文 = %v", err)
	}
	if err := manager.EnqueueRSSPoll(nil, RSSPollArgs{SiteID: 1, Cutoff: now}); !errors.Is(err, ErrRSSPollExecution) {
		t.Fatalf("轮询 nil 上下文 = %v", err)
	}
	if err := manager.EnqueueRSSPoll(context.Background(), RSSPollArgs{Cutoff: now}); !errors.Is(err, ErrInvalidRSSPollArgs) {
		t.Fatalf("轮询非法参数 = %v", err)
	}
	if err := manager.EnqueueRSSSweep(nil, now); !errors.Is(err, ErrRSSSweepExecution) {
		t.Fatalf("扫描 nil 上下文 = %v", err)
	}
	if err := manager.EnqueueRSSSweep(context.Background(), time.Time{}); !errors.Is(err, ErrInvalidRSSSweepArgs) {
		t.Fatalf("扫描非法参数 = %v", err)
	}
	if err := manager.EnqueueRSSMatch(nil, now, nil); !errors.Is(err, ErrRSSMatchExecution) {
		t.Fatalf("匹配 nil 上下文 = %v", err)
	}
	if err := manager.EnqueueRSSMatch(context.Background(), now, []int{2, 1}); !errors.Is(err, ErrInvalidRSSMatchArgs) {
		t.Fatalf("匹配非法参数 = %v", err)
	}
	if err := manager.EnqueueOrganizationSweep(nil); !errors.Is(err, ErrOrganizationSweepExecution) {
		t.Fatalf("恢复 nil 上下文 = %v", err)
	}
	if err := manager.EnqueueOrganizationSweep(canceledContext(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("恢复取消上下文 = %v", err)
	}
	if err := manager.EnqueueOrganization(nil, OrganizationArgs{BatchID: uuid.New()}); !errors.Is(err, ErrOrganizationEnqueue) {
		t.Fatalf("整理 nil 上下文 = %v", err)
	}
	if err := manager.EnqueueOrganization(canceledContext(t), OrganizationArgs{BatchID: uuid.New()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("整理取消上下文 = %v", err)
	}
	if err := manager.EnqueueOrganization(context.Background(), OrganizationArgs{}); err == nil {
		t.Fatal("整理非法参数应报错")
	}

	// 能力开关关闭时统一返回「处理器未配置」。
	disabled := &Manager{}
	if err := disabled.EnqueueRSSPoll(context.Background(), RSSPollArgs{SiteID: 1, Cutoff: now}); !errors.Is(err, ErrRSSPollHandlerUnavailable) {
		t.Fatalf("轮询未启用 = %v", err)
	}
	if err := disabled.EnqueueRSSSweep(context.Background(), now); !errors.Is(err, ErrRSSSweepHandlerUnavailable) {
		t.Fatalf("扫描未启用 = %v", err)
	}
	if err := disabled.EnqueueRSSMatch(context.Background(), now, nil); !errors.Is(err, ErrRSSMatchHandlerUnavailable) {
		t.Fatalf("匹配未启用 = %v", err)
	}
	if err := disabled.EnqueueOrganizationSweep(context.Background()); !errors.Is(err, ErrOrganizationSweepHandlerUnavailable) {
		t.Fatalf("恢复未启用 = %v", err)
	}
	if err := disabled.EnqueueDownload(context.Background(), downloadArgs); !errors.Is(err, ErrDownloadHandlerUnavailable) {
		t.Fatalf("下载未启用 = %v", err)
	}
	if err := disabled.EnqueueDownloadTx(context.Background(), nil, downloadArgs); !errors.Is(err, ErrDownloadEnqueue) {
		t.Fatalf("下载事务未启用 = %v", err)
	}
}
