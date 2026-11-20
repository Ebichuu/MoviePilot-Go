package siteflow

import (
	"context"
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

type batchSiteSourceFake struct {
	sites []BatchSite
	err   error
	calls int
}

func (f *batchSiteSourceFake) ListSitesForRefresh(context.Context) ([]BatchSite, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.sites, nil
}

type batchRefresherFake struct {
	updated      map[int]bool
	failures     map[int]bool
	calls        []int
	onRefresh    func(site BatchSite)
	lastDataName string
}

func (f *batchRefresherFake) RefreshSiteUserData(_ context.Context, site BatchSite) (indexer.SiteUserData, bool, error) {
	f.calls = append(f.calls, site.ID)
	if f.onRefresh != nil {
		f.onRefresh(site)
	}
	if f.failures[site.ID] {
		return indexer.SiteUserData{}, false, errors.New("用户数据拉取失败")
	}
	data := indexer.SiteUserData{Domain: site.Name}
	return data, f.updated[site.ID], nil
}

type publisherFake struct {
	calls int
}

func (f *publisherFake) PublishSiteRefreshedAll(context.Context) { f.calls++ }

func newBatchFixture(t *testing.T, sites []BatchSite) (*BatchRefresher, *batchSiteSourceFake, *batchRefresherFake, *publisherFake) {
	t.Helper()
	source := &batchSiteSourceFake{sites: sites}
	refresher := &batchRefresherFake{updated: map[int]bool{}, failures: map[int]bool{}}
	publisher := &publisherFake{}
	batch, err := NewBatchRefresher(source, refresher, publisher)
	if err != nil {
		t.Fatalf("创建批量刷新器失败: %v", err)
	}
	return batch, source, refresher, publisher
}

func TestNewBatchRefresherRequiresDependencies(t *testing.T) {
	refresher := &batchRefresherFake{updated: map[int]bool{}}
	if _, err := NewBatchRefresher(nil, refresher, nil); err == nil {
		t.Fatal("缺少站点来源应报错")
	}
	source := &batchSiteSourceFake{}
	if _, err := NewBatchRefresher(source, nil, nil); err == nil {
		t.Fatal("缺少刷新器应报错")
	}
	// 发布器可以为 nil（没有通知通道时仍然要能刷新）。
	if batch, err := NewBatchRefresher(source, refresher, nil); err != nil || batch == nil {
		t.Fatalf("发布器可选: %v %v", batch, err)
	}
}

func TestBatchRefreshSkipsInactiveAndInvalidSites(t *testing.T) {
	batch, _, refresher, publisher := newBatchFixture(t, []BatchSite{
		{ID: 1, Name: "启用站", Active: true},
		{ID: 2, Name: "停用站", Active: false},
		{ID: 0, Name: "无 ID 站", Active: true},
		{ID: 3, Name: "也启用", Active: true},
	})
	refresher.updated[1] = true
	refresher.updated[3] = true

	result, err := batch.RefreshAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("批量刷新失败: %v", err)
	}
	// V2 的 get_indexers() 只返回 is_active 站点，停用与无 ID 的都不该被访问。
	if len(refresher.calls) != 2 || refresher.calls[0] != 1 || refresher.calls[1] != 3 {
		t.Fatalf("刷新顺序 = %v", refresher.calls)
	}
	if result.Total != 2 {
		t.Fatalf("Total = %d", result.Total)
	}
	if result.Refreshed != 2 || result.Failed != 0 || !result.AnyUpdated {
		t.Fatalf("结果 = %+v", result)
	}
	if len(result.Data) != 2 {
		t.Fatalf("数据 = %v", result.Data)
	}
	if _, ok := result.Data["启用站"]; !ok {
		t.Fatalf("数据应以站点名为键: %v", result.Data)
	}
	if publisher.calls != 1 {
		t.Fatalf("发布次数 = %d", publisher.calls)
	}
}

func TestBatchRefreshCountsFailuresAndContinues(t *testing.T) {
	batch, _, refresher, publisher := newBatchFixture(t, []BatchSite{
		{ID: 1, Name: "坏站", Active: true},
		{ID: 2, Name: "好站", Active: true},
		{ID: 3, Name: "没更新", Active: true},
	})
	refresher.failures[1] = true
	refresher.updated[2] = true

	result, err := batch.RefreshAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("单站失败不应让整批失败: %v", err)
	}
	if result.Failed != 1 || result.Refreshed != 1 || result.Total != 3 {
		t.Fatalf("结果 = %+v", result)
	}
	// 失败站之后仍要继续刷新。
	if len(refresher.calls) != 3 {
		t.Fatalf("调用 = %v", refresher.calls)
	}
	if publisher.calls != 1 {
		t.Fatalf("发布次数 = %d", publisher.calls)
	}
}

