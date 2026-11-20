package metadata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const maxTMDBSupplementCandidates = 10

var errTMDBSupplementDetailBudgetExhausted = errors.New("TMDB 辅助详情预算已耗尽")

type tmdbSupplementDetailResult struct {
	media Media
	err   error
}

// TMDBSupplementInput describes a primary-source item and optional TV season facts.
type TMDBSupplementInput struct {
	Media      Media
	Season     *int
	SeasonYear int
}

// TMDBSupplementer resolves bounded TMDB facts without changing primary-source identity.
type TMDBSupplementer struct {
	coordinator *Coordinator
}

// NewTMDBSupplementer creates an auxiliary resolver that reuses Coordinator cache, timeout and rate limits.
func NewTMDBSupplementer(coordinator *Coordinator) (*TMDBSupplementer, error) {
	if coordinator == nil {
		return nil, errors.New("TMDB 辅助事实协调器不能为空")
	}
	if _, ok := coordinator.providers["themoviedb"]; !ok {
		return nil, errors.New("TMDB 元数据源未配置")
	}
	return &TMDBSupplementer{coordinator: coordinator}, nil
}

// Supplement resolves TMDB detail and fills only missing auxiliary fields.
// A clean miss returns the unchanged primary item with found=false. Provider failures are returned to the caller.
func (s *TMDBSupplementer) Supplement(ctx context.Context, input TMDBSupplementInput) (Media, bool, error) {
	primary := cloneMedia(input.Media)
	if ctx == nil {
		return primary, false, errors.New("TMDB 辅助事实 context 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return primary, false, err
	}
	if normalizeSource(primary.Source) == "themoviedb" {
		if primary.TMDBID == "" {
			primary.TMDBID = primary.ID
		}
		return primary, true, nil
	}
	mediaType := normalizeMediaType(primary.Type)
	if primary.TMDBID != "" && mediaType != "" {
		detail, err := s.coordinator.Detail(ctx, "themoviedb", mediaType+":"+primary.TMDBID)
		if err != nil {
			return primary, false, fmt.Errorf("读取 TMDB 辅助详情: %w", err)
		}
		return mergeTMDBSupplement(primary, detail), true, nil
	}
	names := supplementNames(primary)
	if len(names) == 0 {
		return primary, false, nil
	}

	seen := make(map[string]struct{})
	details := make(map[string]tmdbSupplementDetailResult)
	detailBudget := maxTMDBSupplementCandidates
	readDetail := func(candidate Media) (Media, error) {
		key := mediaIdentity(candidate)
		if cached, exists := details[key]; exists {
			return cached.media, cached.err
		}
		if detailBudget == 0 {
			return Media{}, errTMDBSupplementDetailBudgetExhausted
		}
		detailBudget--
		detail, err := s.coordinator.Detail(ctx, "themoviedb", candidate.Type+":"+candidate.ID)
		if err != nil {
			err = fmt.Errorf("读取 TMDB 辅助详情: %w", err)
		}
		details[key] = tmdbSupplementDetailResult{media: detail, err: err}
		return detail, err
	}
	for _, name := range names {
		candidates := make([]Media, 0)
		result, err := s.coordinator.Search(ctx, Query{Keyword: name, Source: "themoviedb"})
		if err != nil {
			return primary, false, fmt.Errorf("搜索 TMDB 辅助事实: %w", err)
		}
		for _, match := range result.Matches {
			candidate := match.Media
			if !supplementNameMatches(name, candidate) {
				continue
			}
			identity := mediaIdentity(candidate)
			if _, exists := seen[identity]; exists {
				continue
			}
			seen[identity] = struct{}{}
			candidates = append(candidates, candidate)
		}
		if len(candidates) == 0 {
			continue
		}

		detail, found, err := s.selectDetail(candidates, mediaType, primary.Year, input.Season, input.SeasonYear, readDetail)
		if err != nil {
			if errors.Is(err, errTMDBSupplementDetailBudgetExhausted) {
				return primary, false, nil
			}
			return primary, false, err
		}
		if found {
			return mergeTMDBSupplement(primary, detail), true, nil
		}
	}
	return primary, false, nil
}

func supplementNames(item Media) []string {
	// V2 只从主源的中文名（含简繁转换）和英文名构造查询。当前模型没有
	// 独立简繁名称字段，因此这里使用对应的标题和原标题，不把无界 aliases
	// 展开成额外请求。
	values := []string{item.Title, item.OriginalTitle}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		normalized := supplementName(value)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, value)
	}
	return result
}

func supplementNameMatches(name string, item Media) bool {
	want := supplementName(name)
	for _, candidate := range append([]string{item.Title, item.OriginalTitle}, item.Aliases...) {
		if supplementName(candidate) == want {
			return true
		}
	}
	return false
}

func supplementName(value string) string {
	var result strings.Builder
	for _, character := range strings.ToUpper(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			result.WriteRune(character)
		}
	}
	return result.String()
}

func orderSupplementCandidates(items []Media) {
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].Year != items[right].Year {
			return items[left].Year > items[right].Year
		}
		return false
	})
}

