package subscription

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/dlclark/regexp2"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

const (
	// MaxCandidateLimit is the largest result set returned for one subscription search.
	MaxCandidateLimit      = 200
	defaultCandidateLimit  = 100
	maxCandidateInputs     = 10_000
	maxCandidateTerms      = 256
	maxCandidateTermLength = 512
	maxCustomWordLength    = 4096
	maxResourceTitleLength = 4096
)

var (
	// ErrInvalidCandidateQuery indicates an invalid or unbounded subscription query.
	ErrInvalidCandidateQuery = errors.New("invalid subscription candidate query")
	// ErrTooManyCandidateInputs prevents an unexpectedly large search response from
	// causing an unbounded sort or allocation.
	ErrTooManyCandidateInputs = errors.New("too many subscription candidate inputs")
)

// Query describes a normalized subscription target and its resource policy.
// SiteIDs and QualityOrder are preference ordered; SiteIDs also acts as a whitelist.
type Query struct {
	Title           string
	Aliases         []string
	Year            string
	MediaType       media.MediaType
	IMDBID          string
	Season          *int
	MissingEpisodes []Episode
	// FullSeasonMissing preserves V2's empty episode-list sentinel separately
	// from the explicit coordinates used for matching. An explicit full range
	// does not imply this sentinel; absent entries stay explicit.
	FullSeasonMissing map[int]bool
	// TVMissingContainerEmpty carries V2 False + {} through matching to the
	// outer finish step. It never means a full-season download request.
	TVMissingContainerEmpty bool
	// TVRecoveredTarget keeps a local positive missing total separate from a
	// still-zero subscription config. It is internal execution input only.
	TVRecoveredTarget *TVRecoveredTarget
	// TVMediaTitle is the first recognition's title for RSS fallback ordering.
	TVMediaTitle      string
	TVMediaRecognized bool
	SiteIDs           []int
	Include           []string
	Exclude           []string
	Filter            string
	// Quality, Resolution and Effect are V2-compatible regular-expression
	// filters. Each non-empty pattern must match the release text.
	Quality      string
	Resolution   string
	Effect       string
	CustomWords  []string
	QualityOrder []string
	Limit        int
}

// Candidate is a filtered download candidate. Key is deterministic and never
// contains the original resource locator.
type Candidate struct {
	Key      string
	Resource indexer.Resource
	// MediaTitle is the V2 context media_info.title used by RSS ordering. It
	// stays separate from the site's release title and is never an API field.
	MediaTitle      string
	Parsed          media.ParseResult
	CoveredEpisodes []Episode
	MatchScore      int
	QualityScore    int

	siteRank int
	tieKey   string
}

// SelectCandidates filters, deduplicates and deterministically orders search results.
func SelectCandidates(query Query, resources []indexer.Resource) ([]Candidate, error) {
	prepared, err := prepareCandidateQuery(query)
	if err != nil {
		return nil, err
	}
	if len(resources) > maxCandidateInputs {
		return nil, ErrTooManyCandidateInputs
	}

	unique := make(map[string]Candidate, min(len(resources), prepared.limit))
	for _, resource := range resources {
		candidate, ok := prepared.match(resource)
		if !ok {
			continue
		}
		current, exists := unique[candidate.Key]
		if !exists {
			unique[candidate.Key] = candidate
			continue
		}
		candidate.tieKey = candidateTieKey(candidate.Resource)
		current.tieKey = candidateTieKey(current.Resource)
		if candidateLess(candidate, current) {
			unique[candidate.Key] = candidate
		} else {
			unique[candidate.Key] = current
		}
	}

	result := make([]Candidate, 0, len(unique))
	for _, candidate := range unique {
		candidate.tieKey = ""
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool { return candidateLess(result[i], result[j]) })
	if len(result) > prepared.limit {
		result = result[:prepared.limit]
	}
	if result == nil {
		return []Candidate{}, nil
	}
	return result, nil
}

// MatchV2DownloadCandidates filters resources while preserving the upstream
// order consumed by V2's stable torrent sort. It deliberately does not apply
// the generic selector's pre-sort result limit.
func MatchV2DownloadCandidates(query Query, resources []indexer.Resource) ([]Candidate, error) {
	prepared, err := prepareCandidateQuery(query)
	if err != nil {
		return nil, err
	}
	if len(resources) > maxCandidateInputs {
		return nil, ErrTooManyCandidateInputs
	}

	seen := make(map[string]struct{}, len(resources))
	result := make([]Candidate, 0, len(resources))
	for _, resource := range resources {
		candidate, ok := prepared.match(resource)
		if !ok {
			continue
		}
		if _, exists := seen[candidate.Key]; exists {
			continue
		}
		seen[candidate.Key] = struct{}{}
		result = append(result, candidate)
	}
	if result == nil {
		return []Candidate{}, nil
	}
	return result, nil
}

