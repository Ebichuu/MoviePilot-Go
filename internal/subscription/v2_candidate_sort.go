package subscription

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var defaultV2CandidatePriorityRules = []string{"torrent", "upload", "seeder"}

// SortV2Candidates reproduces TorrentHelper.sort_torrents for one already
// identified subscription target. Fully equal candidates keep their input order.
func SortV2Candidates(
	candidates []Candidate,
	priorityRules []string,
	siteUploads map[string]float64,
) []Candidate {
	result := append([]Candidate(nil), candidates...)
	rules := normalizeV2CandidatePriorityRules(priorityRules)
	sort.SliceStable(result, func(i, j int) bool {
		return v2CandidateSortKey(result[i], rules, siteUploads) >
			v2CandidateSortKey(result[j], rules, siteUploads)
	})
	if result == nil {
		return []Candidate{}
	}
	return result
}

func v2CandidateSortKey(
	candidate Candidate,
	rules []string,
	siteUploads map[string]float64,
) string {
	var key strings.Builder
	mediaTitle := candidate.MediaTitle
	if mediaTitle == "" {
		mediaTitle = candidate.Resource.RecognizedMediaTitle
	}
	key.WriteString(v2MediaTitlePrefix(mediaTitle))
	for _, rule := range rules {
		switch rule {
		case "torrent":
			key.WriteString(v2ZeroPad(strconv.Itoa(candidate.Resource.PriOrder), 3))
		case "site":
			key.WriteString(v2ZeroPad(strconv.Itoa(999-candidate.Resource.SiteOrder), 3))
		case "upload":
			upload := siteUploads[candidate.Resource.Site]
			text := "0"
			if upload != 0 {
				text = v2PythonFloatString(upload)
			}
			key.WriteString(v2ZeroPad(text, 30))
		case "seeder":
			key.WriteString(v2ZeroPad(strconv.Itoa(candidate.Resource.Seeders), 10))
		}
	}
	seasons, episodes := v2CandidateSeasonEpisodeRank(candidate)
	key.WriteString(v2ZeroPad(strconv.Itoa(seasons), 3))
	key.WriteString(v2ZeroPad(strconv.Itoa(episodes), 4))
	return key.String()
}

// v2MediaTitlePrefix mirrors Python str.ljust(200, " "). Python pads by
// Unicode code points, so byte length cannot be used for non-ASCII titles.
func v2MediaTitlePrefix(title string) string {
	if width := utf8.RuneCountInString(title); width < 200 {
		return title + strings.Repeat(" ", 200-width)
	}
	return title
}

func v2ZeroPad(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return strings.Repeat("0", width-len(value)) + value
}

func v2PythonFloatString(value float64) string {
	if value == 0 {
		return "0"
	}
	if math.IsNaN(value) {
		return "nan"
	}
	if math.IsInf(value, 1) {
		return "inf"
	}
	if math.IsInf(value, -1) {
		return "-inf"
	}
	format := byte('e')
	magnitude := math.Abs(value)
	if magnitude >= 1e-4 && magnitude < 1e16 {
		format = 'f'
	}
	result := strconv.FormatFloat(value, format, -1, 64)
	if !strings.ContainsAny(result, ".eE") {
		result += ".0"
	}
	return result
}

func normalizeV2CandidatePriorityRules(input []string) []string {
	if len(input) == 0 {
		return append([]string(nil), defaultV2CandidatePriorityRules...)
	}
	rules := make([]string, 0, len(input))
	for _, rule := range input {
		switch rule {
		case "torrent", "site", "upload", "seeder":
			rules = append(rules, rule)
		}
	}
	return rules
}

func v2CandidateSeasonEpisodeRank(candidate Candidate) (int, int) {
	seasons := len(candidate.Parsed.SeasonList())
	episodes, err := candidate.Parsed.EpisodeList()
	if err != nil || len(episodes) == 0 {
		return seasons, 9999
	}
	return seasons, len(episodes)
}
