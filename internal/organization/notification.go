// Package organization defines immutable organization notification facts and
// MoviePilot V2 compatible rendering rules.
package organization

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	// CurrentSuccessVersion is the persistence contract used by new success inputs.
	CurrentSuccessVersion = 1

	maxUsernameBytes     = 255
	maxMediaSourceBytes  = 32
	maxMediaIDBytes      = 128
	maxTitleYearBytes    = 512
	maxMediaTypeBytes    = 32
	maxCategoryBytes     = 128
	maxResourceTermBytes = 1024
	maxURLBytes          = 4096
	maxRoleBytes         = 64
	maxEpisodes          = 10000
	maxFiles             = 10000
	maxSeason            = 9999
	maxEpisode           = 99999
	groupKeyPrefix       = "sha256:"
	groupKeyHexBytes     = sha256.Size * 2
)

var (
	// ErrInvalidMediaContext means media display facts violate the bounded domain contract.
	ErrInvalidMediaContext = errors.New("invalid organization media context")
	// ErrInvalidSuccessInput means a download success input is malformed.
	ErrInvalidSuccessInput = errors.New("invalid organization success input")
	// ErrInvalidFileFact means an organization file fact is malformed.
	ErrInvalidFileFact = errors.New("invalid organization file fact")
	// ErrInvalidSuccessSnapshot means an organization success snapshot is malformed.
	ErrInvalidSuccessSnapshot = errors.New("invalid organization success snapshot")
	// ErrLegacySuccessSnapshot means the snapshot must use the legacy notification renderer.
	ErrLegacySuccessSnapshot = errors.New("legacy organization success snapshot")
)

// MediaContext is the bounded media display data frozen when a download is submitted.
type MediaContext struct {
	Source      string  `json:"source"`
	MediaID     string  `json:"media_id"`
	TitleYear   string  `json:"title_year"`
	MediaType   string  `json:"type"`
	Category    string  `json:"category"`
	VoteAverage float64 `json:"vote_average"`
	PosterURL   string  `json:"poster_url"`
	BackdropURL string  `json:"backdrop_url"`
}

// Clone returns an independent media context. It currently contains value fields only.
func (m MediaContext) Clone() MediaContext { return m }

// Valid reports whether m can be normalized into the bounded media contract.
func (m MediaContext) Valid() bool {
	_, err := NormalizeMediaContext(m)
	return err == nil
}

// Episode identifies one episode in one season.
type Episode struct {
	Season  int `json:"season"`
	Episode int `json:"episode"`
}

// Clone returns an independent episode value.
func (e Episode) Clone() Episode { return e }

// Valid reports whether the season and episode are in the supported V2 range.
func (e Episode) Valid() bool {
	return e.Season >= 0 && e.Season <= maxSeason && e.Episode > 0 && e.Episode <= maxEpisode
}

// SuccessInput is the immutable notification input created with a download intent.
type SuccessInput struct {
	IntentID     uuid.UUID    `json:"intent_id"`
	Version      int          `json:"version"`
	Legacy       bool         `json:"legacy"`
	Username     string       `json:"username"`
	Manual       bool         `json:"manual"`
	Background   bool         `json:"background"`
	Media        MediaContext `json:"media"`
	ResourceTerm string       `json:"resource_term"`
	Episodes     []Episode    `json:"episodes"`
}

// Clone returns an input that does not share mutable slices with the receiver.
func (s SuccessInput) Clone() SuccessInput {
	s.Media = s.Media.Clone()
	s.Episodes = cloneEpisodes(s.Episodes)
	return s
}

// Valid reports whether s can be normalized into the bounded input contract.
func (s SuccessInput) Valid() bool {
	_, err := NormalizeSuccessInput(s)
	return err == nil
}

// FileFact is the path-free immutable notification fact for one organized file.
type FileFact struct {
	Ordinal      int    `json:"ordinal"`
	Role         string `json:"role"`
	SizeBytes    int64  `json:"size_bytes"`
	Season       int    `json:"season"`
	Episodes     []int  `json:"episodes"`
	ResourceTerm string `json:"resource_term"`
}

// Clone returns a file fact that does not share its episode slice.
func (f FileFact) Clone() FileFact {
	f.Episodes = append([]int(nil), f.Episodes...)
	return f
}

// Valid reports whether f can be normalized into the bounded file contract.
func (f FileFact) Valid() bool {
	_, err := NormalizeFileFact(f)
	return err == nil
}

