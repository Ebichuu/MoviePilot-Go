package storage

import (
	"sort"
	"strconv"
	"strings"
)

// sortItemsByName 使用 V2 的 `natural_sort_key` 语义按名称排序。
func sortItemsByName(items []Item) {
	sort.SliceStable(items, func(i, j int) bool { return naturalLess(items[i].Name, items[j].Name) })
}

// sortItemsByModifyTime 按修改时间倒序（V2 默认排序）。
func sortItemsByModifyTime(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		return modifyTimeValue(items[i]) > modifyTimeValue(items[j])
	})
}

func modifyTimeValue(item Item) float64 {
	if item.ModifyTime == nil {
		return 0
	}
	return *item.ModifyTime
}

func sortStrings(values []string) {
	sort.Strings(values)
}

// globMatch 复刻 V2 `fnmatch.translate(keyword)` 的 `*`/`?`/`[...]` 语义
// （`app/api/endpoints/storage.py:108`，调用方已小写化并做全串匹配）。
func globMatch(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 1 && pattern[1] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 1 {
				return true
			}
			for index := 0; index <= len(name); index++ {
				if globMatch(pattern[1:], name[index:]) {
					return true
				}
			}
			return false
		case '?':
			if len(name) == 0 {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		case '[':
			if len(name) == 0 {
				return false
			}
			matched, remaining, ok := matchCharacterClass(pattern, name[0])
			if !ok {
				if name[0] != '[' {
					return false
				}
				pattern, name = pattern[1:], name[1:]
				continue
			}
			if !matched {
				return false
			}
			pattern, name = remaining, name[1:]
		default:
			if len(name) == 0 || pattern[0] != name[0] {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		}
	}
	return len(name) == 0
}

func matchCharacterClass(pattern string, character byte) (bool, string, bool) {
	end := strings.IndexByte(pattern[1:], ']')
	if end < 0 {
		return false, pattern, false
	}
	body := pattern[1 : 1+end]
	remaining := pattern[2+end:]
	negate := strings.HasPrefix(body, "!")
	if negate {
		body = body[1:]
	}
	matched := false
	for index := 0; index < len(body); index++ {
		if index+2 < len(body) && body[index+1] == '-' {
			if character >= body[index] && character <= body[index+2] {
				matched = true
			}
			index += 2
			continue
		}
		if body[index] == character {
			matched = true
		}
	}
	if negate {
		matched = !matched
	}
	return matched, remaining, true
}

// naturalLess 复刻 V2 `StringUtils.natural_sort_key` 的数字感知排序。
func naturalLess(left, right string) bool {
	leftChunks := naturalChunks(strings.ToLower(left))
	rightChunks := naturalChunks(strings.ToLower(right))
	for index := 0; index < len(leftChunks) && index < len(rightChunks); index++ {
		if leftChunks[index] == rightChunks[index] {
			continue
		}
		return leftChunks[index] < rightChunks[index]
	}
	return len(leftChunks) < len(rightChunks)
}

func naturalChunks(value string) []string {
	chunks := make([]string, 0, 8)
	var builder strings.Builder
	digits := false
	flush := func() {
		if builder.Len() == 0 {
			return
		}
		chunk := builder.String()
		if digits {
			if number, err := strconv.ParseInt(chunk, 10, 64); err == nil {
				chunk = "\x00" + paddedNumber(number)
			}
		}
		chunks = append(chunks, chunk)
		builder.Reset()
	}
	for index := 0; index < len(value); index++ {
		isDigit := value[index] >= '0' && value[index] <= '9'
		if builder.Len() > 0 && isDigit != digits {
			flush()
		}
		digits = isDigit
		builder.WriteByte(value[index])
	}
	flush()
	return chunks
}

func paddedNumber(number int64) string {
	text := strconv.FormatInt(number, 10)
	if len(text) >= 19 {
		return text
	}
	return strings.Repeat("0", 19-len(text)) + text
}

var _ Provider = (*LocalProvider)(nil)
