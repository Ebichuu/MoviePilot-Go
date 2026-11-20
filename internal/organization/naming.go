package organization

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/templating"
)

const (
	// DefaultMovieRenameFormat 对齐 MoviePilot V2 默认电影重命名格式。
	DefaultMovieRenameFormat = "{{title}}{% if year %} ({{year}}){% endif %}/{{title}}{% if year %} ({{year}}){% endif %}{% if part %}-{{part}}{% endif %}{% if videoFormat %} - {{videoFormat}}{% endif %}{{fileExt}}"
	// DefaultTVRenameFormat 对齐 MoviePilot V2 默认电视剧重命名格式。
	DefaultTVRenameFormat = "{{title}}{% if year %} ({{year}}){% endif %}/Season {{season}}/{{title}} - {{season_episode}}{% if part %}-{{part}}{% endif %}{% if episode %} - 第 {{episode}} 集{% endif %}{{fileExt}}"
)

// NamingTemplates 保存电影与电视剧的 Jinja2 命名模板；空值回退到 V2 默认模板。
type NamingTemplates struct {
	Movie string
	TV    string
}

// Effective 返回补齐默认值后的电影/剧集命名模板。
func (t NamingTemplates) Effective() NamingTemplates {
	out := NamingTemplates{
		Movie: strings.TrimSpace(t.Movie),
		TV:    strings.TrimSpace(t.TV),
	}
	if out.Movie == "" {
		out.Movie = DefaultMovieRenameFormat
	}
	if out.TV == "" {
		out.TV = DefaultTVRenameFormat
	}
	return out
}

// NamingInput 描述单文件命名模板渲染所需的媒体上下文。
type NamingInput struct {
	SourcePath string
	MediaType  string
	Title      string
	Year       string
	Category   string
	Season     int
	Episodes   []int
	TMDBID     string
	IMDBID     string
	// ExplicitTemplate 非空时优先使用该模板，不再按 MediaType 自动挑选。
	ExplicitTemplate string
}

