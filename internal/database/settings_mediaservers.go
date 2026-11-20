package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

// MediaServersSettingKey 是 MoviePilot V2 媒体服务器配置使用的系统设置键。
const MediaServersSettingKey = "MediaServers"

// LoadMediaServers 返回解密并规范化后的完整媒体服务器快照。
// 旧明文记录和缺少稳定 ID 的记录会在首次读取时完成加密升级。
func (s *Store) LoadMediaServers(ctx context.Context) (mediaconfig.Snapshot, bool, error) {
	value, encrypted, found, err := s.loadMediaServersSetting(ctx)
	if err != nil || !found {
		return mediaconfig.Snapshot{}, found, err
	}
	snapshot, changed, err := decodeMediaServersSnapshot(value)
	if err != nil {
		return mediaconfig.Snapshot{}, false, err
	}
	if changed || !encrypted {
		snapshot, err = s.SaveMediaServers(ctx, snapshot.Servers)
		if err != nil {
			return mediaconfig.Snapshot{}, false, err
		}
	}
	return snapshot.Clone(), true, nil
}

// SaveMediaServers 规范化、校验并加密完整媒体服务器配置，返回可热重载的快照。
func (s *Store) SaveMediaServers(ctx context.Context, servers []mediaconfig.Server) (mediaconfig.Snapshot, error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return mediaconfig.Snapshot{}, errors.New("媒体服务器设置存储不可用")
	}
	snapshot, storedValue, err := prepareMediaServersSetting(s.secretBox, servers)
	if err != nil {
		return mediaconfig.Snapshot{}, err
	}
	if err := s.saveMediaServersSetting(ctx, storedValue); err != nil {
		return mediaconfig.Snapshot{}, err
	}
	return snapshot.Clone(), nil
}

func (s *Store) loadMediaServersSetting(
	ctx context.Context,
) (value json.RawMessage, encrypted bool, found bool, err error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return nil, false, false, errors.New("媒体服务器设置存储不可用")
	}

	var storedValue []byte
	err = s.db.Pool.QueryRow(ctx, `
		SELECT value, encrypted
		FROM settings
		WHERE key=$1`, MediaServersSettingKey).Scan(&storedValue, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("读取媒体服务器设置: %w", err)
	}

	plainValue, err := decodeMediaServersSetting(s.secretBox, storedValue, encrypted)
	if err != nil {
		return nil, false, false, err
	}
	return plainValue, encrypted, true, nil
}

func (s *Store) saveMediaServersSetting(ctx context.Context, storedValue json.RawMessage) error {
	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value, encrypted, updated_at)
		VALUES ($1, $2::jsonb, TRUE, now())
		ON CONFLICT (key) DO UPDATE
		SET value=EXCLUDED.value, encrypted=TRUE, updated_at=now()`,
		MediaServersSettingKey, storedValue)
	if err != nil {
		return fmt.Errorf("保存媒体服务器设置: %w", err)
	}
	return nil
}

func prepareMediaServersSetting(
	secretBox *auth.SecretBox,
	servers []mediaconfig.Server,
) (mediaconfig.Snapshot, json.RawMessage, error) {
	snapshot, _, err := mediaconfig.Normalize(servers)
	if err != nil {
		return mediaconfig.Snapshot{}, nil, err
	}
	if err := mediaconfig.Validate(snapshot.Servers); err != nil {
		return mediaconfig.Snapshot{}, nil, err
	}
	value, err := json.Marshal(snapshot.Servers)
	if err != nil {
		return mediaconfig.Snapshot{}, nil, errors.New("编码媒体服务器设置失败")
	}
	storedValue, err := encodeMediaServersSetting(secretBox, value)
	if err != nil {
		return mediaconfig.Snapshot{}, nil, err
	}
	return snapshot, storedValue, nil
}

func decodeMediaServersSnapshot(value json.RawMessage) (mediaconfig.Snapshot, bool, error) {
	servers, err := mediaconfig.Decode(value)
	if err != nil {
		return mediaconfig.Snapshot{}, false, err
	}
	snapshot, changed, err := mediaconfig.Normalize(servers)
	if err != nil {
		return mediaconfig.Snapshot{}, false, err
	}
	if err := mediaconfig.Validate(snapshot.Servers); err != nil {
		return mediaconfig.Snapshot{}, false, err
	}
	return snapshot, changed, nil
}

func encodeMediaServersSetting(secretBox *auth.SecretBox, value json.RawMessage) (json.RawMessage, error) {
	if secretBox == nil {
		return nil, errors.New("媒体服务器设置加密器不可用")
	}
	if !json.Valid(value) {
		return nil, errors.New("媒体服务器设置不是有效 JSON")
	}
	cipherText, err := secretBox.Encrypt(string(value))
	if err != nil {
		return nil, fmt.Errorf("加密媒体服务器设置: %w", err)
	}
	encoded, err := json.Marshal(cipherText)
	if err != nil {
		return nil, errors.New("编码媒体服务器设置密文失败")
	}
	return encoded, nil
}

func decodeMediaServersSetting(
	secretBox *auth.SecretBox,
	storedValue json.RawMessage,
	encrypted bool,
) (json.RawMessage, error) {
	if !encrypted {
		if !json.Valid(storedValue) {
			return nil, errors.New("媒体服务器设置不是有效 JSON")
		}
		return append(json.RawMessage(nil), storedValue...), nil
	}
	if secretBox == nil {
		return nil, errors.New("媒体服务器设置解密器不可用")
	}

	var cipherText string
	if err := json.Unmarshal(storedValue, &cipherText); err != nil || cipherText == "" {
		return nil, errors.New("媒体服务器设置密文格式无效")
	}
	plainText, err := secretBox.Decrypt(cipherText)
	if err != nil {
		return nil, fmt.Errorf("解密媒体服务器设置: %w", err)
	}
	if !json.Valid([]byte(plainText)) {
		return nil, errors.New("解密后的媒体服务器设置不是有效 JSON")
	}
	return json.RawMessage(plainText), nil
}
