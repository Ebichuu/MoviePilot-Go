package metadata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

const maxCategoryConfigBytes = 1 << 20

// ErrInvalidCategoryConfig 表示分类配置的 JSON/YAML 结构无法被 V2 分类器使用。
var ErrInvalidCategoryConfig = errors.New("分类策略配置无效")

// CategoryConfig 保留 movie/tv 下分类规则的文件顺序。
type CategoryConfig struct {
	Movie []CategoryRule
	TV    []CategoryRule

	movieNull bool
	tvNull    bool
}

// CategoryRule 是一个有序分类名及其 AND 条件。
type CategoryRule struct {
	Name       string
	Conditions []CategoryCondition
}

// CategoryCondition 保存 V2 支持的条件名和原始字符串值。
type CategoryCondition struct {
	Attribute string
	Value     string
}

// MarshalJSON 输出 V2 CategoryConfig 使用的有序对象形状。
func (c CategoryConfig) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteString(`{"movie":`)
	if err := writeRulesJSON(&buffer, c.Movie, c.movieNull); err != nil {
		return nil, err
	}
	buffer.WriteString(`,"tv":`)
	if err := writeRulesJSON(&buffer, c.TV, c.tvNull); err != nil {
		return nil, err
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// UnmarshalJSON 接受 V2 CategoryConfig 对象并保留规则顺序。
func (c *CategoryConfig) UnmarshalJSON(content []byte) error {
	root := new(yaml.Node)
	if err := yaml.Unmarshal(content, root); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCategoryConfig, err)
	}
	parsed, err := categoryConfigFromYAML(root)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// CategoryService 管理 category.yaml 的磁盘表示和运行期不可变快照。
type CategoryService struct {
	path string
	load func(string) (CategoryConfig, error)
	// beforeSaveLock is a deterministic synchronization hook for package tests.
	beforeSaveLock func()

	mu      sync.RWMutex
	current CategoryConfig
}

// NewCategoryService 建立 V2 category.yaml 服务。初始化失败按 V2 保持空分类，不阻止进程启动。
func NewCategoryService(configDir string) *CategoryService {
	service := &CategoryService{path: filepath.Join(configDir, "category.yaml"), load: readCategoryConfig}
	if _, err := os.Stat(service.path); errors.Is(err, os.ErrNotExist) {
		_ = service.write(defaultCategoryConfig())
	}
	_ = service.Reload()
	return service
}

// LoadCategoryConfig 每次读取磁盘；缺失或损坏时按 V2 返回空配置。
func (s *CategoryService) LoadCategoryConfig() CategoryConfig {
	config, err := readCategoryConfig(s.path)
	if err != nil {
		return CategoryConfig{}
	}
	return config
}

