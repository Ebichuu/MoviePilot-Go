package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// NotificationTemplatesSettingKey is the V2 system setting for business
// notification templates.
const NotificationTemplatesSettingKey = "NotificationTemplates"

// NotificationSendTimeSettingKey is the V2 application setting for the
// in-process notification send-time queue.
const NotificationSendTimeSettingKey = "NotificationSendTime"

// GetSetting 读取 JSON 设置；不存在时返回 false。
func (s *Store) GetSetting(ctx context.Context, key string, target any) (bool, error) {
	var value []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取设置: %w", err)
	}
	if err := json.Unmarshal(value, target); err != nil {
		return false, fmt.Errorf("解析设置: %w", err)
	}
	return true, nil
}

// SetSetting 在事务内覆盖指定 JSON 设置。
func (s *Store) SetSetting(ctx context.Context, key string, value any, encrypted bool) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("编码设置: %w", err)
	}
	_, err = s.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value, encrypted, updated_at)
		VALUES ($1, $2::jsonb, $3, now())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, encrypted=EXCLUDED.encrypted, updated_at=now()`,
		key, encoded, encrypted)
	if err != nil {
		return fmt.Errorf("保存设置: %w", err)
	}
	return nil
}