func TestBatchRefreshPublishesOnlyWhenSomethingUpdated(t *testing.T) {
	batch, _, refresher, publisher := newBatchFixture(t, []BatchSite{
		{ID: 1, Name: "A", Active: true},
		{ID: 2, Name: "B", Active: true},
	})
	// 没有任何站点产生新数据：不能发「全部站点已刷新」事件，否则下游会白跑一轮。
	result, err := batch.RefreshAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("批量刷新失败: %v", err)
	}
	if result.AnyUpdated || result.Refreshed != 0 {
		t.Fatalf("结果 = %+v", result)
	}
	if publisher.calls != 0 {
		t.Fatalf("发布次数 = %d", publisher.calls)
	}
	if len(refresher.calls) != 2 {
		t.Fatalf("调用 = %v", refresher.calls)
	}
}

func TestBatchRefreshReportsProgress(t *testing.T) {
	batch, _, refresher, _ := newBatchFixture(t, []BatchSite{
		{ID: 7, Name: "A", Active: true},
		{ID: 8, Name: "B", Active: true},
	})
	refresher.updated[7] = true

	var progress []BatchProgress
	if _, err := batch.RefreshAll(context.Background(), func(step BatchProgress) {
		progress = append(progress, step)
	}); err != nil {
		t.Fatalf("批量刷新失败: %v", err)
	}
	// 先报总数，再逐个报完成数；finished 单调递增且带当前站点 ID。
	if len(progress) != 3 {
		t.Fatalf("进度回调 = %+v", progress)
	}
	if progress[0] != (BatchProgress{Total: 2}) {
		t.Fatalf("首次进度 = %+v", progress[0])
	}
	if progress[1] != (BatchProgress{Total: 2, Finished: 1, Current: 7}) {
		t.Fatalf("第二次进度 = %+v", progress[1])
	}
	if progress[2].Finished != 2 || progress[2].Current != 8 {
		t.Fatalf("第三次进度 = %+v", progress[2])
	}
}

func TestBatchRefreshStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batch, _, refresher, publisher := newBatchFixture(t, []BatchSite{
		{ID: 1, Name: "A", Active: true},
		{ID: 2, Name: "B", Active: true},
		{ID: 3, Name: "C", Active: true},
	})
	// 第一个站点刷新期间系统停止：必须立刻返回取消错误，不再访问后续站点。
	refresher.onRefresh = func(site BatchSite) {
		if site.ID == 1 {
			cancel()
		}
	}

	result, err := batch.RefreshAll(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应返回 context.Canceled，实际 %v", err)
	}
	if len(refresher.calls) != 1 {
		t.Fatalf("取消后不应继续刷新: %v", refresher.calls)
	}
	if result.AnyUpdated {
		t.Fatalf("取消时不应标记已更新: %+v", result)
	}
	if publisher.calls != 0 {
		t.Fatalf("取消时不应发布事件，实际 %d 次", publisher.calls)
	}
}

func TestBatchRefreshCancelledBeforeStart(t *testing.T) {
	batch, source, refresher, _ := newBatchFixture(t, []BatchSite{{ID: 1, Name: "A", Active: true}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := batch.RefreshAll(ctx, nil); err == nil {
		t.Fatal("已取消的上下文应报错")
	}
	// 取消要在真正干活之前生效（来源查询本身就会带 ctx 失败，这里来源不校验 ctx，
	// 因此至少在进入循环前就退出）。
	if len(refresher.calls) != 0 {
		t.Fatalf("取消后不应刷新任何站点: %v", refresher.calls)
	}
	_ = source.calls
}

func TestBatchRefreshRejectsInvalidInput(t *testing.T) {
	batch, source, _, _ := newBatchFixture(t, nil)
	if _, err := batch.RefreshAll(nil, nil); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	source.err = errors.New("数据库不可用")
	if _, err := batch.RefreshAll(context.Background(), nil); err == nil {
		t.Fatal("站点来源报错应向上返回")
	}
}

func TestBatchRefreshSkipsDataForUnnamedSites(t *testing.T) {
	batch, _, refresher, _ := newBatchFixture(t, []BatchSite{{ID: 5, Active: true}})
	refresher.updated[5] = true

	result, err := batch.RefreshAll(context.Background(), nil)
	if err != nil {
		t.Fatalf("批量刷新失败: %v", err)
	}
	// 有更新就计数，但没名字的站点不进数据表（否则会出现空键）。
	if result.Refreshed != 1 || !result.AnyUpdated {
		t.Fatalf("结果 = %+v", result)
	}
	if len(result.Data) != 0 {
		t.Fatalf("数据 = %v", result.Data)
	}
}

func TestBatchRefreshMessageMatchesV2(t *testing.T) {
	if got := (BatchRefreshResult{Refreshed: 2, Failed: 1}).Message(); got != "刷新了2个站点，1个站点失败" {
		t.Fatalf("文本 = %q", got)
	}
	if got := (BatchRefreshResult{}).Message(); got != "刷新了0个站点，0个站点失败" {
		t.Fatalf("文本 = %q", got)
	}
}
