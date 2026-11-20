package templating

import (
	"fmt"
	"reflect"
	"strings"
)

// Render 渲染 V2 通知模板子集：`{{ 表达式 }}`、`{% if/elif/else/endif %}`、
// `{% for x in list %}{% endfor %}`，以及一组常用过滤器。未定义变量按 Jinja2
// 默认 Undefined 渲染为空串且为假；遇到不支持的语法返回错误而不是静默忽略。
func Render(source string, context map[string]any) (string, error) {
	segments, err := parseTemplate(source)
	if err != nil {
		return "", err
	}
	index := 0
	nodes, tag, _, err := buildNodes(segments, &index, nil)
	if err != nil {
		return "", err
	}
	if tag != "" {
		return "", fmt.Errorf("模板包含未匹配的 %q", tag)
	}
	scope := map[string]any{}
	mergeContext(scope, context)
	var builder strings.Builder
	if err := renderNodes(nodes, scope, &builder); err != nil {
		return "", err
	}
	return builder.String(), nil
}

func mergeContext(dst, src map[string]any) {
	for key, value := range src {
		dst[key] = value
	}
}

func renderNodes(nodes []node, scope map[string]any, out *strings.Builder) error {
	for _, current := range nodes {
		switch typed := current.(type) {
		case textNode:
			out.WriteString(typed.text)
		case exprNode:
			value, err := evalExpression(typed.expression, scope)
			if err != nil {
				return err
			}
			out.WriteString(stringify(value))
		case ifNode:
			if err := renderIfNode(typed, scope, out); err != nil {
				return err
			}
		case forNode:
			if err := renderForNode(typed, scope, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderIfNode(node ifNode, scope map[string]any, out *strings.Builder) error {
	for _, branch := range node.branches {
		if branch.condition == "" {
			return renderNodes(branch.body, scope, out)
		}
		value, err := evalExpression(branch.condition, scope)
		if err != nil {
			return err
		}
		if truthy(value) {
			return renderNodes(branch.body, scope, out)
		}
	}
	return nil
}

func renderForNode(node forNode, scope map[string]any, out *strings.Builder) error {
	iterable, err := evalExpression(node.iterable, scope)
	if err != nil {
		return err
	}
	items, err := iterateValue(iterable)
	if err != nil {
		return err
	}
	for _, item := range items {
		child := map[string]any{}
		mergeContext(child, scope)
		if err := bindLoopVariables(node.variables, item, child); err != nil {
			return err
		}
		if err := renderNodes(node.body, child, out); err != nil {
			return err
		}
	}
	return nil
}

func bindLoopVariables(variables []string, item any, scope map[string]any) error {
	if len(variables) == 1 {
		scope[variables[0]] = item
		return nil
	}
	reflected := reflect.ValueOf(item)
	if reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array {
		return fmt.Errorf("for 循环解包需要序列元素")
	}
	if reflected.Len() < len(variables) {
		return fmt.Errorf("for 循环解包元素过少")
	}
	for index, name := range variables {
		scope[name] = reflected.Index(index).Interface()
	}
	return nil
}

func iterateValue(value any) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	if mapValue, ok := value.(map[string]any); ok {
		keys := make([]string, 0, len(mapValue))
		for key := range mapValue {
			keys = append(keys, key)
		}
		sortStrings(keys)
		items := make([]any, 0, len(keys))
		for _, key := range keys {
			items = append(items, key)
		}
		return items, nil
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array:
		items := make([]any, 0, reflected.Len())
		for index := 0; index < reflected.Len(); index++ {
			items = append(items, reflected.Index(index).Interface())
		}
		return items, nil
	case reflect.String:
		text := reflected.String()
		items := make([]any, 0, len(text))
		for _, character := range text {
			items = append(items, string(character))
		}
		return items, nil
	default:
		return nil, fmt.Errorf("for 循环对象不可迭代")
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func evalExpression(source string, context map[string]any) (any, error) {
	tokens, err := tokenizeExpression(source)
	if err != nil {
		return nil, err
	}
	parser := &exprParser{tokens: tokens, context: context}
	return parser.parse()
}
