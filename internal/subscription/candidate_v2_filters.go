package subscription

import (
	"fmt"
	"strings"

	"github.com/dlclark/regexp2"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

const maxCandidatePatternLength = 512

func compileCandidatePattern(value string) (*regexp2.Regexp, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if len(value) > maxCandidatePatternLength {
		return nil, ErrInvalidCandidateQuery
	}
	pattern, err := regexp2.Compile(value, regexp2.IgnoreCase)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid release pattern", ErrInvalidCandidateQuery)
	}
	return pattern, nil
}

func matchesCandidatePattern(pattern *regexp2.Regexp, value string) bool {
	if pattern == nil {
		return true
	}
	matched, err := pattern.MatchString(value)
	return err == nil && matched
}

func compileCandidatePatterns(values []string) ([]*regexp2.Regexp, error) {
	patterns := make([]*regexp2.Regexp, 0, len(values))
	for _, value := range values {
		pattern, err := compileCandidatePattern(value)
		if err != nil {
			return nil, err
		}
		if pattern != nil {
			patterns = append(patterns, pattern)
		}
	}
	return patterns, nil
}

func matchCandidatePatterns(value string, include, exclude []*regexp2.Regexp) bool {
	for _, pattern := range include {
		matched, err := pattern.MatchString(value)
		if err != nil || !matched {
			return false
		}
	}
	for _, pattern := range exclude {
		matched, err := pattern.MatchString(value)
		if err != nil || matched {
			return false
		}
	}
	return true
}

func candidateFilterText(resource indexer.Resource) string {
	return strings.Join([]string{
		resource.Title,
		resource.Description,
		strings.Join(resource.Labels, " "),
		v2VolumeFactor(resource.UploadVolumeFactor, resource.DownloadVolumeFactor),
	}, " ")
}

func v2VolumeFactor(upload, download *float64) string {
	if upload == nil || download == nil {
		return "未知"
	}
	switch fmt.Sprintf("%.2f %.2f", *upload, *download) {
	case "1.00 1.00":
		return "普通"
	case "1.00 0.00":
		return "免费"
	case "2.00 1.00":
		return "2X"
	case "4.00 1.00":
		return "4X"
	case "2.00 0.00":
		return "2X免费"
	case "4.00 0.00":
		return "4X免费"
	case "1.00 0.50":
		return "50%"
	case "2.00 0.50":
		return "2X 50%"
	case "1.00 0.70":
		return "70%"
	case "1.00 0.30":
		return "30%"
	case "1.00 0.75":
		return "75%"
	case "1.00 0.25":
		return "25%"
	default:
		return "未知"
	}
}

func candidateQualityScore(value string, parsed media.ParseResult, preferences []string) int {
	score := resolutionScore(parsed.Resolution) + resourceTypeScore(parsed.ResourceType)
	if parsed.ResourceEffect != "" {
		score += 20
	}
	if parsed.VideoEncode == "HEVC" || parsed.VideoEncode == "H265" ||
		parsed.VideoEncode == "x265" || parsed.VideoEncode == "AV1" {
		score += 10
	}
	upper := strings.ToUpper(value)
	for index, preference := range preferences {
		preference = strings.ToUpper(strings.TrimSpace(preference))
		if preference != "" && strings.Contains(upper, preference) {
			return 100_000 - index*100 + score
		}
	}
	return score
}

func resolutionScore(resolution string) int {
	switch strings.ToLower(strings.TrimSpace(resolution)) {
	case "2160p":
		return 600
	case "1440p":
		return 500
	case "1080p":
		return 400
	case "720p":
		return 300
	case "576p":
		return 200
	case "480p":
		return 100
	default:
		return 0
	}
}

func resourceTypeScore(resourceType string) int {
	switch strings.ToUpper(strings.TrimSpace(resourceType)) {
	case "BLURAY REMUX":
		return 80
	case "BLURAY":
		return 60
	case "WEB-DL":
		return 40
	case "WEBRIP":
		return 30
	case "HDTV":
		return 20
	default:
		return 0
	}
}
