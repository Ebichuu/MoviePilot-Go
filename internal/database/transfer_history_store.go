package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// TransferHistoryEntry 是「整理成功快照」的只读投影，对应 V2 TransferHistory 的展示字段。
// 快照表本身是组织流程的幂等事实表，这里只读，不参与写入。
type TransferHistoryEntry struct {
	BatchID     uuid.UUID
	GroupKey    string
	Username    string
	Manual      bool
	MediaSource string
	MediaID     string
	TitleYear   string
	MediaType   string
	Category    string
	PosterURL   string
	BackdropURL string
	FileFacts   []organization.FileFact
	CreatedAt   time.Time
}

// ListTransferHistory 按时间倒序返回整理成功记录；username 为空表示不过滤用户，
// title 非空时按标题做不区分大小写的模糊匹配，对应 V2 `GET /history/transfer`。
func (s *Store) ListTransferHistory(ctx context.Context, username, title string, limit, offset int) ([]TransferHistoryEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	username = strings.TrimSpace(username)
	pattern := "%" + strings.TrimSpace(title) + "%"
	rows, err := s.db.Pool.Query(ctx, `
		SELECT batch_id, group_key, username, manual, media_source, media_id, title_year,
		       media_type, category, poster_url, backdrop_url, file_facts, created_at
		FROM organization_success_snapshots
		WHERE ($1 = '' OR username = $1)
		  AND ($2 = '%%' OR title_year ILIKE $2)
		ORDER BY created_at DESC, batch_id DESC, group_key
		LIMIT $3 OFFSET $4`, username, pattern, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("查询整理历史: %w", err)
	}
	defer rows.Close()

	entries := make([]TransferHistoryEntry, 0, limit)
	for rows.Next() {
		var entry TransferHistoryEntry
		var facts []byte
		if err := rows.Scan(&entry.BatchID, &entry.GroupKey, &entry.Username, &entry.Manual,
			&entry.MediaSource, &entry.MediaID, &entry.TitleYear, &entry.MediaType,
			&entry.Category, &entry.PosterURL, &entry.BackdropURL, &facts, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("读取整理历史: %w", err)
		}
		if len(facts) > 0 {
			if err := json.Unmarshal(facts, &entry.FileFacts); err != nil {
				return nil, fmt.Errorf("解析整理历史文件: %w", err)
			}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取整理历史: %w", err)
	}
	return entries, nil
}

// CountTransferHistory 返回与 ListTransferHistory 相同过滤条件下的总条数。
func (s *Store) CountTransferHistory(ctx context.Context, username, title string) (int, error) {
	pattern := "%" + strings.TrimSpace(title) + "%"
	var total int
	err := s.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM organization_success_snapshots
		WHERE ($1 = '' OR username = $1)
		  AND ($2 = '%%' OR title_year ILIKE $2)`, strings.TrimSpace(username), pattern).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("统计整理历史: %w", err)
	}
	return total, nil
}

// CountTransferHistoryByDay 返回最近 days 天每天的整理数量，下标 0 是最早的一天，
// 与 V2 `GET /dashboard/transfer?days=N` 的 `List[int]` 顺序一致。
func (s *Store) CountTransferHistoryByDay(ctx context.Context, days int) ([]int, error) {
	if days <= 0 || days > 31 {
		days = 7
	}
	counts := make([]int, days)
	rows, err := s.db.Pool.Query(ctx, `
		SELECT (date_trunc('day', created_at))::date AS day, count(*)
		FROM organization_success_snapshots
		WHERE created_at >= date_trunc('day', now()) - ($1::int - 1) * interval '1 day'
		GROUP BY day`, days)
	if err != nil {
		return nil, fmt.Errorf("统计整理历史: %w", err)
	}
	defer rows.Close()

	byDay := make(map[string]int, days)
	for rows.Next() {
		var day time.Time
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			return nil, fmt.Errorf("读取整理历史统计: %w", err)
		}
		byDay[day.Format("2006-01-02")] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取整理历史统计: %w", err)
	}

	today := time.Now()
	for index := 0; index < days; index++ {
		day := today.AddDate(0, 0, index-(days-1)).Format("2006-01-02")
		counts[index] = byDay[day]
	}
	return counts, nil
}

// CountTransferHistoryMonth 返回本月整理入库的电影与电视剧数量，对应仪表盘
// `movie_count_month` / `tv_count_month`。
func (s *Store) CountTransferHistoryMonth(ctx context.Context) (int, int, error) {
	var movies, shows int
	err := s.db.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE media_type IN ('电影', 'movie')),
		       count(*) FILTER (WHERE media_type IN ('电视剧', 'tv'))
		FROM organization_success_snapshots
		WHERE created_at >= date_trunc('month', now())`).Scan(&movies, &shows)
	if err != nil {
		return 0, 0, fmt.Errorf("统计本月新增: %w", err)
	}
	return movies, shows, nil
}
