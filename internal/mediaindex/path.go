package mediaindex

import "strings"

// EqualRemotePath compares paths using the target server's configured path
// syntax, never the host OS filepath rules.
func EqualRemotePath(left, right, style string, caseSensitive bool) bool {
	left = normalizeRemotePath(left, style)
	right = normalizeRemotePath(right, style)
	if !caseSensitive {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func normalizeRemotePath(value, style string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(style, "windows") {
		value = strings.ReplaceAll(value, "/", `\`)
		for strings.Contains(value, `\\`) {
			value = strings.ReplaceAll(value, `\\`, `\`)
		}
		return strings.TrimRight(value, `\`)
	}
	value = strings.ReplaceAll(value, `\`, "/")
	for strings.Contains(value, "//") {
		value = strings.ReplaceAll(value, "//", "/")
	}
	if value != "/" {
		value = strings.TrimRight(value, "/")
	}
	return value
}
