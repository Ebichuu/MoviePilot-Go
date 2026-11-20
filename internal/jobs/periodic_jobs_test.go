package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSiteUserDataRefreshPeriodicWiring 覆盖站点用户数据刷新周期任务的构造与入队守卫。
func TestSiteUserDataRefreshPeriodicWiring(t *testing.T) {
	if job := siteUserDataRefreshPeriodicJob(); job == nil {
		t.Fatal("siteUserDataRefreshPeriodicJob() = nil")
	}
	args, opts := siteUserDataRefreshPeriodicArgs()
	typed, ok := args.(SiteUserDataRefreshArgs)
	if !ok {
		t.Fatalf("周期任务参数类型 = %T", args)
	}
	if typed.Key != siteUserDataRefreshKey || opts == nil {
		t.Fatalf("周期任务参数 = (%#v, %v)", typed, opts)
	}

	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	disabled := &Manager{}
	if err := disabled.EnqueueSiteUserDataRefresh(nil); !errors.Is(err, ErrSiteUserDataRefreshExecution) {
		t.Fatalf("nil ctx error = %v", err)
	}
	if err := disabled.EnqueueSiteUserDataRefresh(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文 error = %v", err)
	}
	if err := disabled.EnqueueSiteUserDataRefresh(ctx); !errors.Is(err, ErrSiteUserDataRefreshUnavailable) {
		t.Fatalf("未启用 error = %v", err)
	}
}

// TestSubscriptionSearchPeriodicWiring 覆盖订阅精准搜索周期任务的构造。
func TestSubscriptionSearchPeriodicWiring(t *testing.T) {
	if job := subscriptionSearchPeriodicJob(); job == nil {
		t.Fatal("subscriptionSearchPeriodicJob() = nil")
	}
	args, opts := subscriptionSearchPeriodicArgs()
	typed, ok := args.(SubscriptionSearchSweepArgs)
	if !ok {
		t.Fatalf("周期任务参数类型 = %T", args)
	}
	if typed.Key != subscriptionSearchKey || opts == nil {
		t.Fatalf("周期任务参数 = (%#v, %v)", typed, opts)
	}
	if typed.Cutoff.IsZero() || typed.Cutoff.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("周期任务 cutoff = %v", typed.Cutoff)
	}
	if err := typed.Validate(time.Now().UTC()); err != nil {
		t.Fatalf("周期任务参数校验 error = %v", err)
	}
}