// SaveCategoryConfig 保存配置并立即替换运行期快照。
func (s *CategoryService) SaveCategoryConfig(config CategoryConfig) error {
	if s == nil {
		return errors.New("分类策略服务未初始化")
	}
	if s.beforeSaveLock != nil {
		s.beforeSaveLock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(config); err != nil {
		return err
	}
	s.current = cloneCategoryConfig(config)
	return nil
}

// Reload 从磁盘重载；损坏或缺失时保留之前已生效的快照。
func (s *CategoryService) Reload() error {
	if s == nil {
		return errors.New("分类策略服务未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	config, err := s.load(s.path)
	if err != nil {
		return err
	}
	s.current = cloneCategoryConfig(config)
	return nil
}

// Category 使用当前快照执行 V2 首个匹配规则。
func (s *CategoryService) Category(mediaType string, item Media) string {
	if s == nil {
		return V2Category(mediaType, item)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return categoryFromConfig(s.current, mediaType, item)
}

// MediaCategories 返回 V2 /media/category 的中文媒体类型键和值顺序。
func (s *CategoryService) MediaCategories() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string][]string{
		"电影":  categoryNames(s.current.Movie),
		"电视剧": categoryNames(s.current.TV),
	}
}

func (s *CategoryService) write(config CategoryConfig) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建分类配置目录: %w", err)
	}
	content, err := marshalCategoryYAML(config)
	if err != nil {
		return err
	}
	if len(content) > maxCategoryConfigBytes {
		return fmt.Errorf("%w: 序列化后的 YAML 超过 %d 字节", ErrInvalidCategoryConfig, maxCategoryConfigBytes)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".category-*.yaml")
	if err != nil {
		return fmt.Errorf("创建分类配置临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("写入分类配置: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭分类配置: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("替换分类配置: %w", err)
	}
	return nil
}

func readCategoryConfig(path string) (CategoryConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return CategoryConfig{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxCategoryConfigBytes+1))
	if err != nil {
		return CategoryConfig{}, err
	}
	if len(content) > maxCategoryConfigBytes {
		return CategoryConfig{}, ErrInvalidCategoryConfig
	}
	root := new(yaml.Node)
	if err := yaml.Unmarshal(content, root); err != nil {
		return CategoryConfig{}, fmt.Errorf("%w: %v", ErrInvalidCategoryConfig, err)
	}
	return categoryConfigFromYAML(root)
}

func categoryConfigFromYAML(root *yaml.Node) (CategoryConfig, error) {
	if root == nil || len(root.Content) == 0 {
		return CategoryConfig{}, nil
	}
	node := root
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return CategoryConfig{}, ErrInvalidCategoryConfig
	}
	var result CategoryConfig
	for index := 0; index+1 < len(node.Content); index += 2 {
		switch node.Content[index].Value {
		case "movie":
			result.movieNull = node.Content[index+1].Tag == "!!null"
			rules, err := rulesFromYAML(node.Content[index+1])
			if err != nil {
				return CategoryConfig{}, err
			}
			result.Movie = rules
		case "tv":
			result.tvNull = node.Content[index+1].Tag == "!!null"
			rules, err := rulesFromYAML(node.Content[index+1])
			if err != nil {
				return CategoryConfig{}, err
			}
			result.TV = rules
		}
	}
	return result, nil
}

func rulesFromYAML(node *yaml.Node) ([]CategoryRule, error) {
	if node.Tag == "!!null" {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, ErrInvalidCategoryConfig
	}
	rules := make([]CategoryRule, 0, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		name := node.Content[index].Value
		if name == "" {
			return nil, ErrInvalidCategoryConfig
		}
		ruleNode := node.Content[index+1]
		rule := CategoryRule{Name: name}
		if ruleNode.Tag != "!!null" {
			if ruleNode.Kind != yaml.MappingNode {
				return nil, ErrInvalidCategoryConfig
			}
			for conditionIndex := 0; conditionIndex+1 < len(ruleNode.Content); conditionIndex += 2 {
				valueNode := ruleNode.Content[conditionIndex+1]
				if valueNode.Tag == "!!null" {
					continue
				}
				if valueNode.Kind != yaml.ScalarNode {
					return nil, ErrInvalidCategoryConfig
				}
				rule.Conditions = append(rule.Conditions, CategoryCondition{
					Attribute: ruleNode.Content[conditionIndex].Value,
					Value:     valueNode.Value,
				})
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func marshalCategoryYAML(config CategoryConfig) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	mapping := root.Content[0]
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "movie"}, rulesToYAML(config.Movie),
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "tv"}, rulesToYAML(config.TV),
	)
	var content bytes.Buffer
	content.WriteString(categoryHeader)
	encoder := yaml.NewEncoder(&content)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		_ = encoder.Close()
		return nil, fmt.Errorf("编码分类配置: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("关闭分类配置编码器: %w", err)
	}
	return content.Bytes(), nil
}

