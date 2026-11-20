package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type legacySiteCredentialRow struct {
	id               int
	rawConfig        []byte
	encryptedSecrets string
}

// UpgradeLegacySiteSecrets 将旧版 sites.config 中的敏感字段原子迁移到加密列。
func (s *Store) UpgradeLegacySiteSecrets(ctx context.Context) (returnErr error) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return errors.New("站点凭据升级数据库未初始化")
	}
	if s.secretBox == nil {
		return errors.New("站点凭据升级加密器未初始化")
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return errors.New("开始站点凭据升级事务失败")
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rollbackErr := tx.Rollback(rollbackCtx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			if returnErr == nil {
				returnErr = errors.New("回滚站点凭据升级事务失败")
			} else {
				returnErr = errors.New("站点凭据升级失败且回滚事务失败")
			}
		}
	}()

	rows, err := tx.Query(ctx, `
		SELECT id, config, COALESCE(secrets, '')
		FROM sites
		ORDER BY id
		FOR UPDATE`)
	if err != nil {
		return errors.New("读取待升级站点失败")
	}
	records := make([]legacySiteCredentialRow, 0)
	for rows.Next() {
		var record legacySiteCredentialRow
		if err := rows.Scan(&record.id, &record.rawConfig, &record.encryptedSecrets); err != nil {
			rows.Close()
			return errors.New("读取待升级站点数据失败")
		}
		record.rawConfig = append([]byte(nil), record.rawConfig...)
		records = append(records, record)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return errors.New("遍历待升级站点失败")
	}

	for _, record := range records {
		if err := s.upgradeLegacySiteCredentialRow(ctx, tx, record); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("提交站点凭据升级事务失败")
	}
	return nil
}

func (s *Store) upgradeLegacySiteCredentialRow(ctx context.Context, tx pgx.Tx, record legacySiteCredentialRow) error {
	var config map[string]any
	if err := json.Unmarshal(record.rawConfig, &config); err != nil {
		return fmt.Errorf("解析站点 %d 公开配置失败", record.id)
	}
	publicConfig, legacyCredentials, changed := splitLegacySiteCredentials(config)
	if !changed {
		return nil
	}

	existingCredentials := make(map[string]any)
	if record.encryptedSecrets != "" {
		plainText, err := s.secretBox.Decrypt(record.encryptedSecrets)
		if err != nil {
			return fmt.Errorf("解密站点 %d 已有凭据失败", record.id)
		}
		if err := json.Unmarshal([]byte(plainText), &existingCredentials); err != nil {
			return fmt.Errorf("解析站点 %d 已有凭据失败", record.id)
		}
		if existingCredentials == nil {
			existingCredentials = make(map[string]any)
		}
	}

	mergedCredentials, credentialsAdded := mergeLegacySiteCredentials(existingCredentials, legacyCredentials)
	nextEncryptedSecrets := record.encryptedSecrets
	if len(mergedCredentials) > 0 && (nextEncryptedSecrets == "" || credentialsAdded) {
		encodedCredentials, err := json.Marshal(mergedCredentials)
		if err != nil {
			return fmt.Errorf("编码站点 %d 凭据失败", record.id)
		}
		nextEncryptedSecrets, err = s.secretBox.Encrypt(string(encodedCredentials))
		if err != nil {
			return fmt.Errorf("加密站点 %d 凭据失败", record.id)
		}
	}
	encodedPublicConfig, err := json.Marshal(publicConfig)
	if err != nil {
		return fmt.Errorf("编码站点 %d 公开配置失败", record.id)
	}

	result, err := tx.Exec(ctx, `
		UPDATE sites
		SET config=$1::jsonb, secrets=$2
		WHERE id=$3`, encodedPublicConfig, nextEncryptedSecrets, record.id)
	if err != nil {
		return fmt.Errorf("更新站点 %d 凭据失败", record.id)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("站点 %d 在凭据升级期间不存在", record.id)
	}
	return nil
}

func splitLegacySiteCredentials(config map[string]any) (map[string]any, map[string]any, bool) {
	publicConfig := make(map[string]any, len(config))
	credentials := make(map[string]any)
	changed := false
	for key, value := range config {
		if !sensitiveSiteKey(key) {
			publicConfig[key] = value
			continue
		}
		changed = true
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		credentials[key] = value
	}
	return publicConfig, credentials, changed
}

func mergeLegacySiteCredentials(existing, legacy map[string]any) (map[string]any, bool) {
	merged := make(map[string]any, len(existing)+len(legacy))
	existingIdentities := make(map[string]struct{}, len(existing))
	for key, value := range existing {
		merged[key] = value
		existingIdentities[siteCredentialIdentity(key)] = struct{}{}
	}
	added := false
	for key, value := range legacy {
		if _, exists := existingIdentities[siteCredentialIdentity(key)]; exists {
			continue
		}
		merged[key] = value
		added = true
	}
	return merged, added
}

func siteCredentialIdentity(key string) string {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if normalized == "api_key" {
		return "apikey"
	}
	return normalized
}
