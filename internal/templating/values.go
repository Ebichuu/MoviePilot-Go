package templating

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// truthy 复刻 Jinja2 的真值规则：None/0/空字符串/空容器为假。
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return reflected.Len() > 0
	}
	return true
}

func equalsValue(left, right any) bool {
	return stringify(left) == stringify(right)
}

func compareValues(operator string, left, right any) (any, error) {
	leftNumber, leftOK := numericValue(left)
	rightNumber, rightOK := numericValue(right)
	if leftOK && rightOK {
		switch operator {
		case "<":
			return leftNumber < rightNumber, nil
		case "<=":
			return leftNumber <= rightNumber, nil
		case ">":
			return leftNumber > rightNumber, nil
		default:
			return leftNumber >= rightNumber, nil
		}
	}
	leftText, rightText := stringify(left), stringify(right)
	switch operator {
	case "<":
		return leftText < rightText, nil
	case "<=":
		return leftText <= rightText, nil
	case ">":
		return leftText > rightText, nil
	default:
		return leftText >= rightText, nil
	}
}

func containsValue(container, item any) bool {
	switch typed := container.(type) {
	case string:
		return strings.Contains(typed, stringify(item))
	case []any:
		for _, entry := range typed {
			if equalsValue(entry, item) {
				return true
			}
		}
		return false
	}
	reflected := reflect.ValueOf(container)
	if reflected.Kind() == reflect.Slice || reflected.Kind() == reflect.Array {
		for index := 0; index < reflected.Len(); index++ {
			if equalsValue(reflected.Index(index).Interface(), item) {
				return true
			}
		}
	}
	return false
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	}
	return 0, false
}

func applyFilter(name string, value any, arguments []any) (any, error) {
	switch name {
	case "default", "d":
		if truthy(value) {
			return value, nil
		}
		if len(arguments) > 0 {
			return arguments[0], nil
		}
		return "", nil
	case "upper":
		return strings.ToUpper(stringify(value)), nil
	case "lower":
		return strings.ToLower(stringify(value)), nil
	case "trim":
		return strings.TrimSpace(stringify(value)), nil
	case "title":
		return strings.Title(strings.ToLower(stringify(value))), nil //nolint:staticcheck
	case "length", "count":
		return lengthValue(value), nil
	case "first":
		return indexValue(value, 0), nil
	case "last":
		return indexValue(value, -1), nil
	case "int":
		number, err := strconv.ParseInt(strings.TrimSpace(stringify(value)), 10, 64)
		if err != nil {
			return int64(0), nil
		}
		return number, nil
	case "float":
		number, err := strconv.ParseFloat(strings.TrimSpace(stringify(value)), 64)
		if err != nil {
			return float64(0), nil
		}
		return number, nil
	case "string":
		return stringify(value), nil
	case "join":
		separator := ""
		if len(arguments) > 0 {
			separator = stringify(arguments[0])
		}
		return joinValue(value, separator), nil
	case "replace":
		if len(arguments) < 2 {
			return nil, fmt.Errorf("replace 过滤器需要两个参数")
		}
		return strings.ReplaceAll(stringify(value), stringify(arguments[0]), stringify(arguments[1])), nil
	default:
		return nil, fmt.Errorf("不支持的过滤器 %q", name)
	}
}

func lengthValue(value any) int {
	if value == nil {
		return 0
	}
	if text, ok := value.(string); ok {
		return len([]rune(text))
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return reflected.Len()
	}
	return len([]rune(stringify(value)))
}

func indexValue(value any, index int) any {
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array {
		return nil
	}
	length := reflected.Len()
	if length == 0 {
		return nil
	}
	position := index
	if position < 0 {
		position = length + position
	}
	if position < 0 || position >= length {
		return nil
	}
	return reflected.Index(position).Interface()
}

func joinValue(value any, separator string) string {
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Slice && reflected.Kind() != reflect.Array {
		return stringify(value)
	}
	parts := make([]string, 0, reflected.Len())
	for index := 0; index < reflected.Len(); index++ {
		parts = append(parts, stringify(reflected.Index(index).Interface()))
	}
	return strings.Join(parts, separator)
}

func stringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}
