package database

import (
	"context"
)

// SubscriptionDownloadFile 是某条订阅对应的下载文件。
type SubscriptionDownloadFile struct {
	Path        string
	SizeBytes   int64
	Selected    bool
	TorrentHash string
	IntentState string
}

// ListSubscriptionDownloadFiles 按订阅列出已登记的下载文件（来自 download_files 与 download_intents）。
func (s *Store) ListSubscriptionDownloadFiles(ctx context.Context, subscriptionID int64) ([]SubscriptionDownloadFile, error) {
	files := []SubscriptionDownloadFile{}
	if s == nil || s.db == nil || s.db.Pool == nil || subscriptionID <= 0 {
		return files, nil
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT f.path, COALESCE(f.size_bytes, 0), f.selected,
		       COALESCE(i.torrent_hash, ''), i.state
		FROM download_files f
		JOIN download_intents i ON i.id = f.intent_id
		WHERE i.subscription_id = $1
		ORDER BY f.id DESC`, subscriptionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var file SubscriptionDownloadFile
		if scanErr := rows.Scan(&file.Path, &file.SizeBytes, &file.Selected, &file.TorrentHash, &file.IntentState); scanErr != nil {
			return nil, scanErr
		}
		files = append(files, file)
	}
	return files, rows.Err()
}
