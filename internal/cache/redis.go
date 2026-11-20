package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

const namespace = "mp:v1:"

// Redis 是仅用于可丢弃数据的版本化缓存。
type Redis struct {
	client        *redis.Client
	maxValueBytes int
}

// Open 创建带连接池上限的 Redis 客户端。
func Open(ctx context.Context, cfg config.Config) (*Redis, error) {
	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("解析 Redis 地址: %w", err)
	}
	options.PoolSize = cfg.RedisPoolSize
	options.MinIdleConns = 1
	options.ConnMaxIdleTime = 5 * time.Minute
	options.ConnMaxLifetime = 30 * time.Minute
	options.DialTimeout = 5 * time.Second
	options.ReadTimeout = 3 * time.Second
	options.WriteTimeout = 3 * time.Second
	options.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("连接 Redis: %w", err)
	}
	return &Redis{client: client, maxValueBytes: cfg.MaxCacheValue}, nil
}

// Close 释放 Redis 连接池。
func (r *Redis) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

// Ping 检查 Redis 是否可用。
func (r *Redis) Ping(ctx context.Context) error {
	if r == nil || r.client == nil {
		return errors.New("Redis 客户端未初始化")
	}
	return r.client.Ping(ctx).Err()
}

// GetJSON 将缓存 JSON 解码到目标对象；缓存不存在时返回 false。
func (r *Redis) GetJSON(ctx context.Context, key string, target any) (bool, error) {
	value, err := r.client.Get(ctx, namespaced(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(value) > r.maxValueBytes {
		_ = r.client.Del(ctx, namespaced(key)).Err()
		return false, fmt.Errorf("缓存值超过 %d 字节限制", r.maxValueBytes)
	}
	if err := json.Unmarshal(value, target); err != nil {
		_ = r.client.Del(ctx, namespaced(key)).Err()
		return false, fmt.Errorf("缓存 JSON 损坏: %w", err)
	}
	return true, nil
}

// SetJSON 写入带强制 TTL 和抖动的 JSON 缓存。
func (r *Redis) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("缓存 TTL 必须为正数")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("编码缓存 JSON: %w", err)
	}
	if len(encoded) > r.maxValueBytes {
		return fmt.Errorf("缓存值大小 %d 超过 %d 字节限制", len(encoded), r.maxValueBytes)
	}
	return r.client.Set(ctx, namespaced(key), encoded, jitter(ttl)).Err()
}

// Delete 删除指定缓存键，不提供全库清空能力。
func (r *Redis) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	qualified := make([]string, 0, len(keys))
	for _, key := range keys {
		qualified = append(qualified, namespaced(key))
	}
	return r.client.Del(ctx, qualified...).Err()
}

func namespaced(key string) string {
	return namespace + strings.TrimLeft(key, ":")
}

func jitter(ttl time.Duration) time.Duration {
	span := ttl / 10
	if span > 30*time.Second {
		span = 30 * time.Second
	}
	if span <= 0 {
		return ttl
	}
	return ttl + time.Duration(time.Now().UnixNano()%int64(span))
}
