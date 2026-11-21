package database

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TorrentCacheItem 是 `site_rss_items` 关联站点后的只读缓存投影，对应 V2 `/api/v1/torrent/cache`。
type TorrentCacheItem struct {
	ID                   int64
	SiteID               int64
	SiteName             string
	Domain               string
	Hash                 string
	ItemKey              string
	Title                string
	Description          string
	SizeBytes            int64
	PublishedAt          time.Time
	PageURL              string
	Enclosure            string
	RecognizedMediaTitle string
	Resource             map[string]any
}

// TorrentCacheHash 计算与 V2 `HashUtils.md5(f"{title}{description}")` 一致的缓存项哈希。
func TorrentCacheHash(title, description string) string {
	sum := md5.Sum([]byte(title + description))
	return hex.EncodeToString(sum[:])
}

// ListTorrentCache 返回最近缓存的站点种子条目（按 last_seen_at 倒序，上限 1000 条）。
func (s *Store) ListTorrentCache(ctx context.Context, limit int) ([]TorrentCacheItem, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return []TorrentCacheItem{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT
			i.id,
			i.site_id,
			COALESCE(st.name, '') AS site_name,
			COALESCE(NULLIF(st.domain, ''), st.url, '') AS domain,
			i.item_key,
			i.title,
			i.detail_url,
			i.download_url,
			i.published_at,
			i.recognized_media_title,
			i.resource
		FROM site_rss_items AS i
		LEFT JOIN sites AS st ON st.id = i.site_id
		ORDER BY i.last_seen_at DESC, i.id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询种子缓存: %w", err)
	}
	defer rows.Close()

	items := make([]TorrentCacheItem, 0, min(limit, 128))
	for rows.Next() {
		var (
			item        TorrentCacheItem
			publishedAt *time.Time
			rawResource []byte
		)
		if err := rows.Scan(
			&item.ID, &item.SiteID, &item.SiteName, &item.Domain,
			&item.ItemKey, &item.Title, &item.PageURL, &item.Enclosure,
			&publishedAt, &item.RecognizedMediaTitle, &rawResource,
		); err != nil {
			return nil, fmt.Errorf("读取种子缓存: %w", err)
		}
		if publishedAt != nil {
			item.PublishedAt = publishedAt.UTC()
		}
		if len(rawResource) > 0 {
			var res map[string]any
			if json.Unmarshal(rawResource, &res) == nil {
				item.Resource = res
				if desc, ok := res["description"].(string); ok {
					item.Description = desc
				}
				switch v := res["size_bytes"].(type) {
				case float64:
					item.SizeBytes = int64(v)
				}
			}
		}
		item.Hash = TorrentCacheHash(item.Title, item.Description)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取种子缓存: %w", err)
	}
	return items, nil
}

// ClearTorrentCache 清空所有非 processing 状态的 RSS 种子缓存条目。
func (s *Store) ClearTorrentCache(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return 0, nil
	}
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM site_rss_items WHERE state <> 'processing'`)
	if err != nil {
		return 0, fmt.Errorf("清空种子缓存: %w", err)
	}
	return tag.RowsAffected(), nil
}

// FindTorrentCacheItem 按站点 domain 与 V2 MD5 哈希（或 item_key）查找缓存条目。
func (s *Store) FindTorrentCacheItem(ctx context.Context, domain, torrentHash string) (TorrentCacheItem, bool, error) {
	domain = strings.TrimSpace(domain)
	torrentHash = strings.TrimSpace(torrentHash)
	if domain == "" || torrentHash == "" {
		return TorrentCacheItem{}, false, nil
	}
	items, err := s.ListTorrentCache(ctx, 1000)
	if err != nil {
		return TorrentCacheItem{}, false, err
	}
	for _, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Domain), domain) {
			continue
		}
		if strings.EqualFold(item.Hash, torrentHash) || item.ItemKey == torrentHash {
			return item, true, nil
		}
	}
	return TorrentCacheItem{}, false, nil
}

// DeleteTorrentCacheItem 删除指定站点与哈希对应的单条种子缓存。
func (s *Store) DeleteTorrentCacheItem(ctx context.Context, domain, torrentHash string) (bool, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return false, nil
	}
	item, found, err := s.FindTorrentCacheItem(ctx, domain, torrentHash)
	if err != nil || !found {
		return false, err
	}
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM site_rss_items WHERE id = $1 AND state <> 'processing'`, item.ID)
	if err != nil {
		return false, fmt.Errorf("删除种子缓存: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateTorrentCacheRecognition 更新缓存条目的识别标题并重置为 pending 以便重新匹配订阅。
func (s *Store) UpdateTorrentCacheRecognition(ctx context.Context, id int64, recognizedTitle string, failCount int) error {
	if s == nil || s.db == nil || s.db.Pool == nil || id <= 0 {
		return nil
	}
	if failCount < 0 {
		failCount = 0
	}
	if failCount > 3 {
		failCount = 3
	}
	_, err := s.db.Pool.Exec(ctx, `
		UPDATE site_rss_items
		SET recognized_media_title = $2,
		    media_recognize_fail_count = $3,
		    state = CASE WHEN state = 'processing' THEN state ELSE 'pending' END,
		    updated_at = now()
		WHERE id = $1`, id, strings.TrimSpace(recognizedTitle), failCount)
	if err != nil {
		return fmt.Errorf("更新种子缓存识别结果: %w", err)
	}
	return nil
}
