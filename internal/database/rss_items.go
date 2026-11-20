package database

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ListPendingRSSItems 按站点、首次发现时间稳定分页，返回 pending 或可重试的
// failed 条目，且不会读取认证字段或 RSS 原文。
func (s *Store) ListPendingRSSItems(ctx context.Context, siteID int64, limit int) ([]RSSItem, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return nil, err
	}
	if siteID < 1 || limit < 1 || limit > maxRSSItemsLimit {
		return nil, ErrInvalidRSSInput
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, site_id, item_key, guid, download_id, download_secret, detail_url, download_url,
			title, recognized_media_title, media_recognize_fail_count, published_at, resource, first_seen_at, last_seen_at, state,
			attempt_count, last_error, matched_at, lease_token, lease_expires_at, updated_at
		FROM site_rss_items
		WHERE site_id=$1 AND state IN ('pending','failed') AND attempt_count < $2
		ORDER BY first_seen_at, id
		LIMIT $3`, siteID, maxRSSAttempts, limit)
	if err != nil {
		return nil, rssPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]RSSItem, 0, limit)
	for rows.Next() {
		item, scanErr := s.scanRSSItem(rows)
		if scanErr != nil {
			return nil, ErrRSSPersistence
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, rssPersistenceError(ctx)
	}
	return items, nil
}

// ClaimRSSItems 原子领取一个站点的 pending/failed 条目。一次调用使用同一
// token，便于 worker 传递租约；并发调用通过 SKIP LOCKED 分片。
func (s *Store) ClaimRSSItems(ctx context.Context, siteID int64, limit int, leaseDuration time.Duration) ([]RSSItem, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return nil, err
	}
	if siteID < 1 || limit < 1 || limit > maxRSSItemsLimit || leaseDuration < time.Second || leaseDuration > time.Hour {
		return nil, ErrInvalidRSSInput
	}
	token := uuid.New()
	expiresAt := time.Now().UTC().Add(leaseDuration)
	rows, err := s.db.Pool.Query(ctx, `
		WITH candidates AS MATERIALIZED (
			SELECT id FROM site_rss_items
			WHERE site_id=$1 AND state IN ('pending','failed')
				AND attempt_count < $2
				AND (lease_expires_at IS NULL OR lease_expires_at <= now())
			ORDER BY first_seen_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE site_rss_items AS item
			SET state='processing', lease_token=$4, lease_expires_at=$5,
				attempt_count=item.attempt_count+1, updated_at=now()
			FROM candidates
			WHERE item.id=candidates.id
			RETURNING item.id, item.site_id, item.item_key, item.guid, item.download_id, item.download_secret,
				item.detail_url, item.download_url, item.title, item.recognized_media_title,
				item.media_recognize_fail_count, item.published_at,
				item.resource, item.first_seen_at, item.last_seen_at, item.state,
				item.attempt_count, item.last_error, item.matched_at,
				item.lease_token, item.lease_expires_at, item.updated_at
		)
		SELECT * FROM claimed ORDER BY first_seen_at, id`, siteID, maxRSSAttempts, limit, token, expiresAt)
	if err != nil {
		return nil, rssPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]RSSItem, 0, limit)
	for rows.Next() {
		item, scanErr := s.scanRSSItem(rows)
		if scanErr != nil {
			return nil, ErrRSSPersistence
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, rssPersistenceError(ctx)
	}
	return items, nil
}

// CompleteRSSItem 在 item 租约仍有效时更新状态。state 只允许 matched、ignored、
// failed 或 pending；失败信息会被截断且去除控制字符。
func (s *Store) CompleteRSSItem(ctx context.Context, itemID int64, token uuid.UUID, state string, stableErr string) error {
	if err := validateRSSStore(s, ctx); err != nil {
		return err
	}
	state = strings.ToLower(strings.TrimSpace(state))
	if itemID < 1 || token == uuid.Nil || (state != "matched" && state != "ignored" && state != "failed" && state != "pending") {
		return ErrInvalidRSSInput
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE site_rss_items
		SET state=$3, matched_at=CASE WHEN $3='matched' THEN now() ELSE matched_at END,
			last_error=$4, lease_token=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND lease_token=$2 AND lease_expires_at > now()`, itemID, token, state, sanitizeRSSError(stableErr))
	if err != nil {
		return rssPersistenceError(ctx)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := s.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM site_rss_items WHERE id=$1)`, itemID).Scan(&exists); err != nil {
		return rssPersistenceError(ctx)
	}
	if !exists {
		return ErrRSSItemNotFound
	}
	return ErrRSSLeaseConflict
}

// UpdateRSSItemMediaRecognition persists the private canonical media title or
// one failed recognition attempt. A failed counter is capped at the V2 limit;
// once the limit is reached this method becomes an idempotent no-op for
// failures, while a later successful recognition may still reset the counter.
func (s *Store) UpdateRSSItemMediaRecognition(ctx context.Context, itemID int64, canonicalTitle string) error {
	if err := validateRSSStore(s, ctx); err != nil {
		return err
	}
	canonicalTitle = strings.TrimSpace(canonicalTitle)
	if itemID < 1 || len(canonicalTitle) > maxRSSTitle {
		return ErrInvalidRSSInput
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE site_rss_items
		SET recognized_media_title=CASE WHEN $2 <> '' THEN $2 ELSE recognized_media_title END,
			media_recognize_fail_count=CASE WHEN $2 <> '' THEN 0
				ELSE LEAST(media_recognize_fail_count+1, $3) END,
			updated_at=now()
		WHERE id=$1`, itemID, canonicalTitle, maxRSSMediaRecognizeFailures)
	if err != nil {
		return rssPersistenceError(ctx)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := s.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM site_rss_items WHERE id=$1)`, itemID).Scan(&exists); err != nil {
		return rssPersistenceError(ctx)
	}
	if !exists {
		return ErrRSSItemNotFound
	}
	return nil
}

// RecoverExpiredRSSLeases 将已过期 processing 条目恢复为 pending，避免进程
// 崩溃后永久卡住。limit 保证单次写入有界。
func (s *Store) RecoverExpiredRSSLeases(ctx context.Context, now time.Time, limit int) (int, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return 0, err
	}
	if now.IsZero() || limit < 1 || limit > maxRSSItemsLimit {
		return 0, ErrInvalidRSSInput
	}
	result, err := s.db.Pool.Exec(ctx, `
		WITH expired AS MATERIALIZED (
			SELECT id FROM site_rss_items
			WHERE state='processing' AND lease_expires_at IS NOT NULL AND lease_expires_at <= $1
			ORDER BY lease_expires_at, id LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE site_rss_items AS item
		SET state='pending', lease_token=NULL, lease_expires_at=NULL,
			last_error='lease expired', updated_at=now()
		FROM expired WHERE item.id=expired.id`, now.UTC(), limit)
	if err != nil {
		return 0, rssPersistenceError(ctx)
	}
	return int(result.RowsAffected()), nil
}

// CleanupProcessedRSSItems 删除有限数量的旧 matched/ignored 条目，防止 RSS
// 表无界增长。调用方可周期性执行；pending/processing/failed 不会被删除。
func (s *Store) CleanupProcessedRSSItems(ctx context.Context, siteID int64, before time.Time, limit int) (int, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return 0, err
	}
	if siteID < 1 || before.IsZero() || limit < 1 || limit > maxRSSItemsLimit {
		return 0, ErrInvalidRSSInput
	}
	result, err := s.db.Pool.Exec(ctx, `
		WITH old_items AS MATERIALIZED (
			SELECT id FROM site_rss_items
			WHERE site_id=$1 AND state IN ('matched','ignored')
				AND COALESCE(matched_at, last_seen_at) < $2
			ORDER BY id LIMIT $3
		)
		DELETE FROM site_rss_items AS item
		USING old_items WHERE item.id=old_items.id`, siteID, before.UTC(), limit)
	if err != nil {
		return 0, rssPersistenceError(ctx)
	}
	return int(result.RowsAffected()), nil
}
