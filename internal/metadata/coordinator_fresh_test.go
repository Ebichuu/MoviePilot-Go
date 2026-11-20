package metadata

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCoordinatorDetailFreshBypassesCacheAndUpdatesDetail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cached Media
	}{
		{name: "cached empty media", cached: Media{}},
		{name: "cached empty season facts", cached: Media{ID: "tv:7", Title: "Old", Type: "tv"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := Media{
				Source: "themoviedb", ID: "tv:7", Title: "Fresh", Type: "tv",
				SeasonInfo: []SeasonInfo{{SeasonNumber: 1, EpisodeCount: 12}},
			}
			provider := &freshDetailProvider{detail: func(_ context.Context, id string) (Media, error) {
				if id != "tv:7" {
					t.Errorf("provider ID = %q, want normalized tv:7", id)
				}
				item := want
				item.Source = "provider-source"
				return item, nil
			}}
			cache := &freshDetailCache{memoryJSONCache: newMemoryJSONCache()}
			key := metadataCacheKey("detail", "themoviedb", "tv:7")
			if err := cache.memoryJSONCache.SetJSON(t.Context(), key, tt.cached, time.Hour); err != nil {
				t.Fatal(err)
			}
			coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(cache))
			cached, err := coordinator.Detail(t.Context(), "tmdb", "tv:7")
			if err != nil || !reflect.DeepEqual(cached, tt.cached) || provider.calls.Load() != 0 {
				t.Fatalf("ordinary cached Detail() = (%#v, %v), provider calls=%d", cached, err, provider.calls.Load())
			}
			cache.getCalls.Store(0)
			got, err := coordinator.DetailFresh(t.Context(), " TMDB ", " tv:7 ")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("DetailFresh() = (%#v, %v), want %#v", got, err, want)
			}
			if provider.calls.Load() != 1 || cache.getCalls.Load() != 0 || cache.setCalls.Load() != 1 {
				t.Fatalf("fresh requests = provider:%d cache reads:%d writes:%d, want 1/0/1", provider.calls.Load(), cache.getCalls.Load(), cache.setCalls.Load())
			}
			got, err = coordinator.Detail(t.Context(), " THEMOVIEDB ", " tv:7 ")
			if err != nil || !reflect.DeepEqual(got, want) || provider.calls.Load() != 1 {
				t.Fatalf("Detail() after refresh = (%#v, %v), provider calls=%d", got, err, provider.calls.Load())
			}
		})
	}
}

func TestCoordinatorDetailFreshValidatesWithoutProviderOrCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		id     string
	}{
		{name: "empty source", source: " ", id: "tv:7"},
		{name: "empty ID", source: "tmdb", id: " "},
		{name: "unconfigured source", source: "douban", id: "tv:7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
				return Media{}, nil
			}}
			cache := &freshDetailCache{memoryJSONCache: newMemoryJSONCache()}
			coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(cache))
			_, ordinaryErr := coordinator.Detail(t.Context(), tt.source, tt.id)
			_, freshErr := coordinator.DetailFresh(t.Context(), tt.source, tt.id)
			if ordinaryErr == nil || freshErr == nil || ordinaryErr.Error() != freshErr.Error() {
				t.Fatalf("validation errors = ordinary:%v fresh:%v, want same failure", ordinaryErr, freshErr)
			}
			if provider.calls.Load() != 0 || cache.getCalls.Load() != 0 || cache.setCalls.Load() != 0 {
				t.Fatalf("invalid input touched provider/cache: provider:%d reads:%d writes:%d", provider.calls.Load(), cache.getCalls.Load(), cache.setCalls.Load())
			}
		})
	}
}

func TestCoordinatorDetailFreshMergesConcurrentRequests(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		want := Media{Source: "themoviedb", ID: "tv:7", Title: "Fresh"}
		provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
			<-release
			return want, nil
		}}
		cache := &freshDetailCache{memoryJSONCache: newMemoryJSONCache()}
		coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(cache))
		const callers = 12
		results := make(chan freshDetailResult, callers)
		for i := range callers {
			go func() {
				source := "tmdb"
				if i%2 == 0 {
					source = " THEMOVIEDB "
				}
				item, err := coordinator.DetailFresh(t.Context(), source, " tv:7 ")
				results <- freshDetailResult{item: item, err: err}
			}()
		}
		synctest.Wait()
		if provider.calls.Load() != 1 || cache.getCalls.Load() != 0 {
			t.Fatalf("concurrent refresh called provider %d times and read cache %d times", provider.calls.Load(), cache.getCalls.Load())
		}
		close(release)
		for range callers {
			got := <-results
			if got.err != nil || !reflect.DeepEqual(got.item, want) {
				t.Fatalf("concurrent DetailFresh() = (%#v, %v), want %#v", got.item, got.err, want)
			}
		}
		got, err := coordinator.Detail(t.Context(), "tmdb", "tv:7")
		if err != nil || !reflect.DeepEqual(got, want) || provider.calls.Load() != 1 {
			t.Fatalf("Detail() after shared refresh = (%#v, %v), provider calls=%d", got, err, provider.calls.Load())
		}
	})
}

