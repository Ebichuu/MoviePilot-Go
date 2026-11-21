package httpapi

import (
	"encoding/json"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type episodePatternCandidate struct {
	name     string
	re       *regexp.Regexp
	template func(match []string) string
}

var episodeFormatCandidates = []episodePatternCandidate{
	{
		name: "SxxExx",
		re:   regexp.MustCompile(`(?i)\b(S\d{1,2}E)(\d{1,4})\b`),
		template: func(m []string) string {
			return strings.ToUpper(m[1]) + "{ep}"
		},
	},
	{
		name: "EPxx",
		re:   regexp.MustCompile(`(?i)\b(EP)(\d{1,4})\b`),
		template: func(m []string) string {
			return strings.ToUpper(m[1]) + "{ep}"
		},
	},
	{
		name: "第xx集",
		re:   regexp.MustCompile(`第\s*(\d{1,4})\s*([集话話])`),
		template: func(m []string) string {
			return "第{ep}" + m[2]
		},
	},
	{
		name: "BracketEp",
		re:   regexp.MustCompile(`\[(\d{1,4})(?:v\d+)?\]`),
		template: func(_ []string) string {
			return "[{ep}]"
		},
	},
	{
		name: "DashEp",
		re:   regexp.MustCompile(`\s-\s(\d{1,4})(?:\s|\[|\.|$)`),
		template: func(_ []string) string {
			return " - {ep}"
		},
	},
	{
		name: "Exx",
		re:   regexp.MustCompile(`(?i)(?:^|[\s._-])(E)(\d{1,4})(?:$|[\s._-])`),
		template: func(m []string) string {
			return strings.ToUpper(m[1]) + "{ep}"
		},
	},
}

// recommendEpisodeFormat 对应 V2 `POST /transfer/episode-format/recommend`：
// 根据整季文件名清单分析最稳定的集号命名模式，返回 V2 兼容的 `{ep}` 定位格式与识别统计。
func (d Dependencies) recommendEpisodeFormat(w http.ResponseWriter, r *http.Request) {
	var raw any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "请提供有效的文件清单"})
		return
	}
	files := extractCandidateFileNames(raw)
	if len(files) == 0 {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "文件清单为空，无法推荐集号格式"})
		return
	}

	bestFormat := ""
	bestMatches := 0
	var bestEpisodes []int

	for _, cand := range episodeFormatCandidates {
		templateCounts := make(map[string]int)
		episodesByTpl := make(map[string][]int)
		for _, name := range files {
			m := cand.re.FindStringSubmatch(name)
			if len(m) == 0 {
				continue
			}
			// 集号捕获组总是第一个纯数字子串。
			epNum := 0
			for _, sub := range m[1:] {
				if parsed, err := strconv.Atoi(sub); err == nil && parsed > 0 {
					epNum = parsed
					break
				}
			}
			if epNum <= 0 {
				continue
			}
			tpl := cand.template(m)
			templateCounts[tpl]++
			episodesByTpl[tpl] = append(episodesByTpl[tpl], epNum)
		}
		for tpl, count := range templateCounts {
			if count > bestMatches {
				bestMatches = count
				bestFormat = tpl
				bestEpisodes = episodesByTpl[tpl]
			}
		}
	}

	if bestFormat == "" || bestMatches == 0 {
		writeJSON(w, http.StatusOK, Response{
			Success: false,
			Message: "未能从文件清单中识别到统一的集号格式",
		})
		return
	}

	sort.Ints(bestEpisodes)
	writeJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "集号格式推荐成功",
		Data: map[string]any{
			"episode_format": bestFormat,
			"matched_count":  bestMatches,
			"total_count":    len(files),
			"episodes":       bestEpisodes,
		},
	})
}

func extractCandidateFileNames(raw any) []string {
	var out []string
	var collect func(v any)
	collect = func(v any) {
		switch typed := v.(type) {
		case string:
			name := path.Base(strings.ReplaceAll(strings.TrimSpace(typed), `\`, "/"))
			if name != "" && name != "." && name != "/" {
				out = append(out, name)
			}
		case []any:
			for _, item := range typed {
				collect(item)
			}
		case map[string]any:
			for _, key := range []string{"files", "fileitems", "items", "paths", "names"} {
				if list, ok := typed[key]; ok {
					collect(list)
				}
			}
			for _, key := range []string{"name", "path", "filename"} {
				if s, ok := typed[key].(string); ok {
					collect(s)
					break
				}
			}
		}
	}
	collect(raw)
	return out
}
