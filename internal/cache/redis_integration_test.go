//go:build integration

package cache

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

const (
	redisFaultTestURL         = "MOVIEPILOT_TEST_REDIS_URL"
	redisFaultTestAck         = "MOVIEPILOT_FAULT_TEST"
	redisFaultTestMarkerKey   = "MOVIEPILOT_TEST_REDIS_MARKER_KEY"
	redisFaultTestMarkerValue = "MOVIEPILOT_TEST_REDIS_MARKER_VALUE"
)

func TestRedisFaultRecovery(t *testing.T) {
	redisURL := dedicatedRedisFaultURL(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	redisCache, err := Open(ctx, config.Config{
		RedisURL:      redisURL,
		RedisPoolSize: 2,
		MaxCacheValue: 1 << 20,
	})
	if err != nil {
		t.Fatalf("打开测试 Redis 缓存: %v", err)
	}
	t.Cleanup(func() {
		if err := redisCache.Close(); err != nil {
			t.Errorf("关闭测试 Redis 缓存: %v", err)
		}
	})

	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("解析测试 Redis 地址: %v", err)
	}
	raw := redis.NewClient(options)
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("关闭故障注入 Redis 客户端: %v", err)
		}
	})
	assertDedicatedRedisMarker(t, ctx, raw)

	t.Run("清空缓存后可回填", func(t *testing.T) {
		provider := &redisFaultProvider{}
		recording := &recordingJSONCache{delegate: redisCache}
		coordinator := newRedisFaultCoordinator(t, provider, recording)
		assertRedisFaultDetail(t, coordinator, "flush", "warm")
		assertRedisFaultDetail(t, coordinator, "flush", "warm")
		if calls := provider.calls.Load(); calls != 1 {
			t.Fatalf("清空前 provider 调用次数 = %d, want 1", calls)
		}

		if err := raw.FlushDB(ctx).Err(); err != nil {
			t.Fatalf("清空专用测试 Redis: %v", err)
		}
		assertRedisFaultDetail(t, coordinator, "flush", "rebuilt")
		assertRedisFaultDetail(t, coordinator, "flush", "rebuilt")
		if calls := provider.calls.Load(); calls != 2 {
			t.Fatalf("清空后 provider 调用次数 = %d, want 2", calls)
		}
		key := recording.lastSetKey(t)
		if ttl, err := raw.TTL(ctx, namespaced(key)).Result(); err != nil || ttl <= 0 {
			t.Fatalf("Coordinator 回填 TTL = %s, err=%v", ttl, err)
		}
	})

	t.Run("损坏JSON后协调器回源并重建", func(t *testing.T) {
		provider := &redisFaultProvider{}
		recording := &recordingJSONCache{delegate: redisCache}
		coordinator := newRedisFaultCoordinator(t, provider, recording)
		assertRedisFaultDetail(t, coordinator, "corrupt", "warm")
		key := recording.lastSetKey(t)
		qualified := namespaced(key)
		if err := raw.Set(ctx, qualified, "{broken", time.Minute).Err(); err != nil {
			t.Fatalf("写入损坏缓存: %v", err)
		}
		assertRedisFaultDetail(t, coordinator, "corrupt", "rebuilt")
		assertRedisFaultDetail(t, coordinator, "corrupt", "rebuilt")
		if calls := provider.calls.Load(); calls != 2 {
			t.Fatalf("损坏缓存后 provider 调用次数 = %d, want 2", calls)
		}
		if errors := recording.getErrors(); errors != 1 {
			t.Fatalf("损坏缓存读取错误次数 = %d, want 1", errors)
		}
		if ttl, err := raw.TTL(ctx, qualified).Result(); err != nil || ttl <= 0 {
			t.Fatalf("损坏缓存重建 TTL = %s, err=%v", ttl, err)
		}
	})
}

type redisFaultProvider struct {
	calls atomic.Int32
}

func (*redisFaultProvider) Name() string { return "fault-provider" }

func (*redisFaultProvider) Search(context.Context, string) ([]metadata.Media, error) {
	return nil, errors.New("fault provider search is not supported")
}

