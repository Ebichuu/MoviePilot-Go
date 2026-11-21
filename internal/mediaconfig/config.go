// Package mediaconfig 定义媒体服务器设置的持久化模型、规范化规则和安全快照。
package mediaconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	pathpkg "path"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const (
	maxMediaServerNameBytes   = 255
	maxMediaServerAPIKeyBytes = 4096
)

var (
	// ErrInvalidMediaServers 表示媒体服务器设置不满足持久化或运行时约束。
	ErrInvalidMediaServers = errors.New("媒体服务器设置无效")
)

// Server 是 MoviePilot V2 MediaServers 设置中的单个媒体服务器。
type Server struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Enabled       bool          `json:"enabled"`
	SyncInterval  int           `json:"sync_interval"`
	SyncLibraries []string      `json:"sync_libraries"`
	PathMapping   []PathMapping `json:"path_mapping"`
	Config        Config        `json:"config"`
	hadID         bool          `json:"-"`
}

// Config 是 Emby / Jellyfin / Plex 连接所需的服务器配置。
type Config struct {
	Host     string `json:"host"`
	PlayHost string `json:"play_host"`
	Username string `json:"username"`
	APIKey   string `json:"apikey"`
	Token    string `json:"token,omitempty"`
}

// EffectiveToken 返回用于认证的密钥（Plex 可填 token 或 apikey，Emby/Jellyfin 使用 apikey）。
func (c Config) EffectiveToken() string {
	if token := strings.TrimSpace(c.Token); token != "" {
		return token
	}
	return strings.TrimSpace(c.APIKey)
}

// Snapshot 是可原子替换的完整规范化媒体服务器快照。
// RequiredID 是配置顺序中首个已启用 Emby 服务器的稳定 ID。
type Snapshot struct {
	Servers    []Server `json:"value"`
	RequiredID string   `json:"required"`
}

// Clone 返回与原值不共享可变切片的服务器副本。
func (s Server) Clone() Server {
	s.SyncLibraries = cloneStrings(s.SyncLibraries)
	s.PathMapping = clonePathMappings(s.PathMapping)
	s.Config = s.Config.Clone()
	return s
}

// Clone 返回配置副本。Config 当前仅包含值类型字段。
func (c Config) Clone() Config { return c }

// Clone 返回与原快照不共享可变切片的副本。
func (s Snapshot) Clone() Snapshot {
	return Snapshot{Servers: cloneServers(s.Servers), RequiredID: s.RequiredID}
}

// Decode 解析原始 MediaServers 数组，并保留调用方是否提供稳定 ID 的信息。
func Decode(value json.RawMessage) ([]Server, error) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, invalid("媒体服务器设置必须是数组")
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var servers []Server
	if err := decoder.Decode(&servers); err != nil {
		return nil, invalid("媒体服务器设置 JSON 无效")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid("媒体服务器设置只能包含一个 JSON 值")
	}
	if servers == nil {
		servers = []Server{}
	}
	for index := range servers {
		servers[index].hadID = strings.TrimSpace(servers[index].ID) != ""
	}
	return servers, nil
}

// Normalize 返回独立的规范化快照，不修改调用方传入的配置。
// 该步骤允许已启用服务器暂时缺少 API Key，以便 POST 空值先合并旧秘密；
// 持久化或装载运行态前必须再调用 Validate。
func Normalize(servers []Server) (Snapshot, bool, error) {
	normalized := cloneServers(servers)
	changed := servers == nil
	seenIDs := make(map[string]struct{}, len(normalized))
	seenNames := make(map[string]struct{}, len(normalized))

	for index := range normalized {
		itemChanged, err := normalizeServer(&normalized[index], seenIDs, seenNames)
		if err != nil {
			return Snapshot{}, false, err
		}
		changed = changed || itemChanged
	}

	snapshot := Snapshot{Servers: normalized}
	for _, server := range normalized {
		if server.Enabled && isSupportedServerType(server.Type) {
			snapshot.RequiredID = server.ID
			break
		}
	}
	return snapshot, changed, nil
}

func isSupportedServerType(serverType string) bool {
	return serverType == "emby" || serverType == "jellyfin" || serverType == "plex"
}