func rulesToYAML(rules []CategoryRule) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, rule := range rules {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: rule.Name})
		if len(rule.Conditions) == 0 {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: ""})
			continue
		}
		conditions := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, condition := range rule.Conditions {
			conditions.Content = append(conditions.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: condition.Attribute},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: condition.Value, Style: yaml.SingleQuotedStyle},
			)
		}
		node.Content = append(node.Content, conditions)
	}
	return node
}

func writeRulesJSON(buffer *bytes.Buffer, rules []CategoryRule, null bool) error {
	if null {
		buffer.WriteString("null")
		return nil
	}
	buffer.WriteByte('{')
	for index, rule := range rules {
		if index > 0 {
			buffer.WriteByte(',')
		}
		name, err := json.Marshal(rule.Name)
		if err != nil {
			return err
		}
		buffer.Write(name)
		buffer.WriteByte(':')
		if len(rule.Conditions) == 0 {
			buffer.WriteString("null")
			continue
		}
		buffer.WriteByte('{')
		for conditionIndex, condition := range rule.Conditions {
			if conditionIndex > 0 {
				buffer.WriteByte(',')
			}
			attribute, _ := json.Marshal(condition.Attribute)
			value, _ := json.Marshal(condition.Value)
			buffer.Write(attribute)
			buffer.WriteByte(':')
			buffer.Write(value)
		}
		buffer.WriteByte('}')
	}
	buffer.WriteByte('}')
	return nil
}

func categoryFromConfig(config CategoryConfig, mediaType string, item Media) string {
	switch normalizeMediaType(mediaType) {
	case "movie":
		return evaluateV2Category(toV2Rules(config.Movie), item)
	case "tv":
		return evaluateV2Category(toV2Rules(config.TV), item)
	default:
		return ""
	}
}

func toV2Rules(rules []CategoryRule) []v2CategoryRule {
	result := make([]v2CategoryRule, 0, len(rules))
	for _, rule := range rules {
		conditions := make([]v2CategoryCondition, 0, len(rule.Conditions))
		for _, condition := range rule.Conditions {
			conditions = append(conditions, v2CategoryCondition{attribute: condition.Attribute, value: condition.Value})
		}
		result = append(result, v2CategoryRule{name: rule.Name, conditions: conditions})
	}
	return result
}

func categoryNames(rules []CategoryRule) []string {
	result := make([]string, 0, len(rules))
	for _, rule := range rules {
		result = append(result, rule.Name)
	}
	return result
}

func cloneCategoryConfig(config CategoryConfig) CategoryConfig {
	cloneRules := func(rules []CategoryRule) []CategoryRule {
		result := make([]CategoryRule, len(rules))
		for index, rule := range rules {
			result[index] = CategoryRule{Name: rule.Name, Conditions: append([]CategoryCondition(nil), rule.Conditions...)}
		}
		return result
	}
	return CategoryConfig{
		Movie: cloneRules(config.Movie), TV: cloneRules(config.TV),
		movieNull: config.movieNull, tvNull: config.tvNull,
	}
}

func defaultCategoryConfig() CategoryConfig {
	return CategoryConfig{Movie: fromV2Rules(v2MovieCategoryRules), TV: fromV2Rules(v2TVCategoryRules)}
}

func fromV2Rules(rules []v2CategoryRule) []CategoryRule {
	result := make([]CategoryRule, 0, len(rules))
	for _, rule := range rules {
		conditions := make([]CategoryCondition, 0, len(rule.conditions))
		for _, condition := range rule.conditions {
			conditions = append(conditions, CategoryCondition{Attribute: condition.attribute, Value: condition.value})
		}
		result = append(result, CategoryRule{Name: rule.name, Conditions: conditions})
	}
	return result
}

const categoryHeader = "####### 配置说明 #######\n# movie、tv 为固定一级名称；二级分类按文件顺序首个匹配生效。\n# 多项条件同时满足，逗号分隔候选，! 表示排除，数字范围使用 YYYY-YYYY。\n\n"