// RenderRelativeTarget 使用 Jinja2 命名模板渲染相对于媒体库根目录的安全路径（POSIX 斜杠分隔）。
func RenderRelativeTarget(input NamingInput, templates NamingTemplates) (string, error) {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(input.SourcePath), `\`, "/"))
	if base == "" || base == "." || base == "/" {
		return "", errors.New("源文件名不能为空")
	}

	parsed := media.Parse(base, media.Options{})
	extWithDot := path.Ext(base)
	extNoDot := strings.TrimPrefix(extWithDot, ".")

	mediaType := normalizeNamingMediaType(input.MediaType, parsed.Type)
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = strings.TrimSpace(parsed.Name())
	}
	if title == "" {
		title = strings.TrimSuffix(base, extWithDot)
	}
	year := strings.TrimSpace(input.Year)
	if year == "" {
		year = strings.TrimSpace(parsed.Year)
	}

	seasonNum := input.Season
	if seasonNum <= 0 {
		seasonNum = parsed.BeginSeason
	}
	if mediaType == "电视剧" && seasonNum <= 0 {
		seasonNum = 1
	}

	episodeNum := 0
	if len(input.Episodes) > 0 && input.Episodes[0] > 0 {
		episodeNum = input.Episodes[0]
	} else if parsed.BeginEpisode > 0 {
		episodeNum = parsed.BeginEpisode
	}

	seasonStr := ""
	seasonEpisode := ""
	if seasonNum > 0 {
		seasonStr = strconv.Itoa(seasonNum)
		seasonEpisode = fmt.Sprintf("S%02d", seasonNum)
	}
	episodeStr := ""
	if len(input.Episodes) > 1 {
		episodeStr = strconv.Itoa(input.Episodes[0])
		seasonEpisode = fmt.Sprintf("S%02dE%02d-E%02d", max(seasonNum, 1), input.Episodes[0], input.Episodes[len(input.Episodes)-1])
	} else if episodeNum > 0 {
		episodeStr = strconv.Itoa(episodeNum)
		if parsed.EndEpisode > episodeNum && len(input.Episodes) == 0 {
			seasonEpisode = fmt.Sprintf("S%02dE%02d-E%02d", max(seasonNum, 1), episodeNum, parsed.EndEpisode)
		} else {
			seasonEpisode = fmt.Sprintf("S%02dE%02d", max(seasonNum, 1), episodeNum)
		}
	}

	ctx := map[string]any{
		"title":          title,
		"en_title":       strings.TrimSpace(parsed.EnglishName),
		"cn_title":       strings.TrimSpace(parsed.ChineseName),
		"original_name":  base,
		"file":           base,
		"year":           year,
		"type":           mediaType,
		"category":       strings.TrimSpace(input.Category),
		"season":         seasonStr,
		"episode":        episodeStr,
		"season_episode": seasonEpisode,
		"videoFormat":    strings.TrimSpace(parsed.Resolution),
		"videoCodec":     strings.TrimSpace(parsed.VideoEncode),
		"audioCodec":     strings.TrimSpace(parsed.AudioEncode),
		"resourceType":   strings.TrimSpace(parsed.ResourceType),
		"effect":         strings.TrimSpace(parsed.ResourceEffect),
		"part":           strings.TrimSpace(parsed.Part),
		"fileExt":        extWithDot,
		"ext":            extNoDot,
		"tmdbid":         strings.TrimSpace(input.TMDBID),
		"imdbid":         strings.TrimSpace(input.IMDBID),
	}

	eff := templates.Effective()
	tmpl := strings.TrimSpace(input.ExplicitTemplate)
	if tmpl == "" {
		if mediaType == "电视剧" {
			tmpl = eff.TV
		} else {
			tmpl = eff.Movie
		}
	}

	rendered, err := templating.Render(tmpl, ctx)
	if err != nil {
		return "", fmt.Errorf("渲染命名模板失败: %w", err)
	}
	return sanitizeRenderedRelativePath(rendered, extWithDot)
}

func normalizeNamingMediaType(explicit string, parsed media.MediaType) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "tv", "电视剧":
		return "电视剧"
	case "movie", "电影":
		return "电影"
	}
	if parsed == media.TypeTV {
		return "电视剧"
	}
	return "电影"
}

func sanitizeRenderedRelativePath(rendered, fallbackExt string) (string, error) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(rendered), `\`, "/")
	if cleaned == "" || strings.ContainsRune(cleaned, '\x00') {
		return "", errors.New("命名模板渲染结果为空或包含非法字符")
	}
	rawSegments := strings.Split(cleaned, "/")
	segments := make([]string, 0, len(rawSegments))
	for _, seg := range rawSegments {
		if strings.TrimSpace(seg) == ".." {
			return "", errors.New("命名模板结果不得包含上级目录跳转")
		}
		seg = sanitizePathSegment(seg)
		if seg == "" || seg == "." {
			continue
		}
		segments = append(segments, seg)
	}
	if len(segments) == 0 {
		return "", errors.New("命名模板渲染结果不包含有效文件名")
	}
	last := segments[len(segments)-1]
	if fallbackExt != "" && !strings.HasSuffix(strings.ToLower(last), strings.ToLower(fallbackExt)) {
		segments[len(segments)-1] = last + fallbackExt
	}
	rel := path.Clean(strings.Join(segments, "/"))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") {
		return "", errors.New("命名模板生成的相对路径非法")
	}
	return rel, nil
}

var illegalSegmentReplacer = strings.NewReplacer(
	":", " ",
	"*", " ",
	"?", "",
	"\"", "",
	"<", "",
	">", "",
	"|", " ",
)

func sanitizePathSegment(segment string) string {
	segment = illegalSegmentReplacer.Replace(segment)
	segment = strings.Join(strings.Fields(segment), " ")
	segment = strings.Trim(segment, ". ")
	return segment
}

// IsVideoFilename 判断文件名是否为受支持的视频媒体扩展名。
func IsVideoFilename(filename string) bool {
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(filename))) {
	case ".mkv", ".mp4", ".avi", ".mov", ".ts", ".m2ts", ".iso", ".rmvb", ".wmv", ".flv", ".webm", ".strm":
		return true
	default:
		return false
	}
}
