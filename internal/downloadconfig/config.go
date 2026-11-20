// Package downloadconfig 定义下载器设置的持久化模型、规范化规则和安全运行时投影。
package downloadconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// Downloader 是 MoviePilot V2 Downloaders 设置中的单个下载器。
type Downloader struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Default     bool            `json:"default"`
	Enabled     bool            `json:"enabled"`
	Config      map[string]any  `json:"config"`
	PathMapping json.RawMessage `json:"path_mapping,omitempty"`
	hadID       bool            `json:"-"`
}

// AuthMethod 是下载器运行时使用的认证方式。
type AuthMethod string

const (
	// AuthNone 表示不携带认证信息。
	AuthNone AuthMethod = "none"
	// AuthAPIKey 表示优先使用 qBittorrent Bearer API Key。
	AuthAPIKey AuthMethod = "apikey"
	// AuthUsernamePassword 表示使用用户名和密码登录。
	AuthUsernamePassword AuthMethod = "username_password"
)

// Authentication 封装仅供服务端下载器客户端使用的完整认证信息。
// 凭据通过方法读取，默认字符串和结构化日志投影不会包含秘密。
type Authentication struct {
	method   AuthMethod
	username string
	password string
	apiKey   string
}

// Method 返回认证方式。
func (a Authentication) Method() AuthMethod { return a.method }

// Username 返回用户名。
func (a Authentication) Username() string { return a.username }

// Password 返回密码；调用方不得记录或返回该值。
func (a Authentication) Password() string { return a.password }

// APIKey 返回 API Key；调用方不得记录或返回该值。
func (a Authentication) APIKey() string { return a.apiKey }

// String 返回不含秘密的认证方式描述。
func (a Authentication) String() string { return string(a.method) }

// LogValue 返回不含秘密的结构化日志值。
func (a Authentication) LogValue() slog.Value {
	return slog.GroupValue(slog.String("method", string(a.method)))
}

// Runtime 是 Worker/下载器适配器可直接消费的强类型安全投影。
type Runtime struct {
	ID      string
	Name    string
	Type    string
	Host    string
	Enabled bool
	Default bool
	auth    Authentication
	// forceResume 对应 V2 qBittorrent 模块的 `force_resume`：为真时恢复任务
	// 使用 force start，而不是普通 start。
	forceResume bool
}

// Authentication 返回运行时认证信息。
func (r Runtime) Authentication() Authentication { return r.auth }

// ForceResume 返回 V2 的 force_resume 开关。
func (r Runtime) ForceResume() bool { return r.forceResume }

// LogValue 返回不含认证秘密的结构化日志值。
func (r Runtime) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", r.ID),
		slog.String("name", r.Name),
		slog.String("type", r.Type),
		slog.String("host", safeLogHost(r.Host)),
		slog.Bool("enabled", r.Enabled),
		slog.Bool("default", r.Default),
		slog.Any("authentication", r.auth),
	)
}

// Runtime 返回包含稳定 ID、host、认证方式和启用/默认状态的运行时投影。
func (d Downloader) Runtime() (Runtime, error) {
	host, _, err := configString(d.Config, "host")
	if err != nil {
		return Runtime{}, err
	}
	username, _, err := configString(d.Config, "username")
	if err != nil {
		return Runtime{}, err
	}
	password, _, err := configString(d.Config, "password")
	if err != nil {
		return Runtime{}, err
	}
	apiKey, _, err := configString(d.Config, "apikey")
	if err != nil {
		return Runtime{}, err
	}
	forceResume, err := configBool(d.Config, "force_resume")
	if err != nil {
		return Runtime{}, err
	}
	authentication := Authentication{method: AuthNone}
	if apiKey != "" {
		authentication = Authentication{method: AuthAPIKey, apiKey: apiKey}
	} else if username != "" || password != "" {
		authentication = Authentication{
			method:   AuthUsernamePassword,
			username: username,
			password: password,
		}
	}
	return Runtime{
		ID: d.ID, Name: d.Name, Type: d.Type, Host: strings.TrimSpace(host),
		Enabled: d.Enabled, Default: d.Default, auth: authentication,
		forceResume: forceResume,
	}, nil
}