func TestCoordinatorDetailFreshDoesNotJoinOrdinaryFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var sequence atomic.Int32
		provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
			call := sequence.Add(1)
			<-release
			if call == 1 {
				return Media{ID: "tv:7", Title: "Ordinary"}, nil
			}
			return Media{ID: "tv:7", Title: "Fresh"}, nil
		}}
		coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(nil))
		ordinary := make(chan freshDetailResult, 1)
		fresh := make(chan freshDetailResult, 1)
		go func() {
			item, err := coordinator.Detail(t.Context(), "tmdb", "tv:7")
			ordinary <- freshDetailResult{item: item, err: err}
		}()
		synctest.Wait()
		go func() {
			item, err := coordinator.DetailFresh(t.Context(), "tmdb", "tv:7")
			fresh <- freshDetailResult{item: item, err: err}
		}()
		synctest.Wait()
		if provider.calls.Load() != 2 {
			t.Fatalf("fresh request joined ordinary flight: provider calls=%d, want 2", provider.calls.Load())
		}
		close(release)
		ordinaryResult, freshResult := <-ordinary, <-fresh
		if ordinaryResult.err != nil || ordinaryResult.item.Title != "Ordinary" || freshResult.err != nil || freshResult.item.Title != "Fresh" {
			t.Fatalf("independent flight results = ordinary:%#v fresh:%#v", ordinaryResult, freshResult)
		}
	})
}

func TestCoordinatorDetailFreshFailurePreservesCacheAndBackoff(t *testing.T) {
	t.Parallel()
	sourceErr := errors.New("provider failed")
	provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
		return Media{Title: "Partial"}, sourceErr
	}}
	cache := &freshDetailCache{memoryJSONCache: newMemoryJSONCache()}
	old := Media{ID: "tv:7", Title: "Old"}
	key := metadataCacheKey("detail", "themoviedb", "tv:7")
	if err := cache.memoryJSONCache.SetJSON(t.Context(), key, old, time.Hour); err != nil {
		t.Fatal(err)
	}
	coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(cache))
	got, err := coordinator.DetailFresh(t.Context(), "tmdb", "tv:7")
	if !errors.Is(err, sourceErr) || !reflect.DeepEqual(got, Media{}) {
		t.Fatalf("failed DetailFresh() = (%#v, %v), want zero media and provider error", got, err)
	}
	_, err = coordinator.DetailFresh(t.Context(), "tmdb", "tv:7")
	if err == nil || !strings.Contains(err.Error(), "退避") || provider.calls.Load() != 1 {
		t.Fatalf("fresh request ignored provider backoff: err=%v calls=%d", err, provider.calls.Load())
	}
	if cache.getCalls.Load() != 0 || cache.setCalls.Load() != 0 {
		t.Fatalf("failed refresh touched cache: reads=%d writes=%d", cache.getCalls.Load(), cache.setCalls.Load())
	}
	got, err = coordinator.Detail(t.Context(), "tmdb", "tv:7")
	if err != nil || !reflect.DeepEqual(got, old) {
		t.Fatalf("Detail() lost old cache after failed refresh: (%#v, %v)", got, err)
	}
}

func TestCoordinatorDetailFreshCancellationPreservesCache(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
			<-release
			return Media{ID: "tv:7", Title: "Fresh"}, nil
		}}
		cache := &freshDetailCache{memoryJSONCache: newMemoryJSONCache()}
		old := Media{ID: "tv:7", Title: "Old"}
		key := metadataCacheKey("detail", "themoviedb", "tv:7")
		if err := cache.memoryJSONCache.SetJSON(t.Context(), key, old, time.Hour); err != nil {
			t.Fatal(err)
		}
		coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(cache))
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan freshDetailResult, 1)
		go func() {
			item, err := coordinator.DetailFresh(ctx, "tmdb", "tv:7")
			result <- freshDetailResult{item: item, err: err}
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case got := <-result:
			if !errors.Is(got.err, context.Canceled) {
				t.Fatalf("canceled DetailFresh() error = %v, want canceled", got.err)
			}
		default:
			t.Fatal("canceled refresh waited for provider response")
		}
		close(release)
		synctest.Wait()
		if cache.getCalls.Load() != 0 || cache.setCalls.Load() != 0 {
			t.Fatalf("canceled refresh touched cache after provider succeeded: reads=%d writes=%d", cache.getCalls.Load(), cache.setCalls.Load())
		}
		got, err := coordinator.Detail(t.Context(), "tmdb", "tv:7")
		if err != nil || !reflect.DeepEqual(got, old) {
			t.Fatalf("Detail() lost old cache after canceled refresh: (%#v, %v)", got, err)
		}
		_, err = coordinator.DetailFresh(ctx, "tmdb", "tv:7")
		synctest.Wait()
		if !errors.Is(err, context.Canceled) || provider.calls.Load() != 1 {
			t.Fatalf("already canceled refresh = err:%v provider calls:%d, want canceled without another request", err, provider.calls.Load())
		}
	})
}