// SuccessSnapshot is the immutable per-media, per-season organization result.
type SuccessSnapshot struct {
	BatchID    uuid.UUID    `json:"batch_id"`
	IntentID   uuid.UUID    `json:"intent_id"`
	GroupKey   string       `json:"group_key"`
	Version    int          `json:"version"`
	Legacy     bool         `json:"legacy"`
	Username   string       `json:"username"`
	NeedNotify bool         `json:"need_notify"`
	Manual     bool         `json:"manual"`
	Background bool         `json:"background"`
	Media      MediaContext `json:"media"`
	Files      []FileFact   `json:"files"`
}

// Clone returns a snapshot that does not share mutable slices with the receiver.
func (s SuccessSnapshot) Clone() SuccessSnapshot {
	s.Media = s.Media.Clone()
	s.Files = cloneFileFacts(s.Files)
	return s
}

// Valid reports whether s can be normalized into the bounded snapshot contract.
func (s SuccessSnapshot) Valid() bool {
	_, err := NormalizeSuccessSnapshot(s)
	return err == nil
}

// Eligible implements the V2 notification eligibility expression.
func (s SuccessSnapshot) Eligible() bool {
	return s.NeedNotify && (s.Background || !s.Manual)
}

// V2OrganizeSuccessContext is the exact bounded default-template context.
type V2OrganizeSuccessContext struct {
	TitleYear     string `json:"title_year"`
	SeasonEpisode string `json:"season_episode"`
	VoteAverage   string `json:"vote_average"`
	MediaType     string `json:"type"`
	Category      string `json:"category"`
	ResourceTerm  string `json:"resource_term"`
	FileCount     int    `json:"file_count"`
	TotalSize     string `json:"total_size"`
	ErrMsg        string `json:"err_msg"`
	Username      string `json:"username"`
}

// Clone returns an independent context. It currently contains value fields only.
func (c V2OrganizeSuccessContext) Clone() V2OrganizeSuccessContext { return c }

// Values returns a fresh map suitable for a bounded template renderer.
func (c V2OrganizeSuccessContext) Values() map[string]any {
	return map[string]any{
		"title_year": c.TitleYear, "season_episode": c.SeasonEpisode,
		"vote_average": c.VoteAverage, "type": c.MediaType,
		"category": c.Category, "resource_term": c.ResourceTerm,
		"file_count": c.FileCount, "total_size": c.TotalSize,
		"err_msg": c.ErrMsg, "username": c.Username,
	}
}

// V2OrganizeSuccess is the default V2 organizeSuccess rendering result.
type V2OrganizeSuccess struct {
	Title    string                   `json:"title"`
	Text     string                   `json:"text"`
	Image    string                   `json:"image"`
	Link     *string                  `json:"link"`
	Username string                   `json:"username"`
	Context  V2OrganizeSuccessContext `json:"context"`
}

// Clone returns a rendering that does not share its optional link pointer.
func (m V2OrganizeSuccess) Clone() V2OrganizeSuccess {
	m.Context = m.Context.Clone()
	if m.Link != nil {
		link := *m.Link
		m.Link = &link
	}
	return m
}

// NormalizeMediaContext trims display fields and enforces storage bounds.
func NormalizeMediaContext(media MediaContext) (MediaContext, error) {
	normalized := media.Clone()
	var err error
	if normalized.Source, err = normalizeText(media.Source, maxMediaSourceBytes); err != nil {
		return MediaContext{}, fmt.Errorf("%w: source", ErrInvalidMediaContext)
	}
	if normalized.MediaID, err = normalizeText(media.MediaID, maxMediaIDBytes); err != nil {
		return MediaContext{}, fmt.Errorf("%w: media id", ErrInvalidMediaContext)
	}
	if (normalized.Source == "") != (normalized.MediaID == "") {
		return MediaContext{}, fmt.Errorf("%w: source and media id must be paired", ErrInvalidMediaContext)
	}
	fields := []struct {
		value *string
		limit int
		name  string
	}{
		{&normalized.TitleYear, maxTitleYearBytes, "title year"},
		{&normalized.MediaType, maxMediaTypeBytes, "media type"},
		{&normalized.Category, maxCategoryBytes, "category"},
	}
	for _, field := range fields {
		*field.value, err = normalizeText(*field.value, field.limit)
		if err != nil {
			return MediaContext{}, fmt.Errorf("%w: %s", ErrInvalidMediaContext, field.name)
		}
	}
	if math.IsNaN(normalized.VoteAverage) || math.IsInf(normalized.VoteAverage, 0) || normalized.VoteAverage < 0 || normalized.VoteAverage > 10 {
		return MediaContext{}, fmt.Errorf("%w: vote average", ErrInvalidMediaContext)
	}
	if normalized.PosterURL, err = normalizeHTTPURL(media.PosterURL); err != nil {
		return MediaContext{}, fmt.Errorf("%w: poster url", ErrInvalidMediaContext)
	}
	if normalized.BackdropURL, err = normalizeHTTPURL(media.BackdropURL); err != nil {
		return MediaContext{}, fmt.Errorf("%w: backdrop url", ErrInvalidMediaContext)
	}
	return normalized, nil
}