func (s *TMDBSupplementer) selectDetail(
	candidates []Media,
	mediaType string,
	year int,
	season *int,
	seasonYear int,
	readDetail func(Media) (Media, error),
) (Media, bool, error) {
	movies, shows := splitSupplementCandidates(candidates)
	orderSupplementCandidates(movies)
	orderSupplementCandidates(shows)

	if mediaType == "tv" {
		if season != nil && seasonYear > 0 {
			for _, candidate := range limitSupplementCandidates(shows) {
				detail, err := readDetail(candidate)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return Media{}, false, err
					}
					continue
				}
				if candidate.Year == seasonYear || seasonMatchesYear(detail.SeasonInfo, *season, seasonYear) {
					return detail, true, nil
				}
			}
		}
		return firstSupplementDetail(readDetail, limitSupplementCandidates(orderedYearFallback(shows, year)))
	}

	if mediaType != "tv" {
		if ordered := limitSupplementCandidates(orderedYearMatches(movies, year)); len(ordered) > 0 {
			return firstSupplementDetail(readDetail, ordered)
		}
		if ordered := limitSupplementCandidates(orderedYearMatches(shows, year)); len(ordered) > 0 {
			return firstSupplementDetail(readDetail, ordered)
		}
	}

	// V2 的无年份 multi 回退把电影排在剧集前，再分别按年份降序。
	multi := append(append(make([]Media, 0, len(candidates)), movies...), shows...)
	return firstSupplementDetail(readDetail, limitSupplementCandidates(multi))
}

func limitSupplementCandidates(candidates []Media) []Media {
	if len(candidates) <= maxTMDBSupplementCandidates {
		return candidates
	}
	return candidates[:maxTMDBSupplementCandidates]
}

func splitSupplementCandidates(candidates []Media) (movies, shows []Media) {
	for _, candidate := range candidates {
		switch normalizeMediaType(candidate.Type) {
		case "movie":
			movies = append(movies, candidate)
		case "tv":
			shows = append(shows, candidate)
		}
	}
	return movies, shows
}

func orderedYearMatches(candidates []Media, year int) []Media {
	if year == 0 {
		return nil
	}
	result := make([]Media, 0, len(candidates))
	for _, wanted := range []int{year, year + 1, year - 1} {
		for _, candidate := range candidates {
			if candidate.Year == wanted {
				result = append(result, candidate)
			}
		}
	}
	return result
}

func orderedYearFallback(candidates []Media, year int) []Media {
	ordered := orderedYearMatches(candidates, year)
	seen := make(map[string]struct{}, len(ordered))
	for _, candidate := range ordered {
		seen[mediaIdentity(candidate)] = struct{}{}
	}
	for _, candidate := range candidates {
		if _, exists := seen[mediaIdentity(candidate)]; !exists {
			ordered = append(ordered, candidate)
		}
	}
	return ordered
}

func firstSupplementDetail(readDetail func(Media) (Media, error), candidates []Media) (Media, bool, error) {
	if len(candidates) == 0 {
		return Media{}, false, nil
	}
	detail, err := readDetail(candidates[0])
	if err != nil {
		return Media{}, false, err
	}
	return detail, true, nil
}

func seasonMatchesYear(seasons []SeasonInfo, season, year int) bool {
	for _, item := range seasons {
		if item.SeasonNumber == season && yearFromDate(item.AirDate) == year {
			return true
		}
	}
	return false
}

func mergeTMDBSupplement(primary, tmdb Media) Media {
	result := cloneMedia(primary)
	if result.TMDBID == "" {
		result.TMDBID = tmdb.ID
	}
	if result.OriginalLanguage == "" {
		result.OriginalLanguage = tmdb.OriginalLanguage
	}
	if len(result.GenreIDs) == 0 {
		result.GenreIDs = append([]int(nil), tmdb.GenreIDs...)
	}
	if len(result.OriginCountries) == 0 {
		result.OriginCountries = append([]string(nil), tmdb.OriginCountries...)
	}
	if len(result.ProductionCountries) == 0 {
		result.ProductionCountries = append([]string(nil), tmdb.ProductionCountries...)
	}
	if len(result.SeasonInfo) == 0 {
		result.SeasonInfo = append([]SeasonInfo(nil), tmdb.SeasonInfo...)
	}
	return result
}

func cloneMedia(item Media) Media {
	item.Aliases = append([]string(nil), item.Aliases...)
	item.SeasonInfo = append([]SeasonInfo(nil), item.SeasonInfo...)
	item.GenreIDs = append([]int(nil), item.GenreIDs...)
	item.OriginCountries = append([]string(nil), item.OriginCountries...)
	item.ProductionCountries = append([]string(nil), item.ProductionCountries...)
	item.EpisodeRunTime = append([]int(nil), item.EpisodeRunTime...)
	item.Genres = append([]string(nil), item.Genres...)
	item.Actors = append([]MediaPerson(nil), item.Actors...)
	item.Directors = append([]MediaPerson(nil), item.Directors...)
	return item
}
