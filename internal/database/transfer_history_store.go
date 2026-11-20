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

// TransferHistoryEntry 是整理历史（包含成功快照与失败文件记录）的只读投影，
// 对应 V2 `schemas.TransferHistory` 的展示字段。
type TransferHistoryEntry struct {
	FileID      int64
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
	SourcePath  string
	TargetPath  string
	Mode        string
	Failed      bool
	ErrMsg      string
	CreatedAt   time.Time
}

// TransferHistoryFilter 控制整理历史的分页、关键词与成功/失败筛选。
type TransferHistoryFilter struct {
	Username string
	Title    string
	Status   *bool
	Limit    int
	Offset   int
}

// ListTransferHistory 按时间倒序返回整理成功记录；保留原有签名的向后兼容入口。
func (s *Store) ListTransferHistory(ctx context.Context, username, title string, limit, offset int) ([]TransferHistoryEntry, error) {
	ok := true
	return s.ListTransferHistoryFiltered(ctx, TransferHistoryFilter{
		Username: username,
		Title:    title,
		Status:   &ok,
		Limit:    limit,
		Offset:   offset,
	})
}

// CountTransferHistory 返回整理成功记录的总条数；保留向后兼容入口。
func (s *Store) CountTransferHistory(ctx context.Context, username, title string) (int, error) {
	ok := true
	return s.CountTransferHistoryFiltered(ctx, TransferHistoryFilter{
		Username: username,
		Title:    title,
		Status:   &ok,
	})
}

