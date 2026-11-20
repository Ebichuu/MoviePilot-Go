package metadata

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorRecognizeUsesCrossProviderRanking(t *testing.T) {
	coordinator := newTestCoordinator(t, []Provider{
		&stubProvider{name: "themoviedb", items: []Media{
			{Source: "themoviedb", ID: "1", Type: "movie", Title: "示例电影", Year: 2020},
		}},
		&stubProvider{name: "douban", items: []Media{
			{Source: "douban", ID: "2", Type: "movie", Title: "示例电影", Year: 2026, Aliases: []string{"Example Movie"}},
		}},
		&stubProvider{name: "bangumi", searchErr: errors.New("temporary unavailable")},
	}, Options{RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20})

	match, found, err := coordinator.Recognize(context.Background(), Query{
		Keyword: "Example Movie", Type: "movie", Year: 2026,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found || match.Media.Source != "douban" || match.Media.ID != "2" {
		t.Fatalf("跨源排序选择错误: %#v found=%v", match, found)
	}
}

func TestCoordinatorSearchReturnsPartialResults(t *testing.T) {
	coordinator := newTestCoordinator(t, []Provider{
		&stubProvider{name: "themoviedb", searchErr: errors.New("rate limited")},
		&stubProvider{name: "douban", items: []Media{{Source: "douban", ID: "2", Type: "tv", Title: "动画", Year: 2026}}},
	}, Options{RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20})

	result, err := coordinator.Search(context.Background(), Query{Keyword: "动画", Type: "tv"})
	if err != nil {
		t.Fatalf("单源失败不应丢弃其他来源结果: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Media.Source != "douban" {
		t.Fatalf("部分结果错误: %#v", result.Matches)
	}
	if result.Errors["themoviedb"] == nil {
		t.Fatalf("缺少失败来源诊断: %#v", result.Errors)
	}
}

func TestCoordinatorMergesConcurrentRequestsAndCaches(t *testing.T) {
	provider := &stubProvider{
		name:  "themoviedb",
		items: []Media{{Source: "themoviedb", ID: "1", Type: "movie", Title: "Example"}},
		delay: 30 * time.Millisecond,
	}
	cache := newMemoryJSONCache()
	coordinator := newTestCoordinator(t, []Provider{provider}, Options{
		Cache: cache, RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})

	const callers = 12
	start := make(chan struct{})
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := coordinator.Search(context.Background(), Query{Keyword: "Example"})
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := provider.searchCalls.Load(); calls != 1 {
		t.Fatalf("并发相同请求未合并: calls=%d", calls)
	}

	if _, err := coordinator.Search(context.Background(), Query{Keyword: "Example"}); err != nil {
		t.Fatal(err)
	}
	if calls := provider.searchCalls.Load(); calls != 1 {
		t.Fatalf("缓存命中后仍调用供应商: calls=%d", calls)
	}
}

func TestCoordinatorCapsAndDeduplicatesResults(t *testing.T) {
	coordinator := newTestCoordinator(t, []Provider{
		&stubProvider{name: "themoviedb", items: []Media{
			{Source: "themoviedb", ID: "1", Title: "Example"},
			{Source: "themoviedb", ID: "1", Title: "Example duplicate"},
			{Source: "themoviedb", ID: "2", Title: "Example Two"},
			{Source: "themoviedb", ID: "3", Title: "Example Three"},
		}},
	}, Options{RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 2})

	result, err := coordinator.Search(context.Background(), Query{Keyword: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 2 {
		t.Fatalf("结果上限或去重失败: %#v", result.Matches)
	}
}

func TestCoordinatorDetailUsesCacheAndSourceAliases(t *testing.T) {
	provider := &stubProvider{
		name:   "themoviedb",
		detail: Media{Source: "themoviedb", ID: "movie:7", Type: "movie", Title: "Example"},
	}
	coordinator := newTestCoordinator(t, []Provider{provider}, Options{
		Cache: newMemoryJSONCache(), RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})

	for range 2 {
		item, err := coordinator.Detail(context.Background(), "tmdb", "movie:7")
		if err != nil || item.ID != "movie:7" {
			t.Fatalf("详情查询失败: %#v err=%v", item, err)
		}
	}
	if calls := provider.detailCalls.Load(); calls != 1 {
		t.Fatalf("详情缓存未生效: calls=%d", calls)
	}
}

func TestCoordinatorCacheFailureIsNonFatal(t *testing.T) {
	provider := &stubProvider{
		name: "douban", items: []Media{{Source: "douban", ID: "8", Title: "Example"}},
	}
	coordinator := newTestCoordinator(t, []Provider{provider}, Options{
		Cache: failingJSONCache{}, RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20,
	})
	result, err := coordinator.Search(context.Background(), Query{Keyword: "Example"})
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("缓存故障不应阻断上游查询: matches=%#v err=%v", result.Matches, err)
	}
}

func TestCoordinatorTimesOutAndBacksOffProvider(t *testing.T) {
	provider := &stubProvider{
		name: "anilist", delay: 200 * time.Millisecond,
		items: []Media{{Source: "anilist", ID: "9", Title: "Example"}},
	}
	coordinator := newTestCoordinator(t, []Provider{provider}, Options{
		RequestTimeout: 10 * time.Millisecond, CacheTTL: time.Hour, MaxResults: 20,
		ProviderMaxBackoff: time.Second,
	})
	for range 2 {
		if _, err := coordinator.Search(context.Background(), Query{Keyword: "Example"}); err == nil {
			t.Fatal("超时或退避中的供应商不应返回成功")
		}
	}
	if calls := provider.searchCalls.Load(); calls != 1 {
		t.Fatalf("退避期间仍调用供应商: calls=%d", calls)
	}
}

func newTestCoordinator(t *testing.T, providers []Provider, options Options) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(providers, options)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

type stubProvider struct {
	name        string
	items       []Media
	detail      Media
	searchErr   error
	detailErr   error
	delay       time.Duration
	searchCalls atomic.Int32
	detailCalls atomic.Int32
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Search(ctx context.Context, _ string) ([]Media, error) {
	p.searchCalls.Add(1)
	if p.delay > 0 {
		timer := time.NewTimer(p.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return append([]Media(nil), p.items...), p.searchErr
}

func (p *stubProvider) Detail(context.Context, string) (Media, error) {
	p.detailCalls.Add(1)
	return p.detail, p.detailErr
}

type memoryJSONCache struct {
	mu     sync.Mutex
	values map[string]any
}

func newMemoryJSONCache() *memoryJSONCache {
	return &memoryJSONCache{values: make(map[string]any)}
}

func (c *memoryJSONCache) GetJSON(_ context.Context, key string, target any) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, exists := c.values[key]
	if !exists {
		return false, nil
	}
	switch output := target.(type) {
	case *[]Media:
		*output = append((*output)[:0], value.([]Media)...)
	case *Media:
		*output = value.(Media)
	default:
		return false, errors.New("unsupported cache target")
	}
	return true, nil
}

func (c *memoryJSONCache) SetJSON(_ context.Context, key string, value any, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch item := value.(type) {
	case []Media:
		c.values[key] = append([]Media(nil), item...)
	case Media:
		c.values[key] = item
	default:
		return errors.New("unsupported cache value")
	}
	return nil
}

type failingJSONCache struct{}

func (failingJSONCache) GetJSON(context.Context, string, any) (bool, error) {
	return false, errors.New("cache unavailable")
}

func (failingJSONCache) SetJSON(context.Context, string, any, time.Duration) error {
	return errors.New("cache unavailable")
}
