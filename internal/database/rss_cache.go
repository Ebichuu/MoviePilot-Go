package database

import (
	"context"
	"sort"
	"time"
)

// ListRSSCacheSignatures 返回站点刷新前最近 100 条缓存的严格 V2 签名。
// 签名是持久化 title 与 resource.description 的直接拼接，不做归一化。
func (s *Store) ListRSSCacheSignatures(ctx context.Context, siteID int64) (map[string]struct{}, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return nil, err
	}
	if siteID < 1 {
		return nil, ErrInvalidRSSInput
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT title, COALESCE(resource->>'description', '')
		FROM site_rss_items
		WHERE site_id=$1
		ORDER BY id DESC
		LIMIT $2`, siteID, maxRSSCacheItems)
	if err != nil {
		return nil, rssPersistenceError(ctx)
	}
	defer rows.Close()
	signatures := make(map[string]struct{}, maxRSSCacheItems)
	for rows.Next() {
		var title, description string
		if err := rows.Scan(&title, &description); err != nil {
			return nil, ErrRSSPersistence
		}
		signatures[rssCacheSignature(title, description)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, rssPersistenceError(ctx)
	}
	return signatures, nil
}

// ListRSSCacheSnapshot 返回站点在 cutoff 时已经存在的最近 RSS 缓存。
// 返回数量固定不超过 V2 普通模式的 100 条缓存上限，且不会按处理状态过滤。
func (s *Store) ListRSSCacheSnapshot(ctx context.Context, siteID int64, cutoff time.Time) ([]RSSItem, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return nil, err
	}
	if siteID < 1 || cutoff.IsZero() {
		return nil, ErrInvalidRSSInput
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, site_id, item_key, guid, download_id, download_secret, detail_url, download_url,
			title, recognized_media_title, media_recognize_fail_count, published_at, resource, first_seen_at, last_seen_at, state,
			attempt_count, last_error, matched_at, lease_token, lease_expires_at, updated_at
		FROM (
			SELECT *
			FROM site_rss_items
			WHERE site_id=$1 AND first_seen_at <= $2
			ORDER BY id DESC
			LIMIT $3
		) AS retained
		ORDER BY id`, siteID, cutoff.UTC(), maxRSSCacheItems)
	if err != nil {
		return nil, rssPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]RSSItem, 0, maxRSSCacheItems)
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

// RSSPollRoundReady 报告本轮预期站点是否都已经完成一次轮询尝试。
// 成功和失败都会把 next_poll_at 推进到 cutoff 之后并清除租约。
func (s *Store) RSSPollRoundReady(ctx context.Context, cutoff time.Time, expectedSiteIDs []int64) (bool, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return false, err
	}
	if cutoff.IsZero() {
		return false, ErrInvalidRSSInput
	}
	siteIDs, err := normalizeRSSRoundSiteIDs(expectedSiteIDs)
	if err != nil {
		return false, err
	}
	if len(siteIDs) == 0 {
		return true, nil
	}
	var ready bool
	if err := s.db.Pool.QueryRow(ctx, `
		SELECT count(*) = $3
		FROM site_rss_cursors
		WHERE site_id = ANY($2::bigint[])
		  AND next_poll_at > $1
		  AND lease_token IS NULL
		  AND lease_expires_at IS NULL`, cutoff.UTC(), siteIDs, len(siteIDs)).Scan(&ready); err != nil {
		return false, rssPersistenceError(ctx)
	}
	return ready, nil
}

// TrimRSSCacheItems 将一个站点的缓存收敛到最近 100 条。limit 只限制本次
// 删除数量；仍在 processing 的遗留租约不会被删除。
func (s *Store) TrimRSSCacheItems(ctx context.Context, siteID int64, limit int) (int, error) {
	if err := validateRSSStore(s, ctx); err != nil {
		return 0, err
	}
	if siteID < 1 || limit < 1 || limit > maxRSSItemsLimit {
		return 0, ErrInvalidRSSInput
	}
	result, err := s.db.Pool.Exec(ctx, `
		WITH retained AS MATERIALIZED (
			SELECT id
			FROM site_rss_items
			WHERE site_id=$1
			ORDER BY id DESC
			LIMIT $2
		), old_items AS MATERIALIZED (
			SELECT item.id
			FROM site_rss_items AS item
			WHERE item.site_id=$1
			  AND item.state <> 'processing'
			  AND NOT EXISTS (SELECT 1 FROM retained WHERE retained.id=item.id)
			ORDER BY item.id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM site_rss_items AS item
		USING old_items
		WHERE item.id=old_items.id`, siteID, maxRSSCacheItems, limit)
	if err != nil {
		return 0, rssPersistenceError(ctx)
	}
	return int(result.RowsAffected()), nil
}

func normalizeRSSRoundSiteIDs(siteIDs []int64) ([]int64, error) {
	if len(siteIDs) > maxRSSRoundSites {
		return nil, ErrInvalidRSSInput
	}
	unique := make(map[int64]struct{}, len(siteIDs))
	for _, siteID := range siteIDs {
		if siteID < 1 {
			return nil, ErrInvalidRSSInput
		}
		unique[siteID] = struct{}{}
	}
	normalized := make([]int64, 0, len(unique))
	for siteID := range unique {
		normalized = append(normalized, siteID)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	return normalized, nil
}
