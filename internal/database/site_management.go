package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) ListSites(ctx context.Context) ([]Site, error) {
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id, name, domain, url, adapter, enabled, config
		FROM sites ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("查询站点: %w", err)
	}
	defer rows.Close()
	sites := make([]Site, 0)
	for rows.Next() {
		site, scanErr := scanPublicSite(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历站点: %w", err)
	}
	sort.SliceStable(sites, func(i, j int) bool {
		left, right := sitePriority(sites[i]), sitePriority(sites[j])
		if left == right {
			return sites[i].ID < sites[j].ID
		}
		return left < right
	})
	return sites, nil
}

func (s *Store) SiteByID(ctx context.Context, id int) (Site, error) {
	return scanPublicSite(s.db.Pool.QueryRow(ctx, `
		SELECT id, name, domain, url, adapter, enabled, config
		FROM sites WHERE id=$1`, id))
}

func (s *Store) RuntimeSiteByID(ctx context.Context, id int) (Site, error) {
	return s.siteWithSecrets(ctx, id)
}

func (s *Store) UpdateSite(ctx context.Context, site Site) error {
	if site.ID <= 0 {
		return errors.New("站点 ID 无效")
	}
	if s.secretBox == nil {
		return errors.New("站点敏感配置加密器未初始化")
	}
	publicConfig, secrets := splitSiteConfig(site.Config)
	publicJSON, err := json.Marshal(publicConfig)
	if err != nil {
		return errors.New("编码站点公开配置失败")
	}
	secretJSON, err := json.Marshal(secrets)
	if err != nil {
		return errors.New("编码站点敏感配置失败")
	}
	encrypted, err := s.secretBox.Encrypt(string(secretJSON))
	if err != nil {
		return fmt.Errorf("加密站点敏感配置: %w", err)
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE sites SET name=$1, domain=$2, url=$3, adapter=$4, enabled=$5,
			config=$6::jsonb, secrets=$7, updated_at=now()
		WHERE id=$8`, strings.TrimSpace(site.Name), strings.TrimSpace(site.Domain), strings.TrimSpace(site.URL),
		strings.TrimSpace(site.Adapter), site.Enabled, publicJSON, encrypted, site.ID)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return ErrSiteConflict
		}
		return fmt.Errorf("更新站点: %w", err)
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteSite(ctx context.Context, id int) (bool, error) {
	result, err := s.db.Pool.Exec(ctx, `DELETE FROM sites WHERE id=$1`, id)
	if err != nil {
		return false, fmt.Errorf("删除站点: %w", err)
	}
	return result.RowsAffected() > 0, nil
}

// DeleteAllSites 复刻 V2 `Site.reset(db)` 的清空动作。
// 站点被引用为下载计划来源时，级联会触达不可变快照表，因此显式开启一次性清理开关。
func (s *Store) DeleteAllSites(ctx context.Context) (int64, error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("重置站点: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL moviepilot.allow_snapshot_purge = 'on'`); err != nil {
		return 0, fmt.Errorf("重置站点: %w", err)
	}
	result, err := tx.Exec(ctx, `DELETE FROM sites`)
	if err != nil {
		return 0, fmt.Errorf("重置站点: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("重置站点: %w", err)
	}
	return result.RowsAffected(), nil
}

func (s *Store) siteWithSecrets(ctx context.Context, id int) (Site, error) {
	var site Site
	var rawConfig []byte
	var encryptedSecrets string
	err := s.db.Pool.QueryRow(ctx, `
		SELECT id, name, domain, url, adapter, enabled, config, COALESCE(secrets, '')
		FROM sites WHERE id=$1`, id).Scan(
		&site.ID, &site.Name, &site.Domain, &site.URL, &site.Adapter, &site.Enabled, &rawConfig, &encryptedSecrets,
	)
	if err != nil {
		return Site{}, err
	}
	if err := json.Unmarshal(rawConfig, &site.Config); err != nil {
		return Site{}, fmt.Errorf("解析站点配置: %w", err)
	}
	if site.Config == nil {
		site.Config = make(map[string]any)
	}
	if encryptedSecrets == "" {
		return site, nil
	}
	if s.secretBox == nil {
		return Site{}, errors.New("站点敏感配置解密器未初始化")
	}
	plainText, err := s.secretBox.Decrypt(encryptedSecrets)
	if err != nil {
		return Site{}, fmt.Errorf("解密站点 %d 敏感配置: %w", site.ID, err)
	}
	var secrets map[string]any
	if err := json.Unmarshal([]byte(plainText), &secrets); err != nil {
		return Site{}, fmt.Errorf("解析站点 %d 敏感配置: %w", site.ID, err)
	}
	mergeSiteConfig(site.Config, secrets)
	return site, nil
}

func scanPublicSite(row interface{ Scan(...any) error }) (Site, error) {
	var site Site
	var rawConfig []byte
	if err := row.Scan(&site.ID, &site.Name, &site.Domain, &site.URL, &site.Adapter, &site.Enabled, &rawConfig); err != nil {
		return Site{}, err
	}
	if err := json.Unmarshal(rawConfig, &site.Config); err != nil {
		return Site{}, fmt.Errorf("解析站点配置: %w", err)
	}
	if site.Config == nil {
		site.Config = make(map[string]any)
	}
	if containsSensitiveSiteConfig(site.Config) {
		return Site{}, fmt.Errorf("站点 %d 公开配置包含敏感字段", site.ID)
	}
	return site, nil
}

func sitePriority(site Site) int {
	value, ok := site.Config["pri"]
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	}
	return 0
}

type SitePriority struct {
	ID       int
	Priority int
}

func (s *Store) UpdateSitePriorities(ctx context.Context, priorities []SitePriority) error {
	if len(priorities) == 0 {
		return nil
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开始更新站点优先级: %w", err)
	}
	defer tx.Rollback(ctx)
	for _, priority := range priorities {
		result, updateErr := tx.Exec(ctx, `
			UPDATE sites
			SET config = jsonb_set(config, '{pri}', to_jsonb($2::integer), true), updated_at = now()
			WHERE id = $1`, priority.ID, priority.Priority)
		if updateErr != nil {
			return fmt.Errorf("更新站点 %d 优先级: %w", priority.ID, updateErr)
		}
		if result.RowsAffected() == 0 {
			return fmt.Errorf("站点 %d 不存在: %w", priority.ID, pgx.ErrNoRows)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交站点优先级: %w", err)
	}
	return nil
}