// NormalizeSuccessInput returns a bounded, sorted input without mutating the caller.
func NormalizeSuccessInput(input SuccessInput) (SuccessInput, error) {
	if input.IntentID == uuid.Nil || !validVersion(input.Version, input.Legacy) {
		return SuccessInput{}, ErrInvalidSuccessInput
	}
	normalized := input.Clone()
	var err error
	if normalized.Username, err = normalizeText(input.Username, maxUsernameBytes); err != nil {
		return SuccessInput{}, fmt.Errorf("%w: username", ErrInvalidSuccessInput)
	}
	if normalized.Media, err = NormalizeMediaContext(input.Media); err != nil {
		return SuccessInput{}, fmt.Errorf("%w: media", ErrInvalidSuccessInput)
	}
	if normalized.ResourceTerm, err = normalizeText(input.ResourceTerm, maxResourceTermBytes); err != nil {
		return SuccessInput{}, fmt.Errorf("%w: resource term", ErrInvalidSuccessInput)
	}
	if len(input.Episodes) > maxEpisodes {
		return SuccessInput{}, fmt.Errorf("%w: too many episodes", ErrInvalidSuccessInput)
	}
	for _, episode := range input.Episodes {
		if !episode.Valid() {
			return SuccessInput{}, fmt.Errorf("%w: episode", ErrInvalidSuccessInput)
		}
	}
	sort.Slice(normalized.Episodes, func(i, j int) bool {
		if normalized.Episodes[i].Season == normalized.Episodes[j].Season {
			return normalized.Episodes[i].Episode < normalized.Episodes[j].Episode
		}
		return normalized.Episodes[i].Season < normalized.Episodes[j].Season
	})
	normalized.Episodes = compactEpisodes(normalized.Episodes)
	return normalized, nil
}

// NormalizeFileFact returns a bounded file fact without mutating the caller.
func NormalizeFileFact(file FileFact) (FileFact, error) {
	if file.Ordinal < 0 || file.SizeBytes < 0 || file.Season < 0 || file.Season > maxSeason || len(file.Episodes) > maxEpisodes {
		return FileFact{}, ErrInvalidFileFact
	}
	normalized := file.Clone()
	var err error
	if normalized.Role, err = normalizeText(file.Role, maxRoleBytes); err != nil || normalized.Role == "" {
		return FileFact{}, fmt.Errorf("%w: role", ErrInvalidFileFact)
	}
	if normalized.ResourceTerm, err = normalizeText(file.ResourceTerm, maxResourceTermBytes); err != nil {
		return FileFact{}, fmt.Errorf("%w: resource term", ErrInvalidFileFact)
	}
	for _, episode := range normalized.Episodes {
		if episode <= 0 || episode > maxEpisode {
			return FileFact{}, fmt.Errorf("%w: episode", ErrInvalidFileFact)
		}
	}
	sort.Ints(normalized.Episodes)
	normalized.Episodes = compactInts(normalized.Episodes)
	return normalized, nil
}