func normalizeServer(server *Server, seenIDs, seenNames map[string]struct{}) (bool, error) {
	changed, err := normalizeIdentity(server, seenIDs, seenNames)
	if err != nil {
		return false, err
	}
	serverType := strings.ToLower(strings.TrimSpace(server.Type))
	if !isSupportedServerType(serverType) {
		return false, invalid("媒体服务器类型仅支持 emby、jellyfin、plex")
	}
	if server.Type != serverType {
		server.Type = serverType
		changed = true
	}
	if server.SyncInterval < 0 {
		return false, invalid("媒体服务器同步间隔不能为负数")
	}

	libraries, itemChanged := normalizeLibraries(server.SyncLibraries)
	server.SyncLibraries = libraries
	changed = changed || itemChanged
	mappings, itemChanged, err := normalizePathMappings(server.PathMapping)
	if err != nil {
		return false, err
	}
	server.PathMapping = mappings
	changed = changed || itemChanged
	config, itemChanged, err := normalizeConfig(server.Config)
	if err != nil {
		return false, err
	}
	server.Config = config
	changed = changed || itemChanged
	return changed, nil
}

func normalizeIdentity(server *Server, seenIDs, seenNames map[string]struct{}) (bool, error) {
	changed := false
	trimmedID := strings.TrimSpace(server.ID)
	if trimmedID == "" {
		trimmedID = uuid.NewString()
		changed = true
	}
	parsedID, err := uuid.Parse(trimmedID)
	if err != nil {
		return false, invalid("媒体服务器 ID 无效")
	}
	canonicalID := parsedID.String()
	if server.ID != canonicalID {
		server.ID = canonicalID
		changed = true
	}
	if _, exists := seenIDs[canonicalID]; exists {
		return false, invalid("媒体服务器 ID 重复")
	}
	seenIDs[canonicalID] = struct{}{}

	name := strings.TrimSpace(server.Name)
	if name == "" {
		return false, invalid("媒体服务器名称不能为空")
	}
	if len(name) > maxMediaServerNameBytes {
		return false, invalid("媒体服务器名称过长")
	}
	if server.Name != name {
		server.Name = name
		changed = true
	}
	nameKey := strings.ToLower(name)
	if _, exists := seenNames[nameKey]; exists {
		return false, invalid("媒体服务器名称重复")
	}
	seenNames[nameKey] = struct{}{}
	return changed, nil
}

// Validate 校验规范化后可进入持久化和运行态的完整约束。
func Validate(servers []Server) error {
	snapshot, _, err := Normalize(servers)
	if err != nil {
		return err
	}
	for _, server := range snapshot.Servers {
		if !server.Enabled {
			continue
		}
		if server.Config.Host == "" {
			return invalid("已启用的媒体服务器缺少 host")
		}
		if server.Config.EffectiveToken() == "" {
			return invalid("已启用的媒体服务器缺少 apikey 或 token")
		}
	}
	return nil
}

// MergeSecrets 应用“空 API Key 保留旧值，非空值替换”的更新语义。
// 返回值与 incoming、existing 均不共享可变切片。
func MergeSecrets(incoming, existing []Server) ([]Server, error) {
	incomingSnapshot, _, err := Normalize(incoming)
	if err != nil {
		return nil, err
	}
	existingSnapshot, _, err := Normalize(existing)
	if err != nil {
		return nil, err
	}
	merged := cloneServers(incomingSnapshot.Servers)
	existingCopy := cloneServers(existingSnapshot.Servers)
	existingByID := make(map[string]*Server, len(existingCopy))
	existingByName := make(map[string]*Server, len(existingCopy))
	for index := range existingCopy {
		server := &existingCopy[index]
		existingByID[server.ID] = server
		existingByName[strings.ToLower(server.Name)] = server
	}

	for index := range merged {
		server := &merged[index]
		var previous *Server
		if server.hadID {
			previous = existingByID[server.ID]
			if previous == nil {
				legacy := existingByName[strings.ToLower(server.Name)]
				if legacy != nil && !legacy.hadID {
					previous = legacy
				}
			}
		} else {
			previous = existingByName[strings.ToLower(server.Name)]
			if previous != nil {
				server.ID = previous.ID
				server.hadID = true
			}
		}
		if strings.TrimSpace(server.Config.APIKey) == "" && previous != nil {
			server.Config.APIKey = previous.Config.APIKey
		}
		if strings.TrimSpace(server.Config.Token) == "" && previous != nil {
			server.Config.Token = previous.Config.Token
		}
	}
	return merged, nil
}

