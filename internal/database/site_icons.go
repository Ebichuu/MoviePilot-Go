package database

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

const maxSiteIconConfigBytes = 192 << 10

type siteIconConfig struct {
	URL    string `json:"url"`
	Base64 string `json:"base64"`
}

// SiteIconByDomain reads a cached V2 site icon from sites.config._site_icon.
func (s *Store) SiteIconByDomain(
	ctx context.Context,
	domain string,
) (base64Data string, iconURL string, found bool, err error) {
	domain = normalizeSiteIconDomain(domain)
	if ctx == nil || domain == "" {
		return "", "", false, errors.New("站点图标查询参数无效")
	}
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	var encoded []byte
	err = s.db.Pool.QueryRow(ctx, `
		SELECT config->'_site_icon'
		FROM sites
		WHERE lower(domain)=$1
		ORDER BY id
		LIMIT 1`, domain).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if len(encoded) == 0 || len(encoded) > maxSiteIconConfigBytes {
		return "", "", false, nil
	}
	var icon siteIconConfig
	if err := json.Unmarshal(encoded, &icon); err != nil {
		return "", "", false, nil
	}
	if strings.TrimSpace(icon.Base64) == "" && strings.TrimSpace(icon.URL) == "" {
		return "", "", false, nil
	}
	return icon.Base64, icon.URL, true, nil
}

// UpdateSiteIcon stores a discovered favicon without adding a table or migration.
func (s *Store) UpdateSiteIcon(ctx context.Context, domain, iconURL, base64Data string) error {
	domain = normalizeSiteIconDomain(domain)
	if ctx == nil || domain == "" || (strings.TrimSpace(iconURL) == "" && strings.TrimSpace(base64Data) == "") {
		return errors.New("站点图标更新参数无效")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	icon := siteIconConfig{URL: strings.TrimSpace(iconURL), Base64: strings.TrimSpace(base64Data)}
	encoded, err := json.Marshal(icon)
	if err != nil || len(encoded) > maxSiteIconConfigBytes {
		return errors.New("站点图标超过配置限制")
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE sites
		SET config=jsonb_set(COALESCE(config, '{}'::jsonb), '{_site_icon}', $2::jsonb, TRUE)
		WHERE lower(domain)=$1`, domain, encoded)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func normalizeSiteIconDomain(raw string) string {
	domain := strings.TrimSpace(strings.ToLower(raw))
	if strings.Contains(domain, "://") {
		if index := strings.Index(domain, "://"); index >= 0 {
			domain = domain[index+3:]
		}
	}
	if slash := strings.IndexAny(domain, "/?#"); slash >= 0 {
		domain = domain[:slash]
	}
	return strings.TrimSuffix(strings.TrimSpace(domain), ".")
}

var _ interface {
	SiteIconByDomain(context.Context, string) (string, string, bool, error)
} = (*Store)(nil)
