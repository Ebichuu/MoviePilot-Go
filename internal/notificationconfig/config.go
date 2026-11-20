// Package notificationconfig defines the MoviePilot V2 Notifications value
// and its immutable Telegram runtime projection.
package notificationconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

const (
	// TelegramType is the lowercase V2 notification channel identifier.
	TelegramType = "telegram"
)

var (
	// ErrInvalidSettings reports that the top-level value is not one complete
	// JSON value. Individual invalid list items are skipped instead.
	ErrInvalidSettings = errors.New("通知配置无效")
)

// Config is one V2 NotificationConf item. Config retains unknown nested keys,
// while Snapshot.Raw retains unknown top-level item keys.
type Config struct {
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Config  map[string]any `json:"config"`
	Switchs []any          `json:"switchs"`
	Enabled bool           `json:"enabled"`
}

// Telegram is one complete, enabled Telegram runtime configuration.
type Telegram struct {
	Name     string
	BotToken string
	ChatID   string
	APIURL   string
	Switchs  []string
}

// Snapshot contains the normalized V2 value and its detached runtime
// projection. A snapshot can be cloned before atomic publication.
type Snapshot struct {
	Raw       json.RawMessage
	Configs   []Config
	Telegrams []Telegram
}

// Clone returns a copy with no shared mutable JSON, maps, or slices.
func (c Config) Clone() Config {
	c.Config = cloneJSONObject(c.Config)
	c.Switchs = cloneJSONArray(c.Switchs)
	return c
}

// Clone returns a copy with no shared switch slice.
func (t Telegram) Clone() Telegram {
	t.Switchs = append([]string(nil), t.Switchs...)
	return t
}

// Clone returns a copy with no shared mutable state.
func (s Snapshot) Clone() Snapshot {
	cloned := Snapshot{Raw: append(json.RawMessage(nil), s.Raw...)}
	if s.Configs != nil {
		cloned.Configs = make([]Config, len(s.Configs))
		for index := range s.Configs {
			cloned.Configs[index] = s.Configs[index].Clone()
		}
	}
	if s.Telegrams != nil {
		cloned.Telegrams = make([]Telegram, len(s.Telegrams))
		for index := range s.Telegrams {
			cloned.Telegrams[index] = s.Telegrams[index].Clone()
		}
	}
	return cloned
}

// Decode accepts any complete JSON value. Only a top-level list contributes
// runtime configurations. List items are first filtered using Python
// truthiness to preserve V2 setting behavior; invalid individual items are
// skipped without affecting their siblings.
func Decode(value json.RawMessage) (Snapshot, error) {
	trimmed := bytes.TrimSpace(value)
	decoded, err := decodeJSON(trimmed)
	if err != nil {
		return Snapshot{}, ErrInvalidSettings
	}

	snapshot := Snapshot{Raw: append(json.RawMessage(nil), trimmed...)}
	if _, isList := decoded.([]any); !isList {
		return snapshot, nil
	}

	items, normalized, err := filterListItems(trimmed)
	if err != nil {
		return Snapshot{}, ErrInvalidSettings
	}
	snapshot.Raw = normalized
	configIndexes := make(map[string]int, len(items))
	for _, item := range items {
		config, ok := decodeConfig(item)
		if !ok {
			continue
		}
		cloned := config.Clone()
		if config.Name != "" {
			if index, exists := configIndexes[config.Name]; exists {
				snapshot.Configs[index] = cloned
				continue
			}
			configIndexes[config.Name] = len(snapshot.Configs)
		}
		snapshot.Configs = append(snapshot.Configs, cloned)
	}
	for _, config := range snapshot.Configs {
		if telegram, ok := telegramRuntime(config); ok {
			snapshot.Telegrams = append(snapshot.Telegrams, telegram)
		}
	}
	return snapshot, nil
}

func decodeJSON(value []byte) (any, error) {
	if len(value) == 0 {
		return nil, ErrInvalidSettings
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, ErrInvalidSettings
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidSettings
	}
	return decoded, nil
}

func filterListItems(value []byte) ([]json.RawMessage, json.RawMessage, error) {
	var candidates []json.RawMessage
	if err := json.Unmarshal(value, &candidates); err != nil {
		return nil, nil, ErrInvalidSettings
	}
	items := make([]json.RawMessage, 0, len(candidates))
	for _, candidate := range candidates {
		decoded, err := decodeJSON(candidate)
		if err != nil {
			return nil, nil, ErrInvalidSettings
		}
		if pythonTruthy(decoded) {
			items = append(items, append(json.RawMessage(nil), bytes.TrimSpace(candidate)...))
		}
	}
	if len(items) == 0 {
		return nil, json.RawMessage("null"), nil
	}

	var normalized bytes.Buffer
	normalized.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			normalized.WriteByte(',')
		}
		normalized.Write(item)
	}
	normalized.WriteByte(']')
	return items, json.RawMessage(normalized.Bytes()), nil
}

func pythonTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case json.Number:
		number, _ := strconv.ParseFloat(typed.String(), 64)
		return number != 0
	case string:
		return typed != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

func decodeConfig(value json.RawMessage) (Config, bool) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Config{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, false
	}
	return config, true
}

func telegramRuntime(config Config) (Telegram, bool) {
	if !config.Enabled || config.Type != TelegramType || config.Name == "" || containsControl(config.Name) {
		return Telegram{}, false
	}
	token, ok := requiredString(config.Config, "TELEGRAM_TOKEN")
	if !ok || containsControl(token) {
		return Telegram{}, false
	}
	chatID, ok := requiredString(config.Config, "TELEGRAM_CHAT_ID")
	if !ok || containsControl(chatID) {
		return Telegram{}, false
	}
	apiURL, ok := optionalString(config.Config, "API_URL")
	if !ok || (apiURL != "" && !validAPIURL(apiURL)) {
		return Telegram{}, false
	}

	switchs := make([]string, 0, len(config.Switchs))
	for _, value := range config.Switchs {
		switchValue, ok := value.(string)
		if ok && !containsControl(switchValue) {
			switchs = append(switchs, switchValue)
		}
	}
	return Telegram{
		Name: config.Name, BotToken: token, ChatID: chatID,
		APIURL: apiURL, Switchs: switchs,
	}, true
}

func requiredString(values map[string]any, key string) (string, bool) {
	value, exists := values[key]
	if !exists {
		return "", false
	}
	text, ok := value.(string)
	return text, ok && text != ""
}

func optionalString(values map[string]any, key string) (string, bool) {
	value, exists := values[key]
	if !exists || value == nil {
		return "", true
	}
	text, ok := value.(string)
	return text, ok
}

func validAPIURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func cloneJSONObject(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneJSONValue(item)
	}
	return cloned
}

func cloneJSONArray(value []any) []any {
	if value == nil {
		return nil
	}
	cloned := make([]any, len(value))
	for index, item := range value {
		cloned[index] = cloneJSONValue(item)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONObject(typed)
	case []any:
		return cloneJSONArray(typed)
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	case []string:
		return append([]string(nil), typed...)
	default:
		return typed
	}
}