type candidateQuery struct {
	query          Query
	targetNames    []string
	sites          map[int]int
	missing        map[Episode]struct{}
	filter         media.FilterRule
	hasFilter      bool
	include        []*regexp2.Regexp
	exclude        []*regexp2.Regexp
	quality        *regexp2.Regexp
	resolution     *regexp2.Regexp
	effect         *regexp2.Regexp
	normalizedYear string
	normalizedIMDB string
	limit          int
}

func prepareCandidateQuery(query Query) (candidateQuery, error) {
	query.Title = strings.TrimSpace(query.Title)
	if query.Title == "" || len(query.Title) > maxCandidateTermLength {
		return candidateQuery{}, ErrInvalidCandidateQuery
	}
	if len(query.Year) > 16 || len(query.IMDBID) > 64 {
		return candidateQuery{}, ErrInvalidCandidateQuery
	}
	if query.MediaType != media.TypeMovie && query.MediaType != media.TypeTV {
		return candidateQuery{}, ErrInvalidCandidateQuery
	}
	if query.Season != nil && (*query.Season < 0 || *query.Season > 999) {
		return candidateQuery{}, ErrInvalidCandidateQuery
	}
	if query.Limit < 0 || query.Limit > MaxCandidateLimit {
		return candidateQuery{}, ErrInvalidCandidateQuery
	}
	limit := query.Limit
	if limit == 0 {
		limit = defaultCandidateLimit
	}
	if err := validateCandidateTerms(query.Aliases, query.Include, query.Exclude, query.QualityOrder); err != nil {
		return candidateQuery{}, err
	}
	if len(query.SiteIDs) > 1024 || len(query.MissingEpisodes) > 10_000 || len(query.CustomWords) > maxCandidateTerms {
		return candidateQuery{}, ErrInvalidCandidateQuery
	}
	for _, word := range query.CustomWords {
		if len(word) > maxCustomWordLength {
			return candidateQuery{}, ErrInvalidCandidateQuery
		}
	}

	prepared := candidateQuery{
		query:          query,
		targetNames:    normalizedCandidateNames(append([]string{query.Title}, query.Aliases...)),
		sites:          make(map[int]int, len(query.SiteIDs)),
		missing:        make(map[Episode]struct{}, len(query.MissingEpisodes)),
		normalizedYear: strings.TrimSpace(query.Year),
		normalizedIMDB: strings.ToLower(strings.TrimSpace(query.IMDBID)),
		limit:          limit,
	}
	if len(prepared.targetNames) == 0 {
		return candidateQuery{}, ErrInvalidCandidateQuery
	}
	var patternErr error
	prepared.include, patternErr = compileCandidatePatterns(query.Include)
	if patternErr != nil {
		return candidateQuery{}, patternErr
	}
	prepared.exclude, patternErr = compileCandidatePatterns(query.Exclude)
	if patternErr != nil {
		return candidateQuery{}, patternErr
	}
	for index, siteID := range query.SiteIDs {
		if siteID <= 0 {
			return candidateQuery{}, ErrInvalidCandidateQuery
		}
		if _, exists := prepared.sites[siteID]; !exists {
			prepared.sites[siteID] = index
		}
	}
	for _, episode := range query.MissingEpisodes {
		if episode.Season < 0 || episode.Season > 999 || episode.Episode <= 0 || episode.Episode > 5000 {
			return candidateQuery{}, ErrInvalidCandidateQuery
		}
		if query.Season != nil && episode.Season != *query.Season {
			return candidateQuery{}, ErrInvalidCandidateQuery
		}
		prepared.missing[episode] = struct{}{}
	}
	if strings.TrimSpace(query.Filter) != "" {
		filter, filterErr := media.ParseFilterRule(query.Filter)
		if filterErr != nil {
			return candidateQuery{}, fmt.Errorf("%w: %v", ErrInvalidCandidateQuery, filterErr)
		}
		prepared.filter = filter
		prepared.hasFilter = true
	}
	prepared.quality, patternErr = compileCandidatePattern(query.Quality)
	if patternErr != nil {
		return candidateQuery{}, patternErr
	}
	prepared.resolution, patternErr = compileCandidatePattern(query.Resolution)
	if patternErr != nil {
		return candidateQuery{}, patternErr
	}
	prepared.effect, patternErr = compileCandidatePattern(query.Effect)
	if patternErr != nil {
		return candidateQuery{}, patternErr
	}
	return prepared, nil
}

