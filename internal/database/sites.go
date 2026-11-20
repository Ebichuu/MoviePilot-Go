package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrSiteConflict = errors.New("site already exists")

// Site 是站点适配器装配所需的最小持久化模型。
type Site struct {
	ID      int
	Name    string
	Domain  string
	URL     string
	Adapter string
	Enabled bool
	Config  map[string]any
}

// ListEnabledSites 返回启用的站点定义；敏感 Cookie 仅在内存中传递给适配器。
func (s *Store) ListEnabledSites(ctx context.Context) ([]Site, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, name, domain, url, adapter, enabled, config, COALESCE(secrets, '')
		FROM sites WHERE enabled=TRUE ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查询启用站点: %w", err)
	}
	defer rows.Close()
	sites := make([]Site, 0)
	for rows.Next() {
		var site Site
		var rawConfig []byte
		var encryptedSecrets string
		if err := rows.Scan(&site.ID, &site.Name, &site.Domain, &site.URL, &site.Adapter, &site.Enabled, &rawConfig, &encryptedSecrets); err != nil {
			return nil, fmt.Errorf("读取站点: %w", err)
		}
		if err := json.Unmarshal(rawConfig, &site.Config); err != nil {
			return nil, fmt.Errorf("解析站点配置: %w", err)
		}
		if site.Config == nil {
			site.Config = make(map[string]any)
		}
		if containsSensitiveSiteConfig(site.Config) {
			return nil, fmt.Errorf("站点 %d 敏感配置未加密", site.ID)
		}
		if encryptedSecrets != "" {
			if s.secretBox == nil {
				return nil, errors.New("站点敏感配置解密器未初始化")
			}
			plainText, decryptErr := s.secretBox.Decrypt(encryptedSecrets)
			if decryptErr != nil {
				return nil, fmt.Errorf("解密站点 %d 敏感配置: %w", site.ID, decryptErr)
			}
			var secrets map[string]any
			if err := json.Unmarshal([]byte(plainText), &secrets); err != nil {
				return nil, fmt.Errorf("解析站点 %d 敏感配置: %w", site.ID, err)
			}
			mergeSiteConfig(site.Config, secrets)
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历站点: %w", err)
	}
	return sites, nil
}

// UpsertSite 分离并加密敏感配置，禁止 Cookie/API Key 明文进入 JSONB。
// 当前创建接口在名称冲突时拒绝写入，不会覆盖已有站点。
func (s *Store) UpsertSite(ctx context.Context, site Site) (int, error) {
	if s.secretBox == nil {
		return 0, errors.New("站点敏感配置加密器未初始化")
	}
	publicConfig, secretConfig := splitSiteConfig(site.Config)
	publicJSON, err := json.Marshal(publicConfig)
	if err != nil {
		return 0, errors.New("编码站点公开配置失败")
	}
	secretJSON, err := json.Marshal(secretConfig)
	if err != nil {
		return 0, errors.New("编码站点敏感配置失败")
	}
	encrypted, err := s.secretBox.Encrypt(string(secretJSON))
	if err != nil {
		return 0, fmt.Errorf("加密站点敏感配置: %w", err)
	}
	var id int
	err = s.db.Pool.QueryRow(ctx, `
		INSERT INTO sites (name, domain, url, adapter, enabled, config, secrets, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, now())
		ON CONFLICT DO NOTHING
		RETURNING id`, strings.TrimSpace(site.Name), strings.TrimSpace(site.Domain), strings.TrimSpace(site.URL),
		strings.TrimSpace(site.Adapter), site.Enabled, publicJSON, encrypted).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrSiteConflict
		}
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return 0, ErrSiteConflict
		}
		return 0, fmt.Errorf("保存站点: %w", err)
	}
	return id, nil
}

func splitSiteConfig(config map[string]any) (map[string]any, map[string]any) {
	publicConfig := make(map[string]any, len(config))
	secretConfig := make(map[string]any)
	for key, value := range config {
		if sensitiveSiteKey(key) {
			if value == nil {
				continue
			}
			if text, ok := value.(string); ok && text == "" {
				continue
			}
			secretConfig[key] = value
			continue
		}
		publicConfig[key] = value
	}
	return publicConfig, secretConfig
}

func containsSensitiveSiteConfig(config map[string]any) bool {
	for key := range config {
		if sensitiveSiteKey(key) {
			return true
		}
	}
	return false
}

func sensitiveSiteKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "cookie", "headers", "apikey", "api_key", "passkey", "token", "secret", "authorization":
		return true
	default:
		return false
	}
}

func mergeSiteConfig(target, secrets map[string]any) {
	for key, value := range secrets {
		target[key] = value
	}
}
