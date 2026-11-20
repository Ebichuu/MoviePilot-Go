package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

type fakeMediaIndexSweep struct{ err error }

func (f fakeMediaIndexSweep) HandleMediaIndexSweep(context.Context) error { return f.err }

type fakeOrganizationSweep struct{ err error }

func (f fakeOrganizationSweep) HandleOrganizationSweep(context.Context) error { return f.err }

type fakeSubscriptionSearch struct{ err error }

func (f fakeSubscriptionSearch) HandleSubscriptionSearch(context.Context, time.Time) error {
	return f.err
}

type fakeDownloadReconcile struct{ err error }

func (f fakeDownloadReconcile) HandleDownloadReconciliation(context.Context, time.Time) error {
	return f.err
}

type fakeSiteUserDataRefresh struct{ err error }

func (f fakeSiteUserDataRefresh) HandleSiteUserDataRefresh(context.Context) error { return f.err }

// 用周期任务构造的合法参数跑通成功分支，避免测试里复制一份参数校验规则。
func TestMediaIndexSweepWorkerRunsHandler(t *testing.T) {
	args, _ := mediaIndexSweepPeriodicArgs()
	worker := &mediaIndexSweepWorker{handler: fakeMediaIndexSweep{}}
	if err := worker.Work(context.Background(), &river.Job[MediaIndexSweepArgs]{Args: args.(MediaIndexSweepArgs)}); err != nil {
		t.Fatalf("媒体入库恢复任务不应报错: %v", err)
	}
	failing := &mediaIndexSweepWorker{handler: fakeMediaIndexSweep{err: errors.New("boom")}}
	err := failing.Work(context.Background(), &river.Job[MediaIndexSweepArgs]{Args: args.(MediaIndexSweepArgs)})
	if !errors.Is(err, ErrMediaIndexSweepExecution) {
		t.Fatalf("处理器失败应包成执行错误，实际 err = %v", err)
	}
}

func TestOrganizationSweepWorkerRunsHandler(t *testing.T) {
	args, _ := organizationSweepPeriodicArgs()
	worker := &organizationSweepWorker{handler: fakeOrganizationSweep{}}
	if err := worker.Work(context.Background(), &river.Job[OrganizationSweepArgs]{Args: args.(OrganizationSweepArgs)}); err != nil {
		t.Fatalf("整理恢复任务不应报错: %v", err)
	}
	failing := &organizationSweepWorker{handler: fakeOrganizationSweep{err: errors.New("boom")}}
	err := failing.Work(context.Background(), &river.Job[OrganizationSweepArgs]{Args: args.(OrganizationSweepArgs)})
	if !errors.Is(err, ErrOrganizationSweepExecution) {
		t.Fatalf("处理器失败应包成执行错误，实际 err = %v", err)
	}
}

func TestSubscriptionSearchWorkerRunsHandler(t *testing.T) {
	args, _ := subscriptionSearchPeriodicArgs()
	worker := &subscriptionSearchWorker{handler: fakeSubscriptionSearch{}}
	if err := worker.Work(context.Background(), &river.Job[SubscriptionSearchSweepArgs]{Args: args.(SubscriptionSearchSweepArgs)}); err != nil {
		t.Fatalf("订阅搜索任务不应报错: %v", err)
	}
	failing := &subscriptionSearchWorker{handler: fakeSubscriptionSearch{err: errors.New("boom")}}
	err := failing.Work(context.Background(), &river.Job[SubscriptionSearchSweepArgs]{Args: args.(SubscriptionSearchSweepArgs)})
	if err == nil {
		t.Fatal("处理器失败时应返回错误以便重试")
	}
}

func TestDownloadReconcileWorkerRunsHandler(t *testing.T) {
	args, _ := downloadReconcilePeriodicArgs()
	worker := &downloadReconcileWorker{handler: fakeDownloadReconcile{}}
	if err := worker.Work(context.Background(), &river.Job[DownloadReconcileSweepArgs]{Args: args.(DownloadReconcileSweepArgs)}); err != nil {
		t.Fatalf("下载对账任务不应报错: %v", err)
	}
}

func TestSiteUserDataRefreshWorkerRunsHandler(t *testing.T) {
	args, _ := siteUserDataRefreshPeriodicArgs()
	worker := &siteUserDataRefreshWorker{handler: fakeSiteUserDataRefresh{}}
	if err := worker.Work(context.Background(), &river.Job[SiteUserDataRefreshArgs]{Args: args.(SiteUserDataRefreshArgs)}); err != nil {
		t.Fatalf("站点用户数据刷新任务不应报错: %v", err)
	}
	failing := &siteUserDataRefreshWorker{handler: fakeSiteUserDataRefresh{err: errors.New("boom")}}
	err := failing.Work(context.Background(), &river.Job[SiteUserDataRefreshArgs]{Args: args.(SiteUserDataRefreshArgs)})
	if err == nil {
		t.Fatal("处理器失败时应返回错误以便重试")
	}
}

// 缺少处理器时必须「不注册」而不是注册一个空转的任务。
func TestMaintenanceSweepsSkipRegistrationWithoutHandler(t *testing.T) {
	cases := map[string]func(*river.Workers) (bool, error){
		"media_index":    func(w *river.Workers) (bool, error) { return registerMediaIndexSweepWorker(w, nil) },
		"organization":   func(w *river.Workers) (bool, error) { return registerOrganizationSweepWorker(w, nil) },
		"subscription":   func(w *river.Workers) (bool, error) { return registerSubscriptionSearchWorker(w, nil) },
		"site_user_data": func(w *river.Workers) (bool, error) { return registerSiteUserDataRefreshWorker(w, nil) },
	}
	for name, register := range cases {
		enabled, err := register(river.NewWorkers())
		if err != nil {
			t.Fatalf("%s 注册失败: %v", name, err)
		}
		if enabled {
			t.Fatalf("%s 在没有处理器时不应注册", name)
		}
	}
}