func TestCoordinatorDetailFreshCanceledWaiterDoesNotStopSharedRefresh(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		want := Media{Source: "themoviedb", ID: "tv:7", Title: "Fresh"}
		provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
			<-release
			return want, nil
		}}
		cache := &freshDetailCache{memoryJSONCache: newMemoryJSONCache()}
		coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(cache))
		ctx, cancel := context.WithCancel(t.Context())
		canceled := make(chan freshDetailResult, 1)
		active := make(chan freshDetailResult, 1)
		go func() {
			item, err := coordinator.DetailFresh(ctx, "tmdb", "tv:7")
			canceled <- freshDetailResult{item: item, err: err}
		}()
		synctest.Wait()
		go func() {
			item, err := coordinator.DetailFresh(t.Context(), "tmdb", "tv:7")
			active <- freshDetailResult{item: item, err: err}
		}()
		synctest.Wait()
		cancel()
		if got := <-canceled; !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled waiter error = %v, want canceled", got.err)
		}
		close(release)
		got := <-active
		if got.err != nil || !reflect.DeepEqual(got.item, want) || provider.calls.Load() != 1 || cache.setCalls.Load() != 1 {
			t.Fatalf("active shared refresh = (%#v, %v), provider calls=%d writes=%d", got.item, got.err, provider.calls.Load(), cache.setCalls.Load())
		}
	})
}

func TestCoordinatorDetailFreshUsesTimeoutAndProviderGate(t *testing.T) {
	t.Parallel()
	t.Run("provider timeout preserves cache", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			provider := &freshDetailProvider{detail: func(ctx context.Context, _ string) (Media, error) {
				<-ctx.Done()
				return Media{}, ctx.Err()
			}}
			cache := &freshDetailCache{memoryJSONCache: newMemoryJSONCache()}
			old := Media{ID: "tv:7", Title: "Old"}
			if err := cache.memoryJSONCache.SetJSON(t.Context(), metadataCacheKey("detail", "themoviedb", "tv:7"), old, time.Hour); err != nil {
				t.Fatal(err)
			}
			coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(cache))
			_, err := coordinator.DetailFresh(t.Context(), "tmdb", "tv:7")
			if !errors.Is(err, context.DeadlineExceeded) || cache.setCalls.Load() != 0 {
				t.Fatalf("timed out refresh = err:%v cache writes:%d", err, cache.setCalls.Load())
			}
			got, err := coordinator.Detail(t.Context(), "tmdb", "tv:7")
			if err != nil || !reflect.DeepEqual(got, old) {
				t.Fatalf("Detail() lost old cache after timeout: (%#v, %v)", got, err)
			}
		})
	})
	t.Run("minimum interval applies to fresh calls", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
				return Media{ID: "tv:7"}, nil
			}}
			options := freshDetailOptions(nil)
			options.ProviderMinInterval = 50 * time.Millisecond
			coordinator := newTestCoordinator(t, []Provider{provider}, options)
			if _, err := coordinator.DetailFresh(t.Context(), "tmdb", "tv:7"); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if _, err := coordinator.DetailFresh(t.Context(), "tmdb", "tv:7"); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed != options.ProviderMinInterval || provider.calls.Load() != 2 {
				t.Fatalf("fresh provider gate elapsed=%s calls=%d, want %s/2", elapsed, provider.calls.Load(), options.ProviderMinInterval)
			}
		})
	})
}

func TestCoordinatorDetailFreshCacheFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	want := Media{Source: "themoviedb", ID: "tv:7", Title: "Fresh"}
	provider := &freshDetailProvider{detail: func(context.Context, string) (Media, error) {
		return want, nil
	}}
	coordinator := newTestCoordinator(t, []Provider{provider}, freshDetailOptions(failingJSONCache{}))
	got, err := coordinator.DetailFresh(t.Context(), "tmdb", "tv:7")
	if err != nil || !reflect.DeepEqual(got, want) || provider.calls.Load() != 1 {
		t.Fatalf("DetailFresh() with unavailable cache = (%#v, %v), provider calls=%d", got, err, provider.calls.Load())
	}
}

type freshDetailProvider struct {
	detail func(context.Context, string) (Media, error)
	calls  atomic.Int32
}

func (*freshDetailProvider) Name() string { return "themoviedb" }

func (*freshDetailProvider) Search(context.Context, string) ([]Media, error) { return nil, nil }

func (p *freshDetailProvider) Detail(ctx context.Context, id string) (Media, error) {
	p.calls.Add(1)
	return p.detail(ctx, id)
}

type freshDetailCache struct {
	*memoryJSONCache
	getCalls atomic.Int32
	setCalls atomic.Int32
}

func (c *freshDetailCache) GetJSON(ctx context.Context, key string, target any) (bool, error) {
	c.getCalls.Add(1)
	return c.memoryJSONCache.GetJSON(ctx, key, target)
}

func (c *freshDetailCache) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	c.setCalls.Add(1)
	return c.memoryJSONCache.SetJSON(ctx, key, value, ttl)
}

type freshDetailResult struct {
	item Media
	err  error
}

func freshDetailOptions(cache JSONCache) Options {
	return Options{Cache: cache, RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 20}
}
