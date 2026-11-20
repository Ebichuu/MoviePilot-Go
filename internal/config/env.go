package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

// EnvironmentUpdateResult 描述单个系统环境配置项的更新结果。
type EnvironmentUpdateResult struct {
	Changed bool
	Failed  bool
	Message string
}

// EnvironmentSettings 承载 V2 `/api/v1/system/env` 的运行期读写与 `app.env` 持久化。
type EnvironmentSettings struct {
	mu                  sync.RWMutex
	envFilePath         string
	values              map[string]any
	onRSSIntervalChange func(context.Context, time.Duration) error
}

var protectedEnvKeys = map[string]struct{}{
	"SECRET_KEY":                    {},
	"RESOURCE_SECRET_KEY":           {},
	"MOVIEPILOT_JWT_SECRET":         {},
	"MOVIEPILOT_RESOURCE_SECRET":    {},
	"MOVIEPILOT_MASTER_KEY":         {},
	"MOVIEPILOT_BOOTSTRAP_PASSWORD": {},
	"MOVIEPILOT_DATABASE_URL":       {},
	"MOVIEPILOT_REDIS_URL":          {},
}

// NewEnvironmentSettings 基于当前 Config 与 `CONFIG_DIR/app.env` 构建系统环境配置视图。
func NewEnvironmentSettings(
	cfg Config,
	onRSSIntervalChange func(context.Context, time.Duration) error,
) *EnvironmentSettings {
	configDir := strings.TrimSpace(cfg.ConfigDir)
	if configDir == "" {
		configDir = defaultConfigDir
	}
	envPath := filepath.Join(configDir, "app.env")

	values := make(map[string]any)
	if persisted, err := loadEnvFile(envPath); err == nil {
		for key, raw := range persisted {
			if _, blocked := protectedEnvKeys[key]; blocked {
				continue
			}
			values[key] = raw
		}
	}

	tz := defaultTimezone
	if cfg.Location != nil && cfg.Location.String() != "" {
		tz = cfg.Location.String()
	}
	rssMinutes := int(cfg.SubscribeRSSInterval / time.Minute)
	if rssMinutes <= 0 {
		rssMinutes = int(defaultSubscribeRSS / time.Minute)
	}
	tmdbDomain := strings.TrimSpace(cfg.TMDBImageDomain)
	if tmdbDomain == "" {
		tmdbDomain = defaultTMDBImageDomain
	}

	values["APP_DOMAIN"] = cfg.AppDomain
	values["PROXY_HOST"] = cfg.ProxyHost
	values["TMDB_IMAGE_DOMAIN"] = tmdbDomain
	values["SUBSCRIBE_RSS_INTERVAL"] = rssMinutes
	values["SITE_MESSAGE"] = cfg.SiteMessage
	values["TZ"] = tz
	if cfg.CookieCloudHost != "" {
		values["COOKIECLOUD_HOST"] = cfg.CookieCloudHost
	}
	if cfg.CookieCloudEnableLocal {
		values["COOKIECLOUD_ENABLE_LOCAL"] = true
	}

	return &EnvironmentSettings{
		envFilePath:         envPath,
		values:              values,
		onRSSIntervalChange: onRSSIntervalChange,
	}
}

// Snapshot 返回当前公开系统环境配置的深拷贝。
func (s *EnvironmentSettings) Snapshot() map[string]any {
	if s == nil {
		return map[string]any{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]any, len(s.values))
	for key, value := range s.values {
		if _, blocked := protectedEnvKeys[key]; blocked {
			continue
		}
		out[key] = value
	}
	return out
}

// SiteMessagesEnabled 返回当前是否启用了站点消息抓取。
func (s *EnvironmentSettings) SiteMessagesEnabled() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	raw, ok := s.values["SITE_MESSAGE"]
	if !ok {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		return parseBoolDefault(strings.TrimSpace(v), false)
	default:
		return false
	}
}

// NotificationSendTime 返回配置中的通知发送时段（未设置时返回 nil）。
func (s *EnvironmentSettings) NotificationSendTime() any {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if value, ok := s.values["NOTIFICATION_SEND_TIME"]; ok {
		return value
	}
	if value, ok := s.values["MESSAGER_SEND_TIME"]; ok {
		return value
	}
	return nil
}