// LogValue 返回不含 password、API Key 或完整 config 的结构化日志值。
func (d Downloader) LogValue() slog.Value {
	runtime, err := d.Runtime()
	if err != nil {
		return slog.GroupValue(
			slog.String("id", d.ID),
			slog.String("name", d.Name),
			slog.String("type", d.Type),
			slog.Bool("enabled", d.Enabled),
			slog.Bool("default", d.Default),
			slog.String("config", "invalid"),
		)
	}
	return runtime.LogValue()
}

// Decode 解析原始 Downloaders 数组并保留调用方是否提供了稳定 ID 的信息。
func Decode(value json.RawMessage) ([]Downloader, error) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("下载器设置必须是数组")
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var settings []Downloader
	if err := decoder.Decode(&settings); err != nil {
		return nil, errors.New("下载器设置 JSON 无效")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("下载器设置只能包含一个 JSON 值")
	}
	if settings == nil {
		settings = []Downloader{}
	}
	for index := range settings {
		settings[index].hadID = strings.TrimSpace(settings[index].ID) != ""
	}
	return settings, nil
}

// Normalize 校验并确定性归一 ID、名称、类型、host 和默认下载器。
func Normalize(settings []Downloader) (bool, error) {
	changed := false
	seenIDs := make(map[string]struct{}, len(settings))
	seenNames := make(map[string]struct{}, len(settings))
	firstEnabled := -1
	defaultIndex := -1

	for index := range settings {
		setting := &settings[index]
		trimmedID := strings.TrimSpace(setting.ID)
		if trimmedID == "" {
			trimmedID = uuid.NewString()
			changed = true
		}
		parsedID, err := uuid.Parse(trimmedID)
		if err != nil {
			return false, errors.New("下载器 ID 无效")
		}
		canonicalID := parsedID.String()
		if setting.ID != canonicalID {
			setting.ID = canonicalID
			changed = true
		}
		if _, exists := seenIDs[canonicalID]; exists {
			return false, errors.New("下载器 ID 重复")
		}
		seenIDs[canonicalID] = struct{}{}

		name := strings.TrimSpace(setting.Name)
		if name == "" {
			return false, errors.New("下载器名称不能为空")
		}
		if setting.Name != name {
			setting.Name = name
			changed = true
		}
		nameKey := strings.ToLower(name)
		if _, exists := seenNames[nameKey]; exists {
			return false, errors.New("下载器名称重复")
		}
		seenNames[nameKey] = struct{}{}

		downloaderType := strings.ToLower(strings.TrimSpace(setting.Type))
		if downloaderType == "" {
			return false, errors.New("下载器类型不能为空")
		}
		if setting.Type != downloaderType {
			setting.Type = downloaderType
			changed = true
		}
		if setting.Config == nil {
			setting.Config = map[string]any{}
			changed = true
		}

		if setting.Type == "qbittorrent" {
			host, hostPresent, err := configString(setting.Config, "host")
			if err != nil {
				return false, err
			}
			if hostPresent {
				trimmedHost := strings.TrimSpace(host)
				if trimmedHost != host {
					setting.Config["host"] = trimmedHost
					changed = true
				}
				host = trimmedHost
			}
			if setting.Enabled && host == "" {
				return false, errors.New("已启用的 qBittorrent 下载器缺少 host")
			}
			if host != "" && !validHost(host) {
				return false, errors.New("下载器 host 无效")
			}
		}

		if setting.Type == "transmission" || setting.Type == "rtorrent" {
			host, hostPresent, err := configString(setting.Config, "host")
			if err != nil {
				return false, err
			}
			if hostPresent {
				trimmedHost := strings.TrimSpace(host)
				if trimmedHost != host {
					setting.Config["host"] = trimmedHost
					changed = true
				}
				host = trimmedHost
			}
			if setting.Enabled && host == "" {
				return false, errors.New("已启用的 " + setting.Type + " 下载器缺少 host")
			}
			if host != "" && !validHost(host) {
				return false, errors.New("下载器 host 无效")
			}
		}

		if setting.Enabled && firstEnabled == -1 {
			firstEnabled = index
		}
		if setting.Default {
			if !setting.Enabled || defaultIndex != -1 {
				setting.Default = false
				changed = true
			} else {
				defaultIndex = index
			}
		}
	}

	if firstEnabled != -1 && defaultIndex == -1 {
		settings[firstEnabled].Default = true
		changed = true
	}
	return changed, nil
}