// RedactSecrets 返回 API 可安全输出的独立快照，并清空所有 API Key 与 Token。
func RedactSecrets(snapshot Snapshot) Snapshot {
	redacted := snapshot.Clone()
	for index := range redacted.Servers {
		redacted.Servers[index].Config.APIKey = ""
		redacted.Servers[index].Config.Token = ""
	}
	return redacted
}

func normalizeLibraries(libraries []string) ([]string, bool) {
	if libraries == nil {
		return []string{}, true
	}
	normalized := make([]string, 0, len(libraries))
	seen := make(map[string]struct{}, len(libraries))
	changed := false
	for _, library := range libraries {
		trimmed := strings.TrimSpace(library)
		if trimmed == "" {
			changed = true
			continue
		}
		if trimmed != library {
			changed = true
		}
		if _, exists := seen[trimmed]; exists {
			changed = true
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	return normalized, changed
}

func normalizeConfig(config Config) (Config, bool, error) {
	normalized := config.Clone()
	changed := false

	for _, value := range []*string{&normalized.Username, &normalized.APIKey, &normalized.Token} {
		trimmed := strings.TrimSpace(*value)
		if trimmed != *value {
			*value = trimmed
			changed = true
		}
		if strings.IndexFunc(*value, unicode.IsControl) >= 0 {
			return Config{}, false, invalid("媒体服务器认证字段无效")
		}
	}
	if len(normalized.APIKey) > maxMediaServerAPIKeyBytes || len(normalized.Token) > maxMediaServerAPIKeyBytes {
		return Config{}, false, invalid("媒体服务器 API Key 过长")
	}

	host, hostChanged, err := normalizeEndpoint(config.Host)
	if err != nil {
		return Config{}, false, err
	}
	if hostChanged {
		normalized.Host = host
		changed = true
	}
	playHost, playHostChanged, err := normalizeEndpoint(config.PlayHost)
	if err != nil {
		return Config{}, false, err
	}
	if playHostChanged {
		normalized.PlayHost = playHost
		changed = true
	}
	return normalized, changed, nil
}

func normalizeEndpoint(value string) (string, bool, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", value != "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return "", false, invalid("媒体服务器地址无效")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(trimmed, "#") {
		return "", false, invalid("媒体服务器地址不能包含查询参数或片段")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false, invalid("媒体服务器地址协议无效")
	}
	parsed.Scheme = scheme
	parsed.Host = strings.ToLower(parsed.Host)
	if err := normalizeEndpointPath(parsed); err != nil {
		return "", false, err
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	normalized := parsed.String()
	return normalized, normalized != value, nil
}

func normalizeEndpointPath(parsed *url.URL) error {
	escapedPath := parsed.EscapedPath()
	if escapedPath == "" || escapedPath == "/" {
		parsed.Path = ""
		parsed.RawPath = ""
		return nil
	}
	cleanedPath := pathpkg.Clean(escapedPath)
	decodedPath, err := url.PathUnescape(cleanedPath)
	if err != nil || strings.IndexFunc(decodedPath, unicode.IsControl) >= 0 {
		return invalid("媒体服务器地址路径无效")
	}
	parsed.Path = decodedPath
	if cleanedPath == decodedPath {
		parsed.RawPath = ""
	} else {
		parsed.RawPath = cleanedPath
	}
	return nil
}

func cloneServers(servers []Server) []Server {
	if servers == nil {
		return []Server{}
	}
	cloned := make([]Server, len(servers))
	for index := range servers {
		cloned[index] = servers[index].Clone()
	}
	return cloned
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func clonePathMappings(values []PathMapping) []PathMapping {
	if values == nil {
		return nil
	}
	return append([]PathMapping{}, values...)
}

func invalid(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidMediaServers, message)
}