// ListTransferHistoryFiltered 返回按条件筛选的整理历史（支持成功、失败与全部）。
func (s *Store) ListTransferHistoryFiltered(ctx context.Context, filter TransferHistoryFilter) ([]TransferHistoryEntry, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return []TransferHistoryEntry{}, nil
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	username := strings.TrimSpace(filter.Username)
	pattern := "%" + strings.TrimSpace(filter.Title) + "%"
	includeSuccess := filter.Status == nil || *filter.Status
	includeFailed := filter.Status == nil || !*filter.Status

	rows, err := s.db.Pool.Query(ctx, `
		WITH combined AS (
			SELECT
				0::bigint AS file_id,
				s.batch_id,
				s.group_key,
				s.username,
				s.manual,
				s.media_source,
				s.media_id,
				s.title_year,
				s.media_type,
				s.category,
				s.poster_url,
				s.backdrop_url,
				s.file_facts,
				COALESCE(f.source_path, '') AS source_path,
				COALESCE(f.target_path, '') AS target_path,
				COALESCE(f.mode, '') AS mode,
				FALSE AS failed,
				''::text AS err_msg,
				s.created_at
			FROM organization_success_snapshots AS s
			LEFT JOIN LATERAL (
				SELECT source_path, target_path, mode
				FROM organization_files
				WHERE batch_id = s.batch_id
				ORDER BY ordinal ASC, id ASC
				LIMIT 1
			) AS f ON TRUE
			WHERE $1::bool
			  AND ($3 = '' OR s.username = $3)
			  AND ($4 = '%%' OR s.title_year ILIKE $4 OR COALESCE(f.source_path, '') ILIKE $4)
			UNION ALL
			SELECT
				f.id AS file_id,
				f.batch_id,
				'failed:' || f.id::text AS group_key,
				''::text AS username,
				FALSE AS manual,
				''::text AS media_source,
				''::text AS media_id,
				f.source_path AS title_year,
				''::text AS media_type,
				''::text AS category,
				''::text AS poster_url,
				''::text AS backdrop_url,
				'[]'::jsonb AS file_facts,
				f.source_path,
				f.target_path,
				f.mode,
				TRUE AS failed,
				f.last_error AS err_msg,
				COALESCE(f.completed_at, f.updated_at, f.created_at) AS created_at
			FROM organization_files AS f
			WHERE $2::bool
			  AND f.state IN ('failed', 'terminal_failed')
			  AND ($3 = '')
			  AND ($4 = '%%' OR f.source_path ILIKE $4 OR f.target_path ILIKE $4 OR f.last_error ILIKE $4)
		)
		SELECT file_id, batch_id, group_key, username, manual, media_source, media_id, title_year,
		       media_type, category, poster_url, backdrop_url, file_facts,
		       source_path, target_path, mode, failed, err_msg, created_at
		FROM combined
		ORDER BY created_at DESC, batch_id DESC, group_key DESC
		LIMIT $5 OFFSET $6`,
		includeSuccess, includeFailed, username, pattern, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("查询整理历史: %w", err)
	}
	defer rows.Close()

	entries := make([]TransferHistoryEntry, 0, limit)
	for rows.Next() {
		var entry TransferHistoryEntry
		var facts []byte
		if err := rows.Scan(
			&entry.FileID, &entry.BatchID, &entry.GroupKey, &entry.Username, &entry.Manual,
			&entry.MediaSource, &entry.MediaID, &entry.TitleYear, &entry.MediaType,
			&entry.Category, &entry.PosterURL, &entry.BackdropURL, &facts,
			&entry.SourcePath, &entry.TargetPath, &entry.Mode, &entry.Failed, &entry.ErrMsg, &entry.CreatedAt,
		); err != nil {
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

// CountTransferHistoryFiltered 返回与 ListTransferHistoryFiltered 相同条件下的总条数。
func (s *Store) CountTransferHistoryFiltered(ctx context.Context, filter TransferHistoryFilter) (int, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return 0, nil
	}
	username := strings.TrimSpace(filter.Username)
	pattern := "%" + strings.TrimSpace(filter.Title) + "%"
	includeSuccess := filter.Status == nil || *filter.Status
	includeFailed := filter.Status == nil || !*filter.Status

	var total int
	err := s.db.Pool.QueryRow(ctx, `
		SELECT
			(CASE WHEN $1::bool THEN (
				SELECT count(*)
				FROM organization_success_snapshots AS s
				LEFT JOIN LATERAL (
					SELECT source_path
					FROM organization_files
					WHERE batch_id = s.batch_id
					ORDER BY ordinal ASC, id ASC
					LIMIT 1
				) AS f ON TRUE
				WHERE ($3 = '' OR s.username = $3)
				  AND ($4 = '%%' OR s.title_year ILIKE $4 OR COALESCE(f.source_path, '') ILIKE $4)
			) ELSE 0 END)
			+
			(CASE WHEN $2::bool AND $3 = '' THEN (
				SELECT count(*)
				FROM organization_files AS f
				WHERE f.state IN ('failed', 'terminal_failed')
				  AND ($4 = '%%' OR f.source_path ILIKE $4 OR f.target_path ILIKE $4 OR f.last_error ILIKE $4)
			) ELSE 0 END)`,
		includeSuccess, includeFailed, username, pattern).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("统计整理历史: %w", err)
	}
	return total, nil
}

// DeleteFailedTransferHistory 删除单条失败的整理文件记录（不影响幂等成功快照）。
func (s *Store) DeleteFailedTransferHistory(ctx context.Context, fileID int64) (bool, error) {
	if s == nil || s.db == nil || s.db.Pool == nil || fileID <= 0 {
		return false, nil
	}
	tag, err := s.db.Pool.Exec(ctx, `
		DELETE FROM organization_files
		WHERE id = $1 AND state IN ('failed', 'terminal_failed')`, fileID)
	if err != nil {
		return false, fmt.Errorf("删除失败整理记录: %w", err)
	}
	return tag.RowsAffected() > 0, nil
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

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for offset := 0; offset < days; offset++ {
		day := today.AddDate(0, 0, -(days - 1 - offset)).Format("2006-01-02")
		counts[offset] = byDay[day]
	}
	return counts, nil
}