// MergeSecrets 应用“空值保留、非空替换、clear_* 显式清除”的秘密更新语义。
func MergeSecrets(incoming, existing []Downloader) error {
	existingByID := make(map[string]*Downloader, len(existing))
	existingByName := make(map[string]*Downloader, len(existing))
	for index := range existing {
		setting := &existing[index]
		existingByID[setting.ID] = setting
		existingByName[strings.ToLower(setting.Name)] = setting
	}

	for index := range incoming {
		setting := &incoming[index]
		var previous *Downloader
		if setting.hadID {
			previous = existingByID[setting.ID]
			if previous == nil {
				legacy := existingByName[strings.ToLower(setting.Name)]
				if legacy != nil && !legacy.hadID {
					previous = legacy
				}
			}
		} else {
			previous = existingByName[strings.ToLower(setting.Name)]
			if previous != nil {
				setting.ID = previous.ID
				setting.hadID = true
			}
		}

		for _, field := range []string{"password", "apikey"} {
			if err := mergeSecretField(setting.Config, previous, field); err != nil {
				return err
			}
		}
	}
	return nil
}

// SanitizeStoredSecrets 删除派生/写入专用字段并校验持久化秘密类型。
func SanitizeStoredSecrets(settings []Downloader) (bool, error) {
	changed := false
	for index := range settings {
		config := settings[index].Config
		for _, field := range []string{"password", "apikey"} {
			value, present, err := configString(config, field)
			if err != nil {
				return false, err
			}
			if present && config[field] == nil {
				config[field] = value
				changed = true
			}
			for _, metadataField := range []string{"has_" + field, "clear_" + field} {
				if _, exists := config[metadataField]; exists {
					delete(config, metadataField)
					changed = true
				}
			}
		}
	}
	return changed, nil
}

// RedactSecrets 创建可返回给管理员前端的脱敏副本。
func RedactSecrets(settings []Downloader) ([]Downloader, error) {
	redacted := make([]Downloader, len(settings))
	for index := range settings {
		redacted[index] = settings[index]
		redacted[index].PathMapping = append(json.RawMessage(nil), settings[index].PathMapping...)
		redacted[index].Config = make(map[string]any, len(settings[index].Config)+2)
		for key, value := range settings[index].Config {
			redacted[index].Config[key] = value
		}
		for _, field := range []string{"password", "apikey"} {
			value, _, err := configString(redacted[index].Config, field)
			if err != nil {
				return nil, err
			}
			redacted[index].Config[field] = ""
			redacted[index].Config["has_"+field] = value != ""
			delete(redacted[index].Config, "clear_"+field)
		}
	}
	return redacted, nil
}

func validHost(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return (scheme == "http" || scheme == "https") && parsed.RawQuery == "" && parsed.Fragment == ""
}

func safeLogHost(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host
}

func mergeSecretField(config map[string]any, previous *Downloader, field string) error {
	value, _, err := configString(config, field)
	if err != nil {
		return err
	}
	clear, err := configBool(config, "clear_"+field)
	if err != nil {
		return err
	}
	if clear {
		value = ""
	} else if value == "" && previous != nil {
		previousValue, _, previousErr := configString(previous.Config, field)
		if previousErr != nil {
			return previousErr
		}
		value = previousValue
	}
	config[field] = value
	delete(config, "has_"+field)
	delete(config, "clear_"+field)
	return nil
}

func configString(config map[string]any, key string) (string, bool, error) {
	value, exists := config[key]
	if !exists {
		return "", false, nil
	}
	if value == nil {
		return "", true, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", true, errors.New("下载器字符串配置字段类型无效")
	}
	return text, true, nil
}

func configBool(config map[string]any, key string) (bool, error) {
	value, exists := config[key]
	if !exists || value == nil {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, errors.New("下载器布尔配置字段类型无效")
	}
	return flag, nil
}
