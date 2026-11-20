package pluginengine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FieldType 决定配置项的取值形状与校验规则。
type FieldType string

const (
	FieldString   FieldType = "string"
	FieldTextarea FieldType = "textarea"
	FieldPassword FieldType = "password"
	FieldInteger  FieldType = "integer"
	FieldNumber   FieldType = "number"
	FieldBoolean  FieldType = "boolean"
	FieldSelect   FieldType = "select"
)

// maxConfigStringLength 限制字符串类配置的长度：这些值最终会写进设置表，
// 不加限制等于允许任意大的 JSON。
const maxConfigStringLength = 4096

var (
	pluginIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,63}$`)
	fieldKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// Option 是 select 型配置的一个可选值。
type Option struct {
	Value string
	Label string
}

// Config 是某个插件实例的配置值。键来自 Manifest.Fields，值已按类型归一。
type Config map[string]any

// Field 描述插件的一个配置项。Default 必须与 Type 匹配，Register 时会校验。
type Field struct {
	Key      string
	Label    string
	Type     FieldType
	Default  any
	Required bool
	Help     string
	Options  []Option
}

// Manifest 是插件的静态清单：它不随实例状态变化，注册后不可修改。
type Manifest struct {
	ID          string
	Name        string
	Author      string
	Version     string
	Level       int
	Description string
	Icon        string
	Label       string
	Fields      []Field
}

// validate 校验清单自洽性，错误统一包装 ErrInvalidManifest。
func (m Manifest) validate() error {
	if !pluginIDPattern.MatchString(m.ID) {
		return fmt.Errorf("%w: 插件 ID %q 必须匹配 %s", ErrInvalidManifest, m.ID, pluginIDPattern)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%w: 插件 %s 缺少名称", ErrInvalidManifest, m.ID)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("%w: 插件 %s 缺少版本", ErrInvalidManifest, m.ID)
	}
	if m.Level < 0 {
		return fmt.Errorf("%w: 插件 %s 的 Level 不能为负", ErrInvalidManifest, m.ID)
	}
	seen := make(map[string]struct{}, len(m.Fields))
	for _, field := range m.Fields {
		if err := field.validate(); err != nil {
			return fmt.Errorf("插件 %s: %w", m.ID, err)
		}
		if _, dup := seen[field.Key]; dup {
			return fmt.Errorf("%w: 插件 %s 的配置项 %s 重复", ErrInvalidManifest, m.ID, field.Key)
		}
		seen[field.Key] = struct{}{}
	}
	return nil
}

// validate 校验单个配置项的定义。
func (f Field) validate() error {
	if !fieldKeyPattern.MatchString(f.Key) {
		return fmt.Errorf("%w: 配置项键 %q 必须匹配 %s", ErrInvalidManifest, f.Key, fieldKeyPattern)
	}
	switch f.Type {
	case FieldString, FieldTextarea, FieldPassword, FieldInteger, FieldNumber, FieldBoolean:
	case FieldSelect:
		if len(f.Options) == 0 {
			return fmt.Errorf("%w: 配置项 %s 是 select，但没有可选值", ErrInvalidManifest, f.Key)
		}
		seen := make(map[string]struct{}, len(f.Options))
		for _, option := range f.Options {
			if strings.TrimSpace(option.Value) == "" {
				return fmt.Errorf("%w: 配置项 %s 存在空的可选值", ErrInvalidManifest, f.Key)
			}
			if _, dup := seen[option.Value]; dup {
				return fmt.Errorf("%w: 配置项 %s 的可选值 %s 重复", ErrInvalidManifest, f.Key, option.Value)
			}
			seen[option.Value] = struct{}{}
		}
	default:
		return fmt.Errorf("%w: 配置项 %s 的类型 %q 未知", ErrInvalidManifest, f.Key, f.Type)
	}
	if f.Default == nil {
		return nil
	}
	if _, err := f.coerce(f.Default); err != nil {
		return fmt.Errorf("%w: 配置项 %s 的默认值不合法: %v", ErrInvalidManifest, f.Key, err)
	}
	return nil
}

// coerce 把一个外部值按声明类型归一，返回写进配置的那个值。
func (f Field) coerce(value any) (any, error) {
	switch f.Type {
	case FieldString, FieldTextarea, FieldPassword:
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("期望字符串，实际是 %T", value)
		}
		if len(text) > maxConfigStringLength {
			return nil, fmt.Errorf("长度 %d 超过上限 %d", len(text), maxConfigStringLength)
		}
		return text, nil
	case FieldInteger:
		return toInt64(value)
	case FieldNumber:
		return toFloat64(value)
	case FieldBoolean:
		flag, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("期望布尔值，实际是 %T", value)
		}
		return flag, nil
	case FieldSelect:
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("期望字符串，实际是 %T", value)
		}
		for _, option := range f.Options {
			if option.Value == text {
				return text, nil
			}
		}
		return nil, fmt.Errorf("%q 不在可选值内", text)
	default:
		return nil, fmt.Errorf("类型 %q 未知", f.Type)
	}
}

// requiresNonEmpty 判断某个类型是否适用「必填 = 不能为空」。
// 布尔与 select 不适用：false 是合法取值，select 的可选值在清单校验时已排除空串。
func (f Field) requiresNonEmpty() bool {
	switch f.Type {
	case FieldString, FieldTextarea, FieldPassword, FieldInteger, FieldNumber:
		return true
	default:
		return false
	}
}

// isZero 判断一个已归一的值是否为空，用于必填校验。
func isZero(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) == ""
	case int64:
		return typed == 0
	case float64:
		return typed == 0
	default:
		return value == nil
	}
}

// defaultsConfig 返回清单里声明的默认配置。没有默认值的可选项不会出现，
// 这样「没配」和「配了空串」在存储上可区分。
func defaultsConfig(m Manifest) Config {
	config := make(Config, len(m.Fields))
	for _, field := range m.Fields {
		if field.Default != nil {
			config[field.Key] = field.Default
		}
	}
	return config
}

// normalizeConfig 按清单校验外部配置并补齐默认值。未知键直接拒绝：
// 静默丢弃会让调用方以为配置生效了。
func normalizeConfig(m Manifest, raw map[string]any) (Config, error) {
	fields := make(map[string]Field, len(m.Fields))
	for _, field := range m.Fields {
		fields[field.Key] = field
	}
	for key := range raw {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("%w: 插件 %s 没有配置项 %s", ErrInvalidConfig, m.ID, key)
		}
	}
	config := make(Config, len(m.Fields))
	for _, field := range m.Fields {
		value, provided := raw[field.Key]
		if !provided || value == nil {
			if field.Default != nil {
				config[field.Key] = field.Default
				continue
			}
			if field.Required {
				return nil, fmt.Errorf("%w: 插件 %s 缺少必填配置 %s", ErrInvalidConfig, m.ID, field.Key)
			}
			continue
		}
		normalized, err := field.coerce(value)
		if err != nil {
			return nil, fmt.Errorf("%w: 插件 %s 的配置项 %s: %v", ErrInvalidConfig, m.ID, field.Key, err)
		}
		if field.Required && field.requiresNonEmpty() && isZero(normalized) {
			return nil, fmt.Errorf("%w: 插件 %s 的必填配置 %s 不能为空", ErrInvalidConfig, m.ID, field.Key)
		}
		config[field.Key] = normalized
	}
	return config, nil
}

// toInt64 接受 JSON 解出的 float64、显式整型和 json.Number，拒绝带小数的值：
// 静默截断会让「3.7 小时」变成 3 小时而无人察觉。
func toInt64(value any) (any, error) {
	switch typed := value.(type) {
	case int:
		return int64(typed), nil
	case int32:
		return int64(typed), nil
	case int64:
		return typed, nil
	case float64:
		if typed != float64(int64(typed)) {
			return nil, fmt.Errorf("期望整数，实际是 %v", typed)
		}
		return int64(typed), nil
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return nil, fmt.Errorf("期望整数，实际是 %v", typed)
		}
		return parsed, nil
	case string:
		// 浏览器里的数字输入框（VTextField type="number"）交回的是字符串，
		// 不认就等于「界面上改不了数字配置」。解析失败仍然报错，不放过 "abc"/"3.5"。
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("期望整数，实际是 %q", typed)
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("期望整数，实际是 %T", value)
	}
}

// toFloat64 接受 JSON 解出的 float64、整型和 json.Number。
func toFloat64(value any) (any, error) {
	switch typed := value.(type) {
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case float64:
		return typed, nil
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return nil, fmt.Errorf("期望数字，实际是 %v", typed)
		}
		return parsed, nil
	case string:
		// 同 toInt64：数字输入框给的是字符串。
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return nil, fmt.Errorf("期望数字，实际是 %q", typed)
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("期望数字，实际是 %T", value)
	}
}
