package subscription

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
)

const (
	V2DownloadFailureResourceTTL  = 24 * time.Hour
	V2DownloadFailureTransientTTL = time.Hour
)

var v2DownloadFailureResourceErrorKeywords = [...]string{
	"无法读取种子文件",
	"下载种子内容为空",
	"无法获取下载地址",
	"种子下载失败",
	"torrent not found",
	"not found",
	"404",
	"deleted",
	"invalid torrent",
}

// V2DownloadFailureTarget contains the media identity used by V2's resource
// failure fingerprint. It deliberately contains no persistence types.
type V2DownloadFailureTarget struct {
	MediaType   string
	Title       string
	Year        string
	MediaSource string
	MediaID     string
	TMDBID      string
	DoubanID    string
	BangumiID   string
	AniListID   string
	IMDBID      string
	TVDBID      string
	Season      *int
}

// V2DownloadFailure is the bounded, non-secret fact persisted for one failed
// subscription candidate.
type V2DownloadFailure struct {
	Fingerprint  string
	MediaType    string
	Title        string
	Year         string
	MediaSource  string
	MediaID      string
	Season       string
	Episodes     string
	SiteID       int
	SiteName     string
	ResourceKey  string
	TorrentName  string
	TorrentSize  int64
	Downloader   string
	Source       string
	ErrorMessage string
}

// NewV2DownloadFailure reproduces V2's subscription resource fingerprint.
// The original locator is only used in memory and is never returned.
func NewV2DownloadFailure(
	target V2DownloadFailureTarget,
	candidate Candidate,
	selectedEpisodes []Episode,
	downloader string,
	source string,
	errorMessage string,
) (V2DownloadFailure, bool) {
	mediaType := strings.TrimSpace(target.MediaType)
	mediaKey := v2DownloadFailureMediaKey(target)
	resourceKey := v2DownloadFailureResourceKey(candidate.Resource)
	if mediaType == "" || mediaKey == "" || resourceKey == "" {
		return V2DownloadFailure{}, false
	}

	season := v2DownloadFailureSeason(target, candidate)
	fingerprintEpisodes := candidate.Parsed.Episode()
	storedEpisodes := fingerprintEpisodes
	if len(selectedEpisodes) > 0 {
		storedEpisodes = v2DownloadFailureSelectedEpisodes(selectedEpisodes)
	}
	site := strings.TrimSpace(candidate.Resource.Site)
	siteKey := site
	if candidate.Resource.SiteID > 0 {
		siteKey = strconv.Itoa(candidate.Resource.SiteID)
	}
	payload := map[string]string{
		"media_type": mediaType,
		"media_key":  mediaKey,
		"season":     season,
		"episodes":   fingerprintEpisodes,
		"site":       siteKey,
		"resource":   resourceKey,
	}
	raw := marshalV2DownloadFailurePayload(payload)
	digest := sha256.Sum256(raw)
	return V2DownloadFailure{
		Fingerprint: hex.EncodeToString(digest[:]), MediaType: mediaType,
		Title: strings.TrimSpace(target.Title), Year: strings.TrimSpace(target.Year),
		MediaSource: strings.TrimSpace(target.MediaSource), MediaID: strings.TrimSpace(target.MediaID),
		Season: season, Episodes: storedEpisodes, SiteID: candidate.Resource.SiteID,
		SiteName: site, ResourceKey: resourceKey, TorrentName: strings.TrimSpace(candidate.Resource.Title),
		TorrentSize: candidate.Resource.SizeBytes, Downloader: strings.TrimSpace(downloader),
		Source: v2DownloadFailureTruncate(source), ErrorMessage: v2DownloadFailureTruncate(errorMessage),
	}, true
}

// V2DownloadFailureTTL applies V2's resource and transient failure windows.
func V2DownloadFailureTTL(errorMessage string) time.Duration {
	text := strings.ToLower(errorMessage)
	for _, keyword := range v2DownloadFailureResourceErrorKeywords {
		if strings.Contains(text, keyword) {
			return V2DownloadFailureResourceTTL
		}
	}
	return V2DownloadFailureTransientTTL
}