// NormalizeSuccessSnapshot returns a bounded snapshot without mutating the caller.
func NormalizeSuccessSnapshot(snapshot SuccessSnapshot) (SuccessSnapshot, error) {
	if snapshot.BatchID == uuid.Nil || snapshot.IntentID == uuid.Nil || !validVersion(snapshot.Version, snapshot.Legacy) || !validGroupKey(snapshot.GroupKey) {
		return SuccessSnapshot{}, ErrInvalidSuccessSnapshot
	}
	if len(snapshot.Files) > maxFiles || (!snapshot.Legacy && len(snapshot.Files) == 0) {
		return SuccessSnapshot{}, fmt.Errorf("%w: file count", ErrInvalidSuccessSnapshot)
	}
	normalized := snapshot.Clone()
	var err error
	if normalized.Username, err = normalizeText(snapshot.Username, maxUsernameBytes); err != nil {
		return SuccessSnapshot{}, fmt.Errorf("%w: username", ErrInvalidSuccessSnapshot)
	}
	if normalized.Media, err = NormalizeMediaContext(snapshot.Media); err != nil {
		return SuccessSnapshot{}, fmt.Errorf("%w: media", ErrInvalidSuccessSnapshot)
	}
	seenOrdinals := make(map[int]struct{}, len(normalized.Files))
	season := -1
	for index := range normalized.Files {
		normalized.Files[index], err = NormalizeFileFact(normalized.Files[index])
		if err != nil {
			return SuccessSnapshot{}, fmt.Errorf("%w: file fact", ErrInvalidSuccessSnapshot)
		}
		file := normalized.Files[index]
		if _, exists := seenOrdinals[file.Ordinal]; exists {
			return SuccessSnapshot{}, fmt.Errorf("%w: duplicate ordinal", ErrInvalidSuccessSnapshot)
		}
		seenOrdinals[file.Ordinal] = struct{}{}
		if season == -1 {
			season = file.Season
		} else if file.Season != season {
			return SuccessSnapshot{}, fmt.Errorf("%w: mixed seasons", ErrInvalidSuccessSnapshot)
		}
	}
	sort.Slice(normalized.Files, func(i, j int) bool { return normalized.Files[i].Ordinal < normalized.Files[j].Ordinal })
	if len(normalized.Files) > 0 {
		wantGroupKey, buildErr := BuildGroupKey(normalized.Media, normalized.Files[0].Season)
		if buildErr != nil || normalized.GroupKey != wantGroupKey {
			return SuccessSnapshot{}, fmt.Errorf("%w: group key mismatch", ErrInvalidSuccessSnapshot)
		}
	}
	if _, err = totalFileSize(normalized.Files); err != nil {
		return SuccessSnapshot{}, fmt.Errorf("%w: total size", ErrInvalidSuccessSnapshot)
	}
	return normalized, nil
}

func normalizeText(value string, limit int) (string, error) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", errors.New("invalid text encoding")
	}
	normalized := strings.TrimSpace(value)
	if len(normalized) > limit {
		return "", errors.New("text exceeds limit")
	}
	return normalized, nil
}

func normalizeHTTPURL(value string) (string, error) {
	normalized, err := normalizeText(value, maxURLBytes)
	if err != nil || normalized == "" {
		return normalized, err
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("invalid http url")
	}
	return parsed.String(), nil
}

func validVersion(version int, legacy bool) bool {
	return (legacy && version == 0) || (!legacy && version == CurrentSuccessVersion)
}

func validGroupKey(value string) bool {
	if len(value) != len(groupKeyPrefix)+groupKeyHexBytes || !strings.HasPrefix(value, groupKeyPrefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, groupKeyPrefix))
	return err == nil
}

func cloneEpisodes(episodes []Episode) []Episode {
	if episodes == nil {
		return nil
	}
	cloned := make([]Episode, len(episodes))
	copy(cloned, episodes)
	return cloned
}

func cloneFileFacts(files []FileFact) []FileFact {
	if files == nil {
		return nil
	}
	cloned := make([]FileFact, len(files))
	for index := range files {
		cloned[index] = files[index].Clone()
	}
	return cloned
}

func compactEpisodes(episodes []Episode) []Episode {
	if len(episodes) < 2 {
		return episodes
	}
	write := 1
	for _, episode := range episodes[1:] {
		if episode == episodes[write-1] {
			continue
		}
		episodes[write] = episode
		write++
	}
	return episodes[:write]
}

func compactInts(values []int) []int {
	if len(values) < 2 {
		return values
	}
	write := 1
	for _, value := range values[1:] {
		if value == values[write-1] {
			continue
		}
		values[write] = value
		write++
	}
	return values[:write]
}

func totalFileSize(files []FileFact) (int64, error) {
	var total int64
	for _, file := range files {
		if file.SizeBytes < 0 || total > math.MaxInt64-file.SizeBytes {
			return 0, ErrInvalidSuccessSnapshot
		}
		total += file.SizeBytes
	}
	return total, nil
}

func snapshotResourceTerm(files []FileFact) string {
	for _, file := range files {
		if file.ResourceTerm != "" {
			return file.ResourceTerm
		}
	}
	return ""
}

func pythonFloat(value float64) string {
	formatted := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(formatted, ".") {
		formatted += ".0"
	}
	return formatted
}

func formatV2Vote(value float64) string {
	if value == 0 {
		return ""
	}
	if value > 0 && value < 1e-4 {
		return strconv.FormatFloat(value, 'e', -1, 64)
	}
	return pythonFloat(value)
}