func validateCandidateTerms(groups ...[]string) error {
	for _, group := range groups {
		if len(group) > maxCandidateTerms {
			return ErrInvalidCandidateQuery
		}
		for _, term := range group {
			if len(term) > maxCandidateTermLength {
				return ErrInvalidCandidateQuery
			}
		}
	}
	return nil
}

func (q candidateQuery) match(resource indexer.Resource) (Candidate, bool) {
	resource.Title = strings.TrimSpace(resource.Title)
	if resource.Title == "" || len(resource.Title) > maxResourceTitleLength ||
		(strings.TrimSpace(resource.DownloadID) == "" && strings.TrimSpace(resource.DownloadURL) == "") {
		return Candidate{}, false
	}
	siteRank := 0
	if len(q.sites) > 0 {
		var allowed bool
		siteRank, allowed = q.sites[resource.SiteID]
		if !allowed {
			return Candidate{}, false
		}
	}
	filterText := candidateFilterText(resource)
	if !matchCandidatePatterns(filterText, q.include, q.exclude) || (q.hasFilter && !q.filter.Match(filterText)) {
		return Candidate{}, false
	}
	if !matchesCandidatePattern(q.quality, resource.Title) ||
		!matchesCandidatePattern(q.resolution, resource.Title) ||
		!matchesCandidatePattern(q.effect, resource.Title) {
		return Candidate{}, false
	}

	parsed := media.Parse(resource.Title, media.Options{CustomWords: q.query.CustomWords})
	if q.query.MediaType == media.TypeTV && parsed.Type != media.TypeTV {
		return Candidate{}, false
	}
	if q.query.MediaType == media.TypeMovie && parsed.Type == media.TypeTV {
		return Candidate{}, false
	}
	matchScore := candidateNameScore(q.targetNames, parsed)
	resourceIMDB := strings.ToLower(strings.TrimSpace(resource.IMDBID))
	imdbMatch := q.normalizedIMDB != "" && resourceIMDB != "" && q.normalizedIMDB == resourceIMDB
	if matchScore == 0 && !imdbMatch {
		return Candidate{}, false
	}
	if q.normalizedYear != "" && parsed.Year != "" {
		if !strings.EqualFold(q.normalizedYear, strings.TrimSpace(parsed.Year)) {
			return Candidate{}, false
		}
		matchScore += 80
	}
	if q.normalizedIMDB != "" && resourceIMDB != "" {
		if !imdbMatch {
			return Candidate{}, false
		}
		matchScore = max(matchScore, 1160)
	}

	covered, ok := q.coveredEpisodes(resource.Title, parsed)
	if !ok {
		return Candidate{}, false
	}
	matchScore += min(len(covered), 100)
	key, ok := candidateKey(resource)
	if !ok {
		return Candidate{}, false
	}
	resource.Labels = append([]string(nil), resource.Labels...)
	return Candidate{
		Key: key, Resource: resource, MediaTitle: resource.RecognizedMediaTitle,
		Parsed: parsed, CoveredEpisodes: covered,
		MatchScore: matchScore, QualityScore: candidateQualityScore(filterText, parsed, q.query.QualityOrder),
		siteRank: siteRank,
	}, true
}

func (q candidateQuery) coveredEpisodes(title string, parsed media.ParseResult) ([]Episode, bool) {
	if q.query.MediaType != media.TypeTV {
		return nil, true
	}
	seasons := parsed.SeasonList()
	if parsed.BeginSeason == 0 && explicitSeasonZero(title) {
		seasons = []int{0}
	}
	if q.query.Season != nil && !containsInt(seasons, *q.query.Season) {
		return nil, false
	}
	if len(q.missing) == 0 {
		return nil, true
	}

	episodes, err := parsed.EpisodeList()
	if err != nil {
		return nil, false
	}
	covered := make([]Episode, 0, min(len(q.missing), max(len(episodes), 1)))
	for missing := range q.missing {
		if !containsInt(seasons, missing.Season) {
			continue
		}
		if len(episodes) == 0 || containsInt(episodes, missing.Episode) {
			covered = append(covered, missing)
		}
	}
	if len(covered) == 0 {
		return nil, false
	}
	sort.Slice(covered, func(i, j int) bool {
		if covered[i].Season != covered[j].Season {
			return covered[i].Season < covered[j].Season
		}
		return covered[i].Episode < covered[j].Episode
	})
	return covered, true
}