func (p *redisFaultProvider) Detail(_ context.Context, id string) (metadata.Media, error) {
	title := "warm"
	if p.calls.Add(1) > 1 {
		title = "rebuilt"
	}
	return metadata.Media{ID: id, Type: "movie", Title: title}, nil
}

type recordingJSONCache struct {
	delegate *Redis
	mu       sync.Mutex
	setKey   string
	errors   int
}

func (c *recordingJSONCache) GetJSON(ctx context.Context, key string, target any) (bool, error) {
	hit, err := c.delegate.GetJSON(ctx, key, target)
	if err != nil {
		c.mu.Lock()
		c.errors++
		c.mu.Unlock()
	}
	return hit, err
}

func (c *recordingJSONCache) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	if err := c.delegate.SetJSON(ctx, key, value, ttl); err != nil {
		return err
	}
	c.mu.Lock()
	c.setKey = key
	c.mu.Unlock()
	return nil
}

func (c *recordingJSONCache) lastSetKey(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setKey == "" {
		t.Fatal("Coordinator 未写入 Redis 缓存")
	}
	return c.setKey
}

func (c *recordingJSONCache) getErrors() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errors
}

func newRedisFaultCoordinator(t *testing.T, provider metadata.Provider, cache metadata.JSONCache) *metadata.Coordinator {
	t.Helper()
	coordinator, err := metadata.NewCoordinator([]metadata.Provider{provider}, metadata.Options{
		Cache: cache, CacheTTL: time.Minute, RequestTimeout: 2 * time.Second, MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("创建元数据协调器: %v", err)
	}
	return coordinator
}

func assertRedisFaultDetail(t *testing.T, coordinator *metadata.Coordinator, id, wantTitle string) {
	t.Helper()
	item, err := coordinator.Detail(t.Context(), "fault-provider", id)
	if err != nil {
		t.Fatalf("Coordinator.Detail(%q): %v", id, err)
	}
	if item.Source != "fault-provider" || item.ID != id || item.Title != wantTitle {
		t.Fatalf("Coordinator.Detail(%q) = %#v, want title %q", id, item, wantTitle)
	}
}

func assertDedicatedRedisMarker(t *testing.T, ctx context.Context, client *redis.Client) {
	t.Helper()
	key := strings.TrimSpace(os.Getenv(redisFaultTestMarkerKey))
	want := strings.TrimSpace(os.Getenv(redisFaultTestMarkerValue))
	if key == "" || want == "" {
		t.Fatalf("故障测试必须设置 %s 和 %s", redisFaultTestMarkerKey, redisFaultTestMarkerValue)
	}
	if key != "mp:fault:v1:instance-marker" || len(want) < 32 || len(want) > 128 {
		t.Fatalf("专用 Redis marker 契约无效")
	}
	got, err := client.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("读取专用 Redis marker: %v", err)
	}
	if got != want {
		t.Fatalf("拒绝清空 marker 不匹配的 Redis")
	}
}

func dedicatedRedisFaultURL(t *testing.T) string {
	t.Helper()
	if os.Getenv(redisFaultTestAck) != "1" {
		t.Skipf("仅在 %s=1 的隔离故障栈中运行", redisFaultTestAck)
	}
	rawURL := strings.TrimSpace(os.Getenv(redisFaultTestURL))
	if rawURL == "" {
		t.Fatalf("启用故障测试后必须设置 %s", redisFaultTestURL)
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("%s 无效: %v", redisFaultTestURL, err)
	}
	host, port, err := net.SplitHostPort(options.Addr)
	if err != nil {
		t.Fatalf("%s 必须包含显式主机和端口: %v", redisFaultTestURL, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		t.Fatalf("拒绝对非回环 Redis 执行故障测试: %q", host)
	}
	if port == "6379" {
		t.Fatalf("拒绝对默认 Redis 端口 6379 执行故障测试")
	}
	if options.DB != 15 {
		t.Fatalf("故障测试必须使用专用 Redis database 15，当前为 %d", options.DB)
	}
	return rawURL
}