func v2DownloadFailureMediaKey(target V2DownloadFailureTarget) string {
	source := strings.ToLower(strings.TrimSpace(target.MediaSource))
	if source == "tmdb" {
		source = "themoviedb"
	}
	id := strings.TrimSpace(target.MediaID)
	legacyIDs := map[string]string{
		"themoviedb": strings.TrimSpace(target.TMDBID),
		"douban":     strings.TrimSpace(target.DoubanID),
		"bangumi":    strings.TrimSpace(target.BangumiID),
		"anilist":    strings.TrimSpace(target.AniListID),
	}
	if id == "" && source != "" {
		id = legacyIDs[source]
	}
	if source != "" && id != "" {
		return source + ":" + id
	}
	if source == "" {
		for _, candidateSource := range []string{"themoviedb", "douban", "bangumi", "anilist"} {
			if legacyID := legacyIDs[candidateSource]; legacyID != "" {
				return candidateSource + ":" + legacyID
			}
		}
	}
	if imdbID := strings.TrimSpace(target.IMDBID); imdbID != "" {
		return imdbID
	}
	if tvdbID := strings.TrimSpace(target.TVDBID); tvdbID != "" {
		return tvdbID
	}
	return strings.TrimSpace(target.Title) + ":" + strings.TrimSpace(target.Year)
}

func v2DownloadFailureSeason(target V2DownloadFailureTarget, candidate Candidate) string {
	if explicitSeasonZero(candidate.Resource.Title) {
		return "S00"
	}
	if season := candidate.Parsed.Season(); season != "" {
		return season
	}
	if target.Season != nil {
		return fmt.Sprintf("S%02d", *target.Season)
	}
	return ""
}

func v2DownloadFailureSelectedEpisodes(selected []Episode) string {
	numbers := make([]int, 0, len(selected))
	for _, episode := range selected {
		numbers = append(numbers, episode.Episode)
	}
	return formatV2DownloadFailureEpisodes(numbers)
}

func formatV2DownloadFailureEpisodes(input []int) string {
	if len(input) == 0 {
		return ""
	}
	sort.Ints(input)
	unique := input[:0]
	for _, episode := range input {
		if len(unique) == 0 || unique[len(unique)-1] != episode {
			unique = append(unique, episode)
		}
	}
	parts := make([]string, 0, len(unique))
	for start := 0; start < len(unique); {
		end := start
		for end+1 < len(unique) && unique[end+1] == unique[end]+1 {
			end++
		}
		if end == start {
			parts = append(parts, fmt.Sprintf("E%02d", unique[start]))
		} else {
			parts = append(parts, fmt.Sprintf("E%02d-E%02d", unique[start], unique[end]))
		}
		start = end + 1
	}
	return strings.Join(parts, "、")
}

func v2DownloadFailureResourceKey(resource indexer.Resource) string {
	if resourceID := strings.TrimSpace(resource.TorrentID); resourceID != "" {
		return resourceID
	}
	for _, rawURL := range []string{resource.DetailURL, resource.DownloadURL, resource.DownloadID} {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		if start := strings.Index(rawURL, "["); start >= 0 {
			if end := strings.Index(rawURL[start:], "]"); end >= 0 {
				rawURL = rawURL[start+end+1:]
			}
		}
		parsed, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		query := parsed.Query()
		for _, name := range []string{"id", "torrentid", "torrent_id", "tid", "hash"} {
			for _, value := range query[name] {
				if value != "" {
					return parsed.Host + ":" + name + "=" + value
				}
			}
		}
		if parsed.Host != "" && parsed.Path != "" {
			return parsed.Host + parsed.Path
		}
	}
	return fmt.Sprintf("title=%s|size=%d", strings.TrimSpace(resource.Title), resource.SizeBytes)
}

func marshalV2DownloadFailurePayload(payload map[string]string) []byte {
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, marshalV2JSONString(key)+": "+marshalV2JSONString(payload[key]))
	}
	return []byte("{" + strings.Join(parts, ", ") + "}")
}

func marshalV2JSONString(value string) string {
	var result strings.Builder
	result.Grow(len(value) + 2)
	result.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			result.WriteByte('\\')
			result.WriteRune(character)
		case '\b':
			result.WriteString(`\b`)
		case '\f':
			result.WriteString(`\f`)
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		case '\t':
			result.WriteString(`\t`)
		default:
			if character < 0x20 {
				_, _ = fmt.Fprintf(&result, `\u%04x`, character)
				continue
			}
			result.WriteRune(character)
		}
	}
	result.WriteByte('"')
	return result.String()
}

func v2DownloadFailureTruncate(value string) string {
	runes := []rune(value)
	if len(runes) > 1_000 {
		runes = runes[:1_000]
	}
	return string(runes)
}