func candidateLess(left, right Candidate) bool {
	if left.MatchScore != right.MatchScore {
		return left.MatchScore > right.MatchScore
	}
	if left.siteRank != right.siteRank {
		return left.siteRank < right.siteRank
	}
	if left.QualityScore != right.QualityScore {
		return left.QualityScore > right.QualityScore
	}
	if left.Resource.Seeders != right.Resource.Seeders {
		return left.Resource.Seeders > right.Resource.Seeders
	}
	if !left.Resource.PublishedAt.Equal(right.Resource.PublishedAt) {
		return left.Resource.PublishedAt.After(right.Resource.PublishedAt)
	}
	if left.Key != right.Key {
		return left.Key < right.Key
	}
	return left.tieKey < right.tieKey
}

func candidateNameScore(targetNames []string, parsed media.ParseResult) int {
	candidateNames := normalizedCandidateNames([]string{parsed.ChineseName, parsed.EnglishName, parsed.Name()})
	best := 0
	for _, target := range targetNames {
		for _, candidate := range candidateNames {
			score := 0
			switch {
			case target == candidate:
				score = 1000
			case containsCandidatePhrase(candidate, target):
				score = 750
			case containsCandidatePhrase(target, candidate):
				score = 650
			}
			best = max(best, score)
		}
	}
	return best
}

func normalizedCandidateNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		normalized := normalizeCandidateText(name)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

func normalizeCandidateText(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := true
	for _, current := range value {
		if unicode.IsLetter(current) || unicode.IsDigit(current) {
			builder.WriteRune(unicode.ToLower(current))
			space = false
			continue
		}
		if !space && builder.Len() > 0 {
			builder.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func containsCandidatePhrase(value, phrase string) bool {
	if value == phrase {
		return true
	}
	if strings.Contains(" "+value+" ", " "+phrase+" ") {
		return true
	}
	compactValue := strings.ReplaceAll(value, " ", "")
	compactPhrase := strings.ReplaceAll(phrase, " ", "")
	return len([]rune(compactPhrase)) >= 4 && strings.Contains(compactValue, compactPhrase)
}

func candidateKey(resource indexer.Resource) (string, bool) {
	site := strings.ToLower(strings.TrimSpace(resource.Site))
	if resource.SiteID > 0 {
		site = fmt.Sprintf("id:%d", resource.SiteID)
	} else if site != "" {
		site = "name:" + site
	} else {
		return "", false
	}
	locator := strings.TrimSpace(resource.DownloadID)
	if locator != "" {
		locator = "resource:" + locator
	} else if locator = strings.TrimSpace(resource.DetailURL); locator != "" {
		locator = "detail:" + locator
	} else if locator = strings.TrimSpace(resource.DownloadURL); locator != "" {
		locator = "download:" + locator
	} else {
		return "", false
	}
	digest := sha256.Sum256([]byte("moviepilot:subscription-candidate:v1\x00" + site + "\x00" + locator))
	return "sha256:" + hex.EncodeToString(digest[:]), true
}

func candidateTieKey(resource indexer.Resource) string {
	payload, err := json.Marshal(struct {
		Resource   indexer.Resource `json:"resource"`
		DownloadID string           `json:"download_id"`
	}{Resource: resource, DownloadID: resource.DownloadID})
	if err != nil {
		payload = []byte(resource.DownloadID + "\x00" + resource.Title)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func explicitSeasonZero(title string) bool {
	upper := strings.ToUpper(title)
	for index := 0; index+3 <= len(upper); index++ {
		if upper[index] != 'S' || upper[index+1] != '0' || upper[index+2] != '0' {
			continue
		}
		beforeOK := index == 0 || !isASCIIAlphaNumeric(upper[index-1])
		after := index + 3
		afterOK := after == len(upper) || !isASCIIAlphaNumeric(upper[after]) || upper[after] == 'E'
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}
