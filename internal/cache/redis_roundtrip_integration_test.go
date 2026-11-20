//go:build integration

package cache

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// 缓存层的基本契约只有连真实 Redis 才能验：命名空间、强制 TTL、超限拒绝、
// 损坏值自愈删除。故障栈测试覆盖的是「断线重连」，这里覆盖的是「正常路径」。
const redisRoundTripEnv = "MOVIEPILOT_TEST_REDIS_URL"

func openTestRedis(t *testing.T, maxValue int) *Redis {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(redisRoundTripEnv))
	if url == "" {
		t.Skipf("未设置 %s，跳过 Redis 集成测试", redisRoundTripEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cache, err := Open(ctx, config.Config{RedisURL: url, RedisPoolSize: 4, MaxCacheValue: maxValue})
	if err != nil {
		t.Fatalf("连接测试 Redis 失败: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func TestOpenRejectsInvalidRedisURL(t *testing.T) {
	if _, err := Open(context.Background(), config.Config{RedisURL: "not-a-url"}); err == nil {
		t.Fatal("非法 Redis 地址必须报错")
	}
	// 端口不可达时必须在 Open 阶段就失败，不能把坏连接留到第一次读写。
	if _, err := Open(context.Background(), config.Config{RedisURL: "redis://127.0.0.1:6390/0"}); err == nil {
		t.Fatal("不可达的 Redis 必须在 Open 时失败")
	}
}

func TestRedisJSONRoundTrip(t *testing.T) {
	cache := openTestRedis(t, 1<<20)
	ctx := context.Background()
	key := "itest:" + uuid.NewString()
	t.Cleanup(func() { _ = cache.Delete(context.Background(), key) })

	payload := map[string]any{"title": "沙丘", "year": 2021}
	if err := cache.SetJSON(ctx, key, payload, time.Minute); err != nil {
		t.Fatalf("写入缓存失败: %v", err)
	}

	var decoded map[string]any
	found, err := cache.GetJSON(ctx, key, &decoded)
	if err != nil || !found {
		t.Fatalf("读取缓存失败: found=%v err=%v", found, err)
	}
	if decoded["title"] != "沙丘" {
		t.Fatalf("缓存内容不对: %+v", decoded)
	}

	// 命名空间前缀必须真的加上，否则多环境会互相污染。
	raw, err := cache.client.Get(ctx, namespaced(key)).Result()
	if err != nil {
		t.Fatalf("直接读取原始键失败: %v", err)
	}
	var rawPayload map[string]any
	if err := json.Unmarshal([]byte(raw), &rawPayload); err != nil {
		t.Fatalf("原始值不是合法 JSON: %v", err)
	}
	if !strings.HasPrefix(namespaced(key), namespace) {
		t.Fatalf("命名空间前缀缺失: %q", namespaced(key))
	}

	// TTL 必须被设置（带抖动的正数），永远不能写出永久键。
	ttl, err := cache.client.TTL(ctx, namespaced(key)).Result()
	if err != nil {
		t.Fatalf("读取 TTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > 2*time.Minute {
		t.Fatalf("TTL 不合理: %v", ttl)
	}

	if err := cache.Delete(ctx, key); err != nil {
		t.Fatalf("删除缓存失败: %v", err)
	}
	if found, err := cache.GetJSON(ctx, key, &decoded); err != nil || found {
		t.Fatalf("删除后不应再读到: found=%v err=%v", found, err)
	}
}

func TestRedisSetJSONValidation(t *testing.T) {
	cache := openTestRedis(t, 16)
	ctx := context.Background()
	key := "itest:" + uuid.NewString()
	t.Cleanup(func() { _ = cache.Delete(context.Background(), key) })

	if err := cache.SetJSON(ctx, key, map[string]any{"a": 1}, 0); err == nil {
		t.Fatal("TTL <= 0 必须拒绝")
	}
	if err := cache.SetJSON(ctx, key, strings.Repeat("x", 64), time.Minute); err == nil {
		t.Fatal("超过 MaxCacheValue 的值必须拒绝")
	}
	// 编码失败（channel 不可 JSON 序列化）也要报错而不是写进去。
	if err := cache.SetJSON(ctx, key, make(chan int), time.Minute); err == nil {
		t.Fatal("不可序列化的值必须报错")
	}
}

func TestRedisGetJSONHealsBadEntries(t *testing.T) {
	cache := openTestRedis(t, 16)
	ctx := context.Background()

	oversizeKey := "itest:big:" + uuid.NewString()
	corruptKey := "itest:bad:" + uuid.NewString()
	t.Cleanup(func() { _ = cache.Delete(context.Background(), oversizeKey, corruptKey) })

	if err := cache.client.Set(ctx, namespaced(oversizeKey), strings.Repeat("y", 64), time.Minute).Err(); err != nil {
		t.Fatalf("写入超限值失败: %v", err)
	}
	var target map[string]any
	if _, err := cache.GetJSON(ctx, oversizeKey, &target); err == nil {
		t.Fatal("超限缓存应返回错误")
	}
	if exists, _ := cache.client.Exists(ctx, namespaced(oversizeKey)).Result(); exists != 0 {
		t.Fatal("超限缓存应被删除，避免反复命中")
	}

	if err := cache.client.Set(ctx, namespaced(corruptKey), "{not json", time.Minute).Err(); err != nil {
		t.Fatalf("写入损坏值失败: %v", err)
	}
	if _, err := cache.GetJSON(ctx, corruptKey, &target); err == nil {
		t.Fatal("损坏缓存应返回错误")
	}
	if exists, _ := cache.client.Exists(ctx, namespaced(corruptKey)).Result(); exists != 0 {
		t.Fatal("损坏缓存应被删除，避免永久卡住")
	}
}

func TestRedisPrunesNamespacePrefix(t *testing.T) {
	cache := openTestRedis(t, 1<<20)
	ctx := context.Background()
	key := "itest:ns:" + uuid.NewString()
	t.Cleanup(func() { _ = cache.Delete(context.Background(), key) })

	// 调用方重复带冒号时不应变成 mp:v1::key。
	if err := cache.SetJSON(ctx, "::"+key, 1, time.Minute); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	raw, err := cache.client.Get(ctx, namespaced(key)).Result()
	if err != nil || raw != "1" {
		t.Fatalf("前缀未归一: raw=%q err=%v", raw, err)
	}
	if err := cache.Delete(ctx, "::"+key); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
}

func TestRedisNilSafety(t *testing.T) {
	var empty *Redis
	if err := empty.Ping(context.Background()); err == nil {
		t.Fatal("未初始化的客户端 Ping 应报错")
	}
	if err := empty.Close(); err != nil {
		t.Fatalf("nil 客户端 Close 应返回 nil: %v", err)
	}

	cache := openTestRedis(t, 1<<20)
	if err := cache.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
	// 空键列表是合法的空操作。
	if err := cache.Delete(context.Background()); err != nil {
		t.Fatalf("空删除应返回 nil: %v", err)
	}
}
