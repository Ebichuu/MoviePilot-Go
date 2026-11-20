package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResourceOmitsInternalIDsFromJSON(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal(Resource{Title: "fixture", TorrentID: "77", DownloadID: "private-locator"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "77") || strings.Contains(string(payload), "private-locator") {
		t.Fatalf("internal resource IDs leaked to JSON: %s", payload)
	}
}

type testAdapter struct {
	name      string
	active    *atomic.Int32
	maxActive *atomic.Int32
}

func (a testAdapter) Name() string { return a.name }

func (a testAdapter) Search(ctx context.Context, _ Query) ([]Resource, error) {
	active := a.active.Add(1)
	defer a.active.Add(-1)
	for {
		maximum := a.maxActive.Load()
		if active <= maximum || a.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	select {
	case <-time.After(20 * time.Millisecond):
		return []Resource{{Title: a.name, DownloadURL: a.name}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCoordinatorBoundsGlobalConcurrency(t *testing.T) {
	coordinator, err := NewCoordinator(3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	var active, maximum atomic.Int32
	adapters := make([]Adapter, 10)
	for i := range adapters {
		adapters[i] = testAdapter{name: fmt.Sprintf("site-%d", i), active: &active, maxActive: &maximum}
	}
	resources, err := coordinator.Search(context.Background(), adapters, Query{Keyword: "test"}, nil)
	if err != nil || len(resources) != len(adapters) {
		t.Fatalf("搜索失败: resources=%d err=%v", len(resources), err)
	}
	if maximum.Load() > 3 {
		t.Fatalf("并发超过上限: %d", maximum.Load())
	}
}

type staticAdapter struct {
	name      string
	resources []Resource
}

func (a staticAdapter) Name() string { return a.name }

func (a staticAdapter) Search(context.Context, Query) ([]Resource, error) {
	return a.resources, nil
}

func TestCoordinatorAllowsBrowseAndCapsAdapterResults(t *testing.T) {
	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	resources := make([]Resource, 25)
	for index := range resources {
		resources[index] = Resource{Title: fmt.Sprintf("item-%d", index), DownloadURL: fmt.Sprintf("url-%d", index)}
	}
	got, err := coordinator.Search(context.Background(), []Adapter{
		staticAdapter{name: "browse", resources: resources},
	}, Query{Limit: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("单站点结果未按 limit 截断: %d", len(got))
	}
}

func TestCoordinatorBoundsConcurrencyAcrossSearchRequests(t *testing.T) {
	coordinator, err := NewCoordinator(3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	var active, maximum atomic.Int32
	var group sync.WaitGroup
	group.Add(2)
	for request := range 2 {
		go func() {
			defer group.Done()
			adapters := make([]Adapter, 6)
			for index := range adapters {
				adapters[index] = testAdapter{name: fmt.Sprintf("request-%d-site-%d", request, index), active: &active, maxActive: &maximum}
			}
			_, _ = coordinator.Search(context.Background(), adapters, Query{Keyword: "test"}, nil)
		}()
	}
	group.Wait()
	if maximum.Load() > 3 {
		t.Fatalf("跨请求并发超过全局上限: %d", maximum.Load())
	}
}

type errorAdapter struct {
	name string
	err  error
}

func (a errorAdapter) Name() string { return a.name }
func (a errorAdapter) Search(context.Context, Query) ([]Resource, error) {
	return nil, a.err
}

func TestCoordinatorDoesNotBackoffCanceledSearch(t *testing.T) {
	coordinator, err := NewCoordinator(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Close)
	_, err = coordinator.Search(context.Background(), []Adapter{errorAdapter{name: "fixture", err: context.Canceled}}, Query{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if wait := coordinator.backoff("fixture"); wait > 0 {
		t.Fatalf("取消请求不应触发站点退避: %s", wait)
	}
	_, _ = coordinator.Search(context.Background(), []Adapter{errorAdapter{name: "fixture", err: errors.New("site failed")}}, Query{}, nil)
	if wait := coordinator.backoff("fixture"); wait <= 0 {
		t.Fatal("真实站点错误应触发退避")
	}
}
