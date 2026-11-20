package database

import (
	"context"
	"errors"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
)

// DashboardMediaCounts 是仪表盘统计卡片用的本地计数：媒体条目来自媒体服务器同步
// 缓存（V2 `DashboardChain().media_statistic` 的多库汇总），用户数来自 users 表，
// 「本月新增」来自整理成功快照。
type DashboardMediaCounts struct {
	MovieCount      int64
	TVCount         int64
	EpisodeCount    int64
	UserCount       int64
	MovieCountMonth int64
	TVCountMonth    int64
}

// DashboardMediaCounts 汇总媒体条目与用户数量。剧集集数按缓存里的 seasoninfo
// （每季的分集数组）累加，因此不依赖媒体服务器在线。
func (s *Store) DashboardMediaCounts(ctx context.Context) (DashboardMediaCounts, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return DashboardMediaCounts{}, errors.New("仪表盘统计不可用")
	}
	if ctx == nil {
		return DashboardMediaCounts{}, errors.New("仪表盘统计不可用")
	}
	if err := ctx.Err(); err != nil {
		return DashboardMediaCounts{}, err
	}

	var counts DashboardMediaCounts
	err := s.db.Pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE item_type = $1),
			count(*) FILTER (WHERE item_type = $2),
			coalesce(sum(CASE WHEN item_type = $2 THEN (
				SELECT coalesce(sum(jsonb_array_length(value)), 0)
				FROM jsonb_each(seasoninfo)
			) ELSE 0 END), 0)
		FROM media_server_items`, mediaserver.ItemTypeMovie, mediaserver.ItemTypeTV,
	).Scan(&counts.MovieCount, &counts.TVCount, &counts.EpisodeCount)
	if err != nil {
		return DashboardMediaCounts{}, err
	}
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&counts.UserCount); err != nil {
		return DashboardMediaCounts{}, err
	}
	// 本月新增来自整理成功快照；没有任何整理记录时自然为 0。
	err = s.db.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE media_type IN ('电影', 'movie')),
		       count(*) FILTER (WHERE media_type IN ('电视剧', 'tv'))
		FROM organization_success_snapshots
		WHERE created_at >= date_trunc('month', now())`).Scan(&counts.MovieCountMonth, &counts.TVCountMonth)
	if err != nil {
		return DashboardMediaCounts{}, err
	}
	return counts, nil
}
