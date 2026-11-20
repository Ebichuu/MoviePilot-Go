package mediaconfig

import (
	"errors"
	"strings"
	"unicode"
)

var (
	// ErrInvalidMediaPath 表示待映射路径不是受支持的绝对路径。
	ErrInvalidMediaPath = errors.New("媒体路径无效")
)

// PathStyle 描述 Emby 端路径使用的分隔符和卷格式。
type PathStyle string

const (
	// PathStylePOSIX 表示 /library/movie.mkv 形式的路径。
	PathStylePOSIX PathStyle = "posix"
	// PathStyleWindows 表示 D:\Media\movie.mkv 或 UNC 形式的路径。
	PathStyleWindows PathStyle = "windows"
)

// CasePolicy 描述路径前缀匹配的大小写策略。
type CasePolicy string

const (
	// CasePolicySensitive 表示路径段区分大小写。
	CasePolicySensitive CasePolicy = "sensitive"
	// CasePolicyInsensitive 表示路径段不区分大小写。
	CasePolicyInsensitive CasePolicy = "insensitive"
)

// PathMapping 将一个本地绝对根目录映射到 Emby 可见的绝对根目录。
type PathMapping struct {
	LocalRoot   string     `json:"local_root"`
	EmbyRoot    string     `json:"emby_root"`
	RemoteStyle PathStyle  `json:"remote_style"`
	CasePolicy  CasePolicy `json:"case_policy"`
}

// MapPathResult 是一次最长前缀映射实际命中的远端路径及比较策略。
type MapPathResult struct {
	Path       string     `json:"path"`
	Style      PathStyle  `json:"style"`
	CasePolicy CasePolicy `json:"case_policy"`
}

type lexicalPath struct {
	style    PathStyle
	volume   string
	segments []string
}

// MapPath 按最长本地路径段前缀映射到 Emby 路径。
// 比较以路径段为边界，因此 /media 不会误匹配 /media2。
func MapPath(mappings []PathMapping, localPath string) (string, bool, error) {
	result, ok, err := ResolvePath(mappings, localPath)
	return result.Path, ok, err
}

// ResolvePath 按最长本地路径段前缀映射，并返回实际命中的远端策略。
func ResolvePath(mappings []PathMapping, localPath string) (MapPathResult, bool, error) {
	normalized, _, err := normalizePathMappings(mappings)
	if err != nil {
		return MapPathResult{}, false, err
	}
	if len(normalized) == 0 {
		return MapPathResult{}, false, nil
	}

	bestDepth := -1
	bestResult := MapPathResult{}
	parsedAny := false
	for _, mapping := range normalized {
		localStyle := inferPathStyle(mapping.LocalRoot)
		candidate, parseErr := parseAbsolutePath(localPath, localStyle)
		if parseErr != nil {
			continue
		}
		parsedAny = true
		root, parseErr := parseAbsolutePath(mapping.LocalRoot, localStyle)
		if parseErr != nil || !hasPathPrefix(root, candidate, mapping.CasePolicy) {
			continue
		}
		if len(root.segments) <= bestDepth {
			continue
		}
		remoteRoot, parseErr := parseAbsolutePath(mapping.EmbyRoot, mapping.RemoteStyle)
		if parseErr != nil {
			return MapPathResult{}, false, invalid("Emby 路径根无效")
		}
		remoteRoot.segments = append(remoteRoot.segments, candidate.segments[len(root.segments):]...)
		bestDepth = len(root.segments)
		bestResult = MapPathResult{
			Path: remoteRoot.String(), Style: mapping.RemoteStyle, CasePolicy: mapping.CasePolicy,
		}
	}
	if !parsedAny {
		return MapPathResult{}, false, ErrInvalidMediaPath
	}
	if bestDepth == -1 {
		return MapPathResult{}, false, nil
	}
	return bestResult, true, nil
}

func normalizePathMappings(mappings []PathMapping) ([]PathMapping, bool, error) {
	if mappings == nil {
		return []PathMapping{}, true, nil
	}
	normalized := make([]PathMapping, len(mappings))
	changed := false
	for index, mapping := range mappings {
		next, itemChanged, err := normalizePathMapping(mapping)
		if err != nil {
			return nil, false, err
		}
		for previousIndex := 0; previousIndex < index; previousIndex++ {
			if equivalentLocalRoots(normalized[previousIndex], next) {
				return nil, false, invalid("本地路径映射根重复")
			}
		}
		normalized[index] = next
		changed = changed || itemChanged
	}
	return normalized, changed, nil
}

func normalizePathMapping(mapping PathMapping) (PathMapping, bool, error) {
	normalized := mapping
	normalized.LocalRoot = strings.TrimSpace(mapping.LocalRoot)
	normalized.EmbyRoot = strings.TrimSpace(mapping.EmbyRoot)
	normalized.RemoteStyle = PathStyle(strings.ToLower(strings.TrimSpace(string(mapping.RemoteStyle))))
	normalized.CasePolicy = CasePolicy(strings.ToLower(strings.TrimSpace(string(mapping.CasePolicy))))
	if normalized.LocalRoot == "" || normalized.EmbyRoot == "" {
		return PathMapping{}, false, invalid("路径映射根不能为空")
	}
	if normalized.RemoteStyle == "" {
		normalized.RemoteStyle = inferPathStyle(normalized.EmbyRoot)
	}
	if normalized.RemoteStyle != PathStylePOSIX && normalized.RemoteStyle != PathStyleWindows {
		return PathMapping{}, false, invalid("远端路径风格无效")
	}
	if normalized.CasePolicy == "" {
		normalized.CasePolicy = defaultCasePolicy(normalized.RemoteStyle)
	}
	if normalized.CasePolicy != CasePolicySensitive && normalized.CasePolicy != CasePolicyInsensitive {
		return PathMapping{}, false, invalid("路径大小写策略无效")
	}

	local, err := parseAbsolutePath(normalized.LocalRoot, inferPathStyle(normalized.LocalRoot))
	if err != nil {
		return PathMapping{}, false, invalid("本地路径根无效")
	}
	remote, err := parseAbsolutePath(normalized.EmbyRoot, normalized.RemoteStyle)
	if err != nil {
		return PathMapping{}, false, invalid("Emby 路径根无效")
	}
	normalized.LocalRoot = local.String()
	normalized.EmbyRoot = remote.String()
	return normalized, normalized != mapping, nil
}

