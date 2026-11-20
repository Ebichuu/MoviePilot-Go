package database

import (
	"context"
	"errors"
	"strings"
)

const maxDownloadIntentOwnerLabels = 1000

var (
	// ErrInvalidDownloadIntentOwnerLookup 表示 owner 查询包含无效或过多的意图标签。
	ErrInvalidDownloadIntentOwnerLookup = errors.New("下载意图所有者查询无效")
	// ErrDownloadIntentOwnerLookup 表示数据库暂时无法安全查询意图所有者。
	ErrDownloadIntentOwnerLookup = errors.New("查询下载意图所有者失败")
)

// DownloadIntentOwner 是下载列表向 V2 用户边界投影所需的最小所有者信息。
type DownloadIntentOwner struct {
	UserID   int64
	Username string
}

const listDownloadIntentOwnersSQL = `
	SELECT di.label, u.id, di.username
	FROM download_intents AS di
	JOIN users AS u ON u.username=di.username
	WHERE di.label=ANY($1::text[])`

// ListDownloadIntentOwners 批量返回规范意图标签对应的用户，避免下载列表逐任务查询。
func (s *Store) ListDownloadIntentOwners(ctx context.Context, labels []string) (map[string]DownloadIntentOwner, error) {
	normalized, err := normalizeDownloadIntentOwnerLabels(labels)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]DownloadIntentOwner, len(normalized))
	if len(normalized) == 0 {
		return owners, nil
	}
	if ctx == nil || !downloadIntentStoreReady(s) {
		return nil, ErrDownloadIntentOwnerLookup
	}
	rows, err := s.db.Pool.Query(ctx, listDownloadIntentOwnersSQL, normalized)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrDownloadIntentOwnerLookup
	}
	defer rows.Close()
	for rows.Next() {
		var (
			label string
			owner DownloadIntentOwner
		)
		if err := rows.Scan(&label, &owner.UserID, &owner.Username); err != nil {
			return nil, ErrDownloadIntentOwnerLookup
		}
		owners[label] = owner
	}
	if err := rows.Err(); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrDownloadIntentOwnerLookup
	}
	return owners, nil
}

func normalizeDownloadIntentOwnerLabels(labels []string) ([]string, error) {
	if len(labels) > maxDownloadIntentOwnerLabels {
		return nil, ErrInvalidDownloadIntentOwnerLookup
	}
	normalized := make([]string, 0, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for _, raw := range labels {
		label := strings.TrimSpace(raw)
		if !validDownloadIntentOpaqueHex(label, "mp-", 32) {
			return nil, ErrInvalidDownloadIntentOwnerLookup
		}
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		normalized = append(normalized, label)
	}
	return normalized, nil
}
