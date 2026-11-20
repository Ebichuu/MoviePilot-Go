package database

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const torrentsPrioritySetting = "TorrentsPriority"

// LoadV2CandidateSortInputs returns the persisted inputs consumed by the V2
// torrent ordering step. User-data identity follows V2: history is grouped by
// domain and the resulting upload value is exposed by the stored site name.
func (s *Store) LoadV2CandidateSortInputs(
	ctx context.Context,
) ([]string, map[string]float64, map[int]int, error) {
	var priorityRules []string
	if _, err := s.GetSetting(ctx, torrentsPrioritySetting, &priorityRules); err != nil {
		return nil, nil, nil, fmt.Errorf("读取种子排序规则: %w", err)
	}

	siteOrders, err := s.loadV2SiteOrders(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	siteUploads, err := s.loadV2SiteUploads(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	return priorityRules, siteUploads, siteOrders, nil
}

func (s *Store) loadV2SiteOrders(ctx context.Context) (map[int]int, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, COALESCE(config->>'pri', '')
		FROM sites`)
	if err != nil {
		return nil, fmt.Errorf("读取站点排序优先级: %w", err)
	}
	defer rows.Close()

	orders := make(map[int]int)
	for rows.Next() {
		var id int
		var rawPriority string
		if err := rows.Scan(&id, &rawPriority); err != nil {
			return nil, fmt.Errorf("扫描站点排序优先级: %w", err)
		}
		priority := 0
		if parsed, parseErr := strconv.Atoi(strings.TrimSpace(rawPriority)); parseErr == nil {
			priority = parsed
		}
		orders[id] = priority
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历站点排序优先级: %w", err)
	}
	return orders, nil
}

func (s *Store) loadV2SiteUploads(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT user_data.domain, user_data.name, COALESCE(user_data.upload, 0)
		FROM site_userdata AS user_data
		JOIN (
			SELECT domain, MAX(updated_day) AS latest_update_day
			FROM site_userdata
			WHERE err_msg IS NULL OR err_msg = ''
			GROUP BY domain
		) AS latest_success
			ON user_data.domain = latest_success.domain
			AND user_data.updated_day = latest_success.latest_update_day
		ORDER BY user_data.updated_time DESC`)
	if err != nil {
		return nil, fmt.Errorf("读取站点上传量: %w", err)
	}
	defer rows.Close()

	uploads := make(map[string]float64)
	for rows.Next() {
		var domain, name string
		var upload float64
		if err := rows.Scan(&domain, &name, &upload); err != nil {
			return nil, fmt.Errorf("扫描站点上传量: %w", err)
		}
		uploads[name] = upload
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历站点上传量: %w", err)
	}
	return uploads, nil
}
