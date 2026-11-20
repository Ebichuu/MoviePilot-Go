package indexer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖协调器的构造/关闭闸门、查询归一化、回调、关闭后的拒绝，
// 以及退避与去重这些不依赖真实站点的分支。

func TestCoordinatorConstructorAndCloseGuards(t *testing.T) {
	for _, workers := range []int{0, -1, 129} {
		if _, err := NewCoordinator(workers); err == nil {
			t.Fatalf("workers=%d 应被拒绝", workers)
		}
	}

	var nilCoordinator *Coordinator
	nilCoordinator.Close()
	if nilCoordinator.LastSearchResults() != nil {
		t.Fatal("nil 协调器的 LastSearchResults 应为 nil")
	}
	if query := nilCoordinator.LastSearchQuery(); query.Page != 0 || query.Keyword != "" {
		t.Fatalf("nil 协调器的 LastSearchQuery = %#v", query)
	}

	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatalf("构造协调器: %v", err)
	}
	coordinator.Close()
	coordinator.Close()

	resources, err := coordinator.Search(context.Background(), nil, Query{Limit: 5}, nil)
	if err != nil || len(resources) != 0 {
		t.Fatalf("空适配器 Search() = %#v, %v", resources, err)
	}
}

func TestCoordinatorSearchNormalizesQueryAndInvokesCallback(t *testing.T) {
	coordinator, err := NewCoordinator(2)
	if err != nil {
		t.Fatalf("构造协调器: %v", err)
	}
	defer coordinator.Close()

	var events int
	resources, err := coordinator.Search(context.Background(),
		[]Adapter{staticAdapter{name: "Alpha", resources: []Resource{
			{Title: "第一条", DownloadURL: "/download/1", Seeders: 9},
			{Title: "第二条", DownloadURL: "/download/2", Seeders: 3},
		}}},
		Query{Keyword: "x", Page: -5, Limit: 0},
		func(event Event) {
			events++
			if event.Site != "Alpha" {
				t.Fatalf("回调站点 = %q", event.Site)
			}
		})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if events != 1 || len(resources) != 2 {
		t.Fatalf("回调次数 = %d，结果 = %#v", events, resources)
	}
	if resources[0].Title != "第一条" || resources[0].Site != "Alpha" {
		t.Fatalf("结果排序或站点填充错误: %#v", resources)
	}
	if query := coordinator.LastSearchQuery(); query.Page != 0 || query.Limit != 100 {
		t.Fatalf("归一化后的查询 = %#v", query)
	}
	if last := coordinator.LastSearchResults(); len(last) != 2 {
		t.Fatalf("LastSearchResults() = %#v", last)
	}
}

func TestCoordinatorSearchRejectsClosedCoordinatorAndCanceledCaller(t *testing.T) {
	closedCtx, cancelClosed := context.WithCancel(context.Background())
	cancelClosed()
	closed := &Coordinator{
		workers:  1,
		jobs:     make(chan searchJob),
		gates:    map[string]chan struct{}{},
		failures: map[string]failure{},
		ctx:      closedCtx,
		cancel:   func() {},
	}
	if _, err := closed.Search(context.Background(), []Adapter{staticAdapter{name: "Alpha"}}, Query{Limit: 5}, nil); err == nil ||
		!strings.Contains(err.Error(), "已关闭") {
		t.Fatalf("已关闭协调器 Search() error = %v", err)
	}

	live := &Coordinator{
		workers:  1,
		jobs:     make(chan searchJob),
		gates:    map[string]chan struct{}{},
		failures: map[string]failure{},
		ctx:      context.Background(),
		cancel:   func() {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := live.Search(ctx, []Adapter{staticAdapter{name: "Alpha"}}, Query{Limit: 5}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消调用方上下文 Search() error = %v", err)
	}
}

func TestDeduplicateKeyPreferences(t *testing.T) {
	withItemKey := deduplicate([]Resource{
		{Site: "Alpha", SiteID: 7, ItemKey: "k1", DownloadURL: "/a", Title: "同一条"},
		{Site: "Alpha", SiteID: 7, ItemKey: "k1", DownloadURL: "/b", Title: "同一条"},
	})
	if len(withItemKey) != 1 {
		t.Fatalf("ItemKey 相同的资源应去重，得到 %#v", withItemKey)
	}

	withoutLocator := deduplicate([]Resource{
		{Site: "Alpha", DetailURL: "/detail/1", Title: "只按详情去重"},
		{Site: "Alpha", DetailURL: "/detail/1", Title: "只按详情去重"},
		{Site: "Alpha", DetailURL: "/detail/2", Title: "另一条"},
	})
	if len(withoutLocator) != 2 {
		t.Fatalf("缺少 ItemKey/DownloadURL 时应按详情去重，得到 %#v", withoutLocator)
	}
}

func TestCoordinatorRecordFailureBackoffAndGateCancellation(t *testing.T) {
	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatalf("构造协调器: %v", err)
	}
	defer coordinator.Close()

	for attempt := 0; attempt < 12; attempt++ {
		coordinator.recordFailure("slow")
	}
	coordinator.mu.Lock()
	state := coordinator.failures["slow"]
	coordinator.mu.Unlock()
	if state.count != 12 {
		t.Fatalf("失败计数 = %d, want 12", state.count)
	}
	if remaining := time.Until(state.next); remaining > 5*time.Minute+time.Second {
		t.Fatalf("退避应封顶 5 分钟，实际剩余 %s", remaining)
	}

	event := coordinator.searchOne(context.Background(), staticAdapter{name: "slow"}, Query{Limit: 5})
	if !strings.Contains(event.Error, "站点退避中") {
		t.Fatalf("退避期间 searchOne() = %#v", event)
	}

	gate := coordinator.siteGate(adapterGateKey(staticAdapter{name: "blocked"}))
	gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	event = coordinator.searchOne(ctx, staticAdapter{name: "blocked"}, Query{Limit: 5})
	if event.Error != context.Canceled.Error() {
		t.Fatalf("等待站点闸门时取消 searchOne() = %#v", event)
	}
}
