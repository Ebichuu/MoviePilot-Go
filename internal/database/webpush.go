package database

import (
	"context"
	"fmt"
	"strings"
)

// WebPushSubscription 是一个浏览器推送订阅（V2 WebPush 订阅数据）。
type WebPushSubscription struct {
	Endpoint  string
	Username  string
	P256dh    string
	Auth      string
	UserAgent string
}

// UpsertWebPushSubscription 以 endpoint 为主键新增或更新订阅。
func (s *Store) UpsertWebPushSubscription(ctx context.Context, subscription WebPushSubscription) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("webpush 订阅存储未初始化")
	}
	endpoint := strings.TrimSpace(subscription.Endpoint)
	p256dh := strings.TrimSpace(subscription.P256dh)
	auth := strings.TrimSpace(subscription.Auth)
	if endpoint == "" || p256dh == "" || auth == "" {
		return fmt.Errorf("webpush 订阅参数无效")
	}
	if len(endpoint) > 4096 || len(p256dh) > 256 || len(auth) > 256 {
		return fmt.Errorf("webpush 订阅参数超限")
	}
	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO webpush_subscriptions (endpoint, username, p256dh, auth, user_agent, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (endpoint) DO UPDATE SET
			username = EXCLUDED.username,
			p256dh = EXCLUDED.p256dh,
			auth = EXCLUDED.auth,
			user_agent = EXCLUDED.user_agent,
			updated_at = now()`,
		endpoint, strings.TrimSpace(subscription.Username), p256dh, auth, strings.TrimSpace(subscription.UserAgent))
	if err != nil {
		return fmt.Errorf("保存 webpush 订阅: %w", err)
	}
	return nil
}

// ListWebPushSubscriptions 返回全部有效订阅，按 endpoint 稳定排序。
func (s *Store) ListWebPushSubscriptions(ctx context.Context) ([]WebPushSubscription, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("webpush 订阅存储未初始化")
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT endpoint, username, p256dh, auth, user_agent
		FROM webpush_subscriptions ORDER BY endpoint`)
	if err != nil {
		return nil, fmt.Errorf("查询 webpush 订阅: %w", err)
	}
	defer rows.Close()
	subscriptions := make([]WebPushSubscription, 0)
	for rows.Next() {
		var subscription WebPushSubscription
		if err := rows.Scan(
			&subscription.Endpoint, &subscription.Username, &subscription.P256dh,
			&subscription.Auth, &subscription.UserAgent,
		); err != nil {
			return nil, fmt.Errorf("解析 webpush 订阅: %w", err)
		}
		subscriptions = append(subscriptions, subscription)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 webpush 订阅: %w", err)
	}
	return subscriptions, nil
}

// DeleteWebPushSubscription 移除失效订阅，返回是否确实删除。
func (s *Store) DeleteWebPushSubscription(ctx context.Context, endpoint string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("webpush 订阅存储未初始化")
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return false, fmt.Errorf("webpush 订阅参数无效")
	}
	result, err := s.db.Pool.Exec(ctx, `DELETE FROM webpush_subscriptions WHERE endpoint=$1`, endpoint)
	if err != nil {
		return false, fmt.Errorf("删除 webpush 订阅: %w", err)
	}
	return result.RowsAffected() > 0, nil
}
