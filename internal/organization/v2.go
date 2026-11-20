package organization

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// BuildGroupKey returns a path-free deterministic media identity and season digest.
func BuildGroupKey(media MediaContext, season int) (string, error) {
	if season < 0 || season > maxSeason {
		return "", ErrInvalidSuccessSnapshot
	}
	normalized, err := NormalizeMediaContext(media)
	if err != nil {
		return "", fmt.Errorf("%w: media", ErrInvalidSuccessSnapshot)
	}
	identityKind := "title"
	identityA := normalized.TitleYear
	identityB := ""
	if normalized.Source != "" && normalized.MediaID != "" {
		identityKind = "id"
		identityA = strings.ToLower(normalized.Source)
		identityB = normalized.MediaID
	}
	payload := strings.Join([]string{
		"moviepilot:organization-success-group:v1", identityKind, identityA, identityB, strconv.Itoa(season),
	}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return groupKeyPrefix + hex.EncodeToString(digest[:]), nil
}

// FormatV2FileSize reproduces V2 StringUtils.str_filesize for non-negative byte counts.
func FormatV2FileSize(size int64) string {
	if size < 0 {
		return ""
	}
	thresholds := [...]int64{1024 - 1, 1<<20 - 1, 1<<30 - 1, 1<<40 - 1}
	units := [...]string{"K", "M", "G", "T"}
	index := sort.Search(len(thresholds), func(i int) bool { return thresholds[i] >= size }) - 1
	if index < 0 {
		return pythonFloat(float64(size)) + "B"
	}
	value := math.RoundToEven(float64(size)/float64(thresholds[index]+1)*100) / 100
	return pythonFloat(value) + units[index]
}

// FormatV2Episodes reproduces V2 StringUtils.format_ep without mutating nums.
func FormatV2Episodes(nums []int) string {
	if len(nums) == 0 {
		return ""
	}
	episodes := append([]int(nil), nums...)
	sort.Ints(episodes)
	parts := make([]string, 0, len(episodes))
	start, end := episodes[0], episodes[0]
	appendRange := func() {
		if start == end {
			parts = append(parts, fmt.Sprintf("E%02d", start))
			return
		}
		parts = append(parts, fmt.Sprintf("E%02d-E%02d", start, end))
	}
	for _, episode := range episodes[1:] {
		if episode == end+1 {
			end = episode
			continue
		}
		appendRange()
		start, end = episode, episode
	}
	appendRange()
	return strings.Join(parts, "、")
}

// FormatV2SeasonEpisodes formats Sxx followed by the optional V2 episode ranges.
func FormatV2SeasonEpisodes(season int, episodes []int) string {
	formattedSeason := fmt.Sprintf("S%02d", season)
	formattedEpisodes := FormatV2Episodes(episodes)
	if formattedEpisodes == "" {
		return formattedSeason
	}
	return formattedSeason + " " + formattedEpisodes
}

// V2MessageImage selects backdrop before poster and replaces every original token.
func V2MessageImage(media MediaContext) string {
	image := media.BackdropURL
	if image == "" {
		image = media.PosterURL
	}
	return strings.ReplaceAll(image, "original", "w500")
}

// V2HistoryLink reproduces standardize_base_url plus Python urljoin for #/history.
func V2HistoryLink(appDomain string) *string {
	if appDomain == "" || len(appDomain) > maxURLBytes || strings.ContainsRune(appDomain, '\x00') || !utf8.ValidString(appDomain) {
		return nil
	}
	host := appDomain
	if !strings.HasSuffix(host, "/") {
		host += "/"
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}
	base, err := url.Parse(host)
	if err != nil {
		return nil
	}
	reference := &url.URL{Fragment: "/history"}
	link := base.ResolveReference(reference).String()
	if link == "" || len(link) > maxURLBytes {
		return nil
	}
	return &link
}

// RenderV2OrganizeSuccess renders the V2 default organizeSuccess template.
func RenderV2OrganizeSuccess(snapshot SuccessSnapshot, appDomain string) (V2OrganizeSuccess, error) {
	normalized, err := NormalizeSuccessSnapshot(snapshot)
	if err != nil {
		return V2OrganizeSuccess{}, err
	}
	if normalized.Legacy {
		return V2OrganizeSuccess{}, ErrLegacySuccessSnapshot
	}
	seasonEpisode := ""
	if normalized.Media.MediaType == "电视剧" {
		episodes := make([]int, 0)
		for _, file := range normalized.Files {
			episodes = append(episodes, file.Episodes...)
		}
		sort.Ints(episodes)
		episodes = compactInts(episodes)
		seasonEpisode = FormatV2SeasonEpisodes(normalized.Files[0].Season, episodes)
	}
	totalSize, err := totalFileSize(normalized.Files)
	if err != nil {
		return V2OrganizeSuccess{}, fmt.Errorf("%w: total size", ErrInvalidSuccessSnapshot)
	}
	context := V2OrganizeSuccessContext{
		TitleYear: normalized.Media.TitleYear, SeasonEpisode: seasonEpisode,
		VoteAverage: formatV2Vote(normalized.Media.VoteAverage), MediaType: normalized.Media.MediaType,
		Category: normalized.Media.Category, ResourceTerm: snapshotResourceTerm(normalized.Files),
		FileCount: len(normalized.Files), TotalSize: FormatV2FileSize(totalSize),
		ErrMsg: "", Username: normalized.Username,
	}
	title := context.TitleYear
	if context.SeasonEpisode != "" {
		title += " " + context.SeasonEpisode
	}
	title += " 已入库"
	var text strings.Builder
	if context.VoteAverage != "" {
		text.WriteString("评分：")
		text.WriteString(context.VoteAverage)
		text.WriteString("，")
	}
	text.WriteString("类型：")
	text.WriteString(context.MediaType)
	if context.Category != "" {
		text.WriteString("，类别：")
		text.WriteString(context.Category)
	}
	if context.ResourceTerm != "" {
		text.WriteString("，质量：")
		text.WriteString(context.ResourceTerm)
	}
	fmt.Fprintf(&text, "，共%d个文件，大小：%s", context.FileCount, context.TotalSize)
	return V2OrganizeSuccess{
		Title: title, Text: text.String(), Image: V2MessageImage(normalized.Media),
		Link: V2HistoryLink(appDomain), Username: normalized.Username, Context: context,
	}, nil
}
