package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
)

// DownloadersSettingKey 是 MoviePilot V2 下载器配置使用的系统设置键。
const DownloadersSettingKey = "Downloaders"

// LoadDownloaders 仅向服务端代码返回解密后的完整下载器配置。
// 旧明文记录和缺少稳定 ID 的记录会在首次读取时完成安全升级。
func (s *Store) LoadDownloaders(ctx context.Context) ([]downloadconfig.Downloader, bool, error) {
	value, encrypted, found, err := s.loadDownloadersSetting(ctx)
	if err != nil || !found {
		return nil, found, err
	}
	settings, err := downloadconfig.Decode(value)
	if err != nil {
		return nil, false, err
	}
	changed, err := downloadconfig.Normalize(settings)
	if err != nil {
		return nil, false, err
	}
	secretMetadataChanged, err := downloadconfig.SanitizeStoredSecrets(settings)
	if err != nil {
		return nil, false, err
	}
	if changed || secretMetadataChanged || !encrypted {
		if err := s.SaveDownloaders(ctx, settings); err != nil {
			return nil, false, err
		}
	}
	return settings, true, nil
}

// SaveDownloaders 规范化并加密完整下载器配置。
func (s *Store) SaveDownloaders(ctx context.Context, settings []downloadconfig.Downloader) error {
	if _, err := downloadconfig.Normalize(settings); err != nil {
		return err
	}
	if _, err := downloadconfig.SanitizeStoredSecrets(settings); err != nil {
		return err
	}
	value, err := json.Marshal(settings)
	if err != nil {
		return errors.New("编码下载器设置失败")
	}
	return s.saveDownloadersSetting(ctx, value)
}

func (s *Store) loadDownloadersSetting(ctx context.Context) (value json.RawMessage, encrypted bool, found bool, err error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil, false, false, errors.New("下载器设置存储不可用")
	}

	var storedValue []byte
	err = s.db.Pool.QueryRow(ctx, `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, DownloadersSettingKey).Scan(&storedValue, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("读取下载器设置: %w", err)
	}

	plainValue, err := decodeDownloadersSetting(s.secretBox, storedValue, encrypted)
	if err != nil {
		return nil, false, false, err
	}
	return plainValue, encrypted, true, nil
}

func (s *Store) saveDownloadersSetting(ctx context.Context, value json.RawMessage) error {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return errors.New("下载器设置存储不可用")
	}

	storedValue, err := encodeDownloadersSetting(s.secretBox, value)
	if err != nil {
		return err
	}
	_, err = s.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value, encrypted, updated_at)
		VALUES ($1, $2::jsonb, TRUE, now())
		ON CONFLICT (key) DO UPDATE
		SET value=EXCLUDED.value, encrypted=TRUE, updated_at=now()`,
		DownloadersSettingKey, storedValue)
	if err != nil {
		return fmt.Errorf("保存下载器设置: %w", err)
	}
	return nil
}

func encodeDownloadersSetting(secretBox *auth.SecretBox, value json.RawMessage) (json.RawMessage, error) {
	if secretBox == nil {
		return nil, errors.New("下载器设置加密器不可用")
	}
	if !json.Valid(value) {
		return nil, errors.New("下载器设置不是有效 JSON")
	}
	cipherText, err := secretBox.Encrypt(string(value))
	if err != nil {
		return nil, fmt.Errorf("加密下载器设置: %w", err)
	}
	encoded, err := json.Marshal(cipherText)
	if err != nil {
		return nil, errors.New("编码下载器设置密文失败")
	}
	return encoded, nil
}

func decodeDownloadersSetting(secretBox *auth.SecretBox, storedValue json.RawMessage, encrypted bool) (json.RawMessage, error) {
	if !encrypted {
		if !json.Valid(storedValue) {
			return nil, errors.New("下载器设置不是有效 JSON")
		}
		return append(json.RawMessage(nil), storedValue...), nil
	}
	if secretBox == nil {
		return nil, errors.New("下载器设置解密器不可用")
	}

	var cipherText string
	if err := json.Unmarshal(storedValue, &cipherText); err != nil || cipherText == "" {
		return nil, errors.New("下载器设置密文格式无效")
	}
	plainText, err := secretBox.Decrypt(cipherText)
	if err != nil {
		return nil, fmt.Errorf("解密下载器设置: %w", err)
	}
	if !json.Valid([]byte(plainText)) {
		return nil, errors.New("解密后的下载器设置不是有效 JSON")
	}
	return json.RawMessage(plainText), nil
}