// Update 校验并更新系统环境配置，并将变更持久化到 `CONFIG_DIR/app.env`。
func (s *EnvironmentSettings) Update(ctx context.Context, env map[string]any) (map[string]EnvironmentUpdateResult, error) {
	if s == nil {
		return nil, errors.New("系统环境配置未初始化")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	results := make(map[string]EnvironmentUpdateResult, len(env))
	if len(env) == 0 {
		return results, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	staged := make(map[string]any, len(s.values))
	for k, v := range s.values {
		staged[k] = v
	}

	var pendingRSSInterval *time.Duration
	anyChanged := false

	for rawKey, rawVal := range env {
		key := strings.TrimSpace(rawKey)
		if !validEnvKey(key) {
			results[rawKey] = EnvironmentUpdateResult{Failed: true, Message: fmt.Sprintf("配置项名称无效: %s", rawKey)}
			continue
		}
		if _, blocked := protectedEnvKeys[key]; blocked {
			results[key] = EnvironmentUpdateResult{Failed: true, Message: fmt.Sprintf("配置项 %s 不允许通过接口修改", key)}
			continue
		}

		normalized, ok, errMsg := normalizeEnvValue(key, rawVal)
		if !ok {
			results[key] = EnvironmentUpdateResult{Failed: true, Message: errMsg}
			continue
		}

		if key == "SUBSCRIBE_RSS_INTERVAL" {
			interval, err := rssIntervalFromValue(normalized)
			if err != nil {
				results[key] = EnvironmentUpdateResult{Failed: true, Message: fmt.Sprintf("SUBSCRIBE_RSS_INTERVAL 无效: %v", err)}
				continue
			}
			pendingRSSInterval = &interval
		}

		if envValueEqual(staged[key], normalized) {
			results[key] = EnvironmentUpdateResult{}
			continue
		}
		staged[key] = normalized
		results[key] = EnvironmentUpdateResult{Changed: true}
		anyChanged = true
	}

	if !anyChanged {
		return results, nil
	}

	if pendingRSSInterval != nil && s.onRSSIntervalChange != nil {
		if err := s.onRSSIntervalChange(ctx, *pendingRSSInterval); err != nil {
			results["SUBSCRIBE_RSS_INTERVAL"] = EnvironmentUpdateResult{
				Failed:  true,
				Message: "更新 RSS 扫描周期失败",
			}
			delete(staged, "SUBSCRIBE_RSS_INTERVAL")
			if prev, exists := s.values["SUBSCRIBE_RSS_INTERVAL"]; exists {
				staged["SUBSCRIBE_RSS_INTERVAL"] = prev
			}
		}
	}

	if err := persistEnvFile(s.envFilePath, staged); err != nil {
		return nil, fmt.Errorf("写入环境配置文件: %w", err)
	}
	s.values = staged
	return results, nil
}

func validEnvKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for i, r := range key {
		isLetter := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 && !isLetter {
			return false
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

func normalizeEnvValue(key string, raw any) (any, bool, string) {
	if raw == nil {
		return "", true, ""
	}
	switch v := raw.(type) {
	case string:
		if strings.ContainsAny(v, "\x00\r\n") {
			return nil, false, fmt.Sprintf("配置项 %s 包含非法控制字符", key)
		}
		if key == "SITE_MESSAGE" {
			if parsed, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(v))); err == nil {
				return parsed, true, ""
			}
			return nil, false, "SITE_MESSAGE 必须为布尔值"
		}
		return strings.TrimSpace(v), true, ""
	case bool:
		return v, true, ""
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true, ""
		}
		if f, err := v.Float64(); err == nil {
			return f, true, ""
		}
		return v.String(), true, ""
	case float64:
		return v, true, ""
	case int:
		return int64(v), true, ""
	case int64:
		return v, true, ""
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, false, fmt.Sprintf("配置项 %s 格式无效", key)
		}
		return string(encoded), true, ""
	}
}

func rssIntervalFromValue(value any) (time.Duration, error) {
	switch v := value.(type) {
	case int64:
		if v <= 0 {
			return 0, errors.New("必须大于 0")
		}
		return time.Duration(v) * time.Minute, nil
	case int:
		if v <= 0 {
			return 0, errors.New("必须大于 0")
		}
		return time.Duration(v) * time.Minute, nil
	case float64:
		if v <= 0 || v != float64(int64(v)) {
			return 0, errors.New("必须为正整数分钟")
		}
		return time.Duration(int64(v)) * time.Minute, nil
	case string:
		return parseRSSInterval(strings.TrimSpace(v))
	default:
		return 0, errors.New("必须为正整数分钟或时长")
	}
}

func envValueEqual(a, b any) bool {
	return formatEnvString(a) == formatEnvString(b)
}

func formatEnvString(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

func persistEnvFile(path string, values map[string]any) error {
	existing, err := loadEnvFile(path)
	if err != nil {
		return err
	}
	merged := make(map[string]string, len(existing)+len(values))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range values {
		if _, blocked := protectedEnvKeys[k]; blocked {
			continue
		}
		merged[k] = formatEnvString(v)
	}

	content, err := godotenv.Marshal(merged)
	if err != nil {
		return err
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".app.env.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