func parseAbsolutePath(value string, style PathStyle) (lexicalPath, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return lexicalPath{}, ErrInvalidMediaPath
	}
	if style == PathStylePOSIX {
		if !strings.HasPrefix(value, "/") {
			return lexicalPath{}, ErrInvalidMediaPath
		}
		segments, err := cleanSegments(strings.Split(value[1:], "/"))
		if err != nil {
			return lexicalPath{}, err
		}
		return lexicalPath{style: style, volume: "/", segments: segments}, nil
	}
	if style != PathStyleWindows {
		return lexicalPath{}, ErrInvalidMediaPath
	}
	return parseWindowsAbsolutePath(value)
}

func parseWindowsAbsolutePath(value string) (lexicalPath, error) {
	value = strings.ReplaceAll(value, "/", `\`)
	if len(value) >= 3 && unicode.IsLetter(rune(value[0])) && value[1] == ':' && value[2] == '\\' {
		segments, err := cleanSegments(strings.Split(strings.TrimLeft(value[3:], `\`), `\`))
		if err != nil {
			return lexicalPath{}, err
		}
		return lexicalPath{
			style: PathStyleWindows, volume: strings.ToUpper(value[:1]) + ":", segments: segments,
		}, nil
	}
	if !strings.HasPrefix(value, `\\`) {
		return lexicalPath{}, ErrInvalidMediaPath
	}
	parts := nonEmptySegments(strings.Split(strings.TrimLeft(value, `\`), `\`))
	if len(parts) < 2 {
		return lexicalPath{}, ErrInvalidMediaPath
	}
	segments, err := cleanSegments(parts[2:])
	if err != nil {
		return lexicalPath{}, err
	}
	return lexicalPath{
		style: PathStyleWindows, volume: `\\` + parts[0] + `\` + parts[1], segments: segments,
	}, nil
}

func cleanSegments(parts []string) ([]string, error) {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(cleaned) == 0 {
				return nil, ErrInvalidMediaPath
			}
			cleaned = cleaned[:len(cleaned)-1]
		default:
			cleaned = append(cleaned, part)
		}
	}
	return cleaned, nil
}

func nonEmptySegments(parts []string) []string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func inferPathStyle(value string) PathStyle {
	value = strings.TrimSpace(value)
	if strings.Contains(value, `\`) || (len(value) >= 2 && unicode.IsLetter(rune(value[0])) && value[1] == ':') {
		return PathStyleWindows
	}
	return PathStylePOSIX
}

func defaultCasePolicy(style PathStyle) CasePolicy {
	if style == PathStyleWindows {
		return CasePolicyInsensitive
	}
	return CasePolicySensitive
}

func equivalentLocalRoots(left, right PathMapping) bool {
	leftStyle := inferPathStyle(left.LocalRoot)
	rightStyle := inferPathStyle(right.LocalRoot)
	if leftStyle != rightStyle {
		return false
	}
	leftPath, leftErr := parseAbsolutePath(left.LocalRoot, leftStyle)
	rightPath, rightErr := parseAbsolutePath(right.LocalRoot, rightStyle)
	if leftErr != nil || rightErr != nil || len(leftPath.segments) != len(rightPath.segments) {
		return false
	}
	policy := CasePolicySensitive
	if left.CasePolicy == CasePolicyInsensitive || right.CasePolicy == CasePolicyInsensitive {
		policy = CasePolicyInsensitive
	}
	return equalPathPart(leftPath.volume, rightPath.volume, policy) &&
		equalSegments(leftPath.segments, rightPath.segments, policy)
}

func hasPathPrefix(root, candidate lexicalPath, policy CasePolicy) bool {
	if root.style != candidate.style || !equalPathPart(root.volume, candidate.volume, policy) ||
		len(root.segments) > len(candidate.segments) {
		return false
	}
	return equalSegments(root.segments, candidate.segments[:len(root.segments)], policy)
}

func equalSegments(left, right []string, policy CasePolicy) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !equalPathPart(left[index], right[index], policy) {
			return false
		}
	}
	return true
}

func equalPathPart(left, right string, policy CasePolicy) bool {
	if policy == CasePolicyInsensitive {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func (p lexicalPath) String() string {
	if p.style == PathStylePOSIX {
		if len(p.segments) == 0 {
			return "/"
		}
		return "/" + strings.Join(p.segments, "/")
	}
	if len(p.segments) == 0 {
		if strings.HasPrefix(p.volume, `\\`) {
			return p.volume
		}
		return p.volume + `\`
	}
	return p.volume + `\` + strings.Join(p.segments, `\`)
}
