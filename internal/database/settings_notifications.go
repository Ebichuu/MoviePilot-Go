package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

// NotificationSettingsKey 是 MoviePilot V2 通知配置使用的系统设置键。
const NotificationSettingsKey = "Notifications"

// LoadNotificationSettings 返回解密后的完整通知配置 JSON 值。
// 旧明文记录会在首次读取时升级为密文。
func (s *Store) LoadNotificationSettings(ctx context.Context) (json.RawMessage, bool, error) {
	value, encrypted, found, err := s.loadNotificationSetting(ctx)
	if err != nil || !found {
		return nil, found, err
	}
	if !encrypted {
		if _, err := s.upgradeLegacyNotificationSetting(ctx, value); err != nil {
			return nil, false, err
		}
	}
	return append(json.RawMessage(nil), value...), true, nil
}

// SaveNotificationSettings 加密并覆盖完整通知配置 JSON 值。
func (s *Store) SaveNotificationSettings(ctx context.Context, value json.RawMessage) error {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return errors.New("通知设置存储不可用")
	}

	storedValue, err := encodeNotificationSetting(s.secretBox, value)
	if err != nil {
		return err
	}
	_, err = s.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value, encrypted, updated_at)
		VALUES ($1, $2::jsonb, TRUE, now())
		ON CONFLICT (key) DO UPDATE
		SET value=EXCLUDED.value, encrypted=TRUE, updated_at=now()`,
		NotificationSettingsKey, storedValue)
	if err != nil {
		return fmt.Errorf("保存通知设置: %w", err)
	}
	return nil
}

func (s *Store) upgradeLegacyNotificationSetting(
	ctx context.Context,
	previousValue json.RawMessage,
) (bool, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return false, errors.New("通知设置存储不可用")
	}

	storedValue, err := encodeNotificationSetting(s.secretBox, previousValue)
	if err != nil {
		return false, err
	}
	commandTag, err := s.db.Pool.Exec(ctx, `
		UPDATE settings
		SET value=$3::jsonb, encrypted=TRUE, updated_at=now()
		WHERE key=$1 AND encrypted=FALSE AND value=$2::jsonb`,
		NotificationSettingsKey, previousValue, storedValue)
	if err != nil {
		return false, fmt.Errorf("保存通知设置: %w", err)
	}
	return commandTag.RowsAffected() > 0, nil
}

func (s *Store) loadNotificationSetting(
	ctx context.Context,
) (value json.RawMessage, encrypted bool, found bool, err error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil, false, false, errors.New("通知设置存储不可用")
	}

	var storedValue []byte
	err = s.db.Pool.QueryRow(ctx, `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, NotificationSettingsKey).Scan(&storedValue, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("读取通知设置: %w", err)
	}

	plainValue, err := decodeNotificationSetting(s.secretBox, storedValue, encrypted)
	if err != nil {
		return nil, false, false, err
	}
	return plainValue, encrypted, true, nil
}

func encodeNotificationSetting(secretBox *auth.SecretBox, value json.RawMessage) (json.RawMessage, error) {
	if secretBox == nil {
		return nil, errors.New("通知设置加密器不可用")
	}
	if !json.Valid(value) {
		return nil, errors.New("通知设置不是有效 JSON")
	}

	cipherText, err := secretBox.Encrypt(string(value))
	if err != nil {
		return nil, fmt.Errorf("加密通知设置: %w", err)
	}
	encoded, err := json.Marshal(cipherText)
	if err != nil {
		return nil, errors.New("编码通知设置密文失败")
	}
	return encoded, nil
}

func decodeNotificationSetting(
	secretBox *auth.SecretBox,
	storedValue json.RawMessage,
	encrypted bool,
) (json.RawMessage, error) {
	if !encrypted {
		if !json.Valid(storedValue) {
			return nil, errors.New("通知设置不是有效 JSON")
		}
		return append(json.RawMessage(nil), storedValue...), nil
	}
	if secretBox == nil {
		return nil, errors.New("通知设置解密器不可用")
	}

	var cipherText string
	if err := json.Unmarshal(storedValue, &cipherText); err != nil || cipherText == "" {
		return nil, errors.New("通知设置密文格式无效")
	}
	plainText, err := secretBox.Decrypt(cipherText)
	if err != nil {
		return nil, fmt.Errorf("解密通知设置: %w", err)
	}
	if !json.Valid([]byte(plainText)) {
		return nil, errors.New("解密后的通知设置不是有效 JSON")
	}
	return json.RawMessage(plainText), nil
}
