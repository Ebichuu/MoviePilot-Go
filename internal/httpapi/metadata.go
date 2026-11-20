package httpapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

const (
	maxMetadataEpisodeNumber = 5_000
	maxMetadataEpisodes      = 10_000
	maxMetadataSeason        = 999
)

func (d Dependencies) identifyMedia(
	ctx context.Context,
	parsed media.ParseResult,
	requestedSource string,
	fallbackTitle string,
) (metadata.Media, bool, error) {
	source := strings.TrimSpace(requestedSource)
	if source == "" {
		source = parsed.MediaSource
	}
	if parsed.MediaID != "" && source != "" {
		id := typedMetadataID(source, parsed.MediaID, parsed.Type)
		item, err := d.Metadata.Detail(ctx, source, id)
		return item, err == nil, err
	}
	keyword := parsed.Name()
	if keyword == "" {
		keyword = strings.TrimSpace(fallbackTitle)
	}
	year, _ := strconv.Atoi(parsed.Year)
	match, found, err := d.Metadata.Recognize(ctx, metadata.Query{
		Keyword: keyword,
		Type:    metadataMediaType(parsed.Type),
		Year:    year,
		Source:  source,
	})
	return match.Media, found, err
}

func typedMetadataID(source, id string, mediaType media.MediaType) string {
	if strings.Contains(id, ":") {
		return id
	}
	if !strings.EqualFold(source, "tmdb") &&
		!strings.EqualFold(source, "themoviedb") &&
		!strings.EqualFold(source, "douban") {
		return id
	}
	if value := metadataMediaType(mediaType); value != "" {
		return value + ":" + id
	}
	return id
}

// v2MediaInfoDefaults 返回 V2 `schemas.MediaInfo` 的完整字段集合的空值。
//
// 两件事必须守住：
//  1. **字段个数与 V2 一致**（58 个）。前端有些地方直接下标/遍历，字段缺失时
//     `undefined[0]`、`.map` 会抛 TypeError，Vue 渲染中断，整页变空白
//     （2026-11-20 用户反馈「点电影进去后面又是空白」）。
//  2. **数组字段给空数组、数值字段给 0**，不要给 null 或缺省：模板写的是
//     `media.runtime || media.episode_run_time[0]`，`undefined[0]` 直接抛异常。
//
// 数据源没给的部分一律留空，不编造；ID 类字段保持 null，与 V2 的
// `Optional[int] = None` 序列化结果一致，由 setV2SourceID 按来源填真实值。
func v2MediaInfoDefaults() map[string]any {
	return map[string]any{
		"source":               nil,
		"scrape_source":        nil,
		"type":                 nil,
		"title":                nil,
		"en_title":             nil,
		"year":                 nil,
		"title_year":           nil,
		"season":               nil,
		"tmdb_id":              nil,
		"imdb_id":              nil,
		"tvdb_id":              nil,
		"douban_id":            nil,
		"bangumi_id":           nil,
		"anilist_id":           nil,
		"anidb_id":             nil,
		"collection_id":        nil,
		"mediaid_prefix":       nil,
		"media_id":             nil,
		"original_language":    nil,
		"original_title":       nil,
		"release_date":         nil,
		"backdrop_path":        nil,
		"poster_path":          nil,
		"vote_average":         0.0,
		"overview":             nil,
		"category":             "",
		"seasons":              map[string]any{},
		"season_info":          []any{},
		"names":                []string{},
		"actors":               []any{},
		"directors":            []any{},
		"detail_link":          nil,
		"adult":                false,
		"created_by":           []any{},
		"episode_run_time":     []int{},
		"genres":               []any{},
		"first_air_date":       nil,
		"homepage":             nil,
		"languages":            []string{},
		"last_air_date":        nil,
		"networks":             []any{},
		"number_of_episodes":   0,
		"number_of_seasons":    0,
		"origin_country":       []string{},
		"original_name":        nil,
		"production_companies": []any{},
		"production_countries": []string{},
		"spoken_languages":     []any{},
		"release_dates":        []any{},
		"status":               nil,
		"tagline":              nil,
		"genre_ids":            []int{},
		"vote_count":           0,
		"popularity":           0.0,
		"runtime":              0,
		"next_episode_to_air":  map[string]any{},
		"episode_groups":       []any{},
		"episode_group":        nil,
	}
}

func v2MediaInfo(item metadata.Media, season int) map[string]any {
	mediaType := ""
	switch strings.ToLower(item.Type) {
	case "movie":
		mediaType = "电影"
	case "tv", "anime":
		mediaType = "电视剧"
	}
	seasons, seasonInfo := v2MediaSeasons(item, season)
	info := v2MediaInfoDefaults()
	info["source"] = item.Source
	info["type"] = mediaType
	info["title"] = item.Title
	info["en_title"] = item.OriginalTitle
	info["year"] = strconv.Itoa(item.Year)
	info["original_title"] = item.OriginalTitle
	info["overview"] = item.Overview
	info["poster_path"] = item.PosterURL
	// Aliases 为空时给空数组：前端会直接 names.map，null 会抛异常。
	aliases := item.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	info["names"] = aliases
	info["mediaid_prefix"] = item.Source
	info["media_id"] = item.ID
	info["seasons"] = seasons
	info["season_info"] = seasonInfo
	info["backdrop_path"] = item.BackdropURL
	info["vote_average"] = item.VoteAverage
	info["original_language"] = item.OriginalLanguage
	if item.OriginCountries != nil {
		info["origin_country"] = item.OriginCountries
	}
	if item.ProductionCountries != nil {
		info["production_countries"] = item.ProductionCountries
	}
	if item.GenreIDs != nil {
		info["genre_ids"] = item.GenreIDs
	}
	if item.Year == 0 {
		info["year"] = ""
		info["title_year"] = item.Title
	} else {
		info["title_year"] = fmt.Sprintf("%s (%d)", item.Title, item.Year)
	}
	if season > 0 {
		info["season"] = season
	}
	// 详情页的「类型 / 片长 / 演员 / 导演」区块读这些字段：数据源给了就填，
	// 没给就保持空数组/0，前端不渲染整块，而不是显示空白卡片。
	info["actors"] = v2MediaPersons(item.Actors)
	info["directors"] = v2MediaPersons(item.Directors)
	if len(item.Genres) > 0 {
		genres := make([]any, 0, len(item.Genres))
		for _, name := range item.Genres {
			genres = append(genres, map[string]any{"id": 0, "name": name})
		}
		info["genres"] = genres
	}
	if item.Runtime > 0 {
		info["runtime"] = item.Runtime
	}
	if len(item.EpisodeRunTime) > 0 {
		info["episode_run_time"] = item.EpisodeRunTime
	}
	// 剧集的「片长」就是单集时长：TMDB 的 tv 详情不给 runtime 时取 episode_run_time[0]，
	// 与前端 `media.runtime || media.episode_run_time[0]` 的取值保持一致。
	if item.Runtime == 0 && len(item.EpisodeRunTime) > 0 {
		info["runtime"] = item.EpisodeRunTime[0]
	}
	if item.NumberOfSeasons > 0 {
		info["number_of_seasons"] = item.NumberOfSeasons
	}
	if item.NumberOfEpisodes > 0 {
		info["number_of_episodes"] = item.NumberOfEpisodes
	}
	if item.ReleaseDate != "" {
		info["release_date"] = item.ReleaseDate
	}
	if item.VoteCount > 0 {
		info["vote_count"] = item.VoteCount
	}
	if item.Popularity > 0 {
		info["popularity"] = item.Popularity
	}
	setV2SourceID(info, item)
	return info
}

// v2MediaPersons 把识别层的演职员转成 V2 `schemas.MediaPerson` 的 JSON 形状。
// 前端 PersonCard 读 source / name / character，头像优先 profile_path（TMDB）。
func v2MediaPersons(people []metadata.MediaPerson) []any {
	result := make([]any, 0, len(people))
	for _, person := range people {
		entry := map[string]any{
			"source": person.Source, "id": person.ID, "type": person.Type,
			"name": person.Name, "character": person.Character,
		}
		if person.ProfilePath != "" {
			entry["profile_path"] = person.ProfilePath
		}
		result = append(result, entry)
	}
	return result
}

func v2MediaSeasons(item metadata.Media, requestedSeason int) (map[int]any, []metadata.SeasonInfo) {
	seasons := make(map[int]any)
	if item.Type != "tv" && item.Type != "anime" {
		return seasons, []metadata.SeasonInfo{}
	}
	if len(item.SeasonInfo) == 0 {
		if item.EpisodeCount < 1 || item.EpisodeCount > maxMetadataEpisodeNumber {
			return seasons, []metadata.SeasonInfo{}
		}
		if requestedSeason <= 0 {
			requestedSeason = 1
		}
		if requestedSeason > maxMetadataSeason {
			return seasons, []metadata.SeasonInfo{}
		}
		seasons[requestedSeason] = v2EpisodeNumbers(item.EpisodeCount)
		return seasons, []metadata.SeasonInfo{}
	}

	seasonInfo := make([]metadata.SeasonInfo, 0, len(item.SeasonInfo))
	seen := make(map[int]struct{}, len(item.SeasonInfo))
	total := 0
	for _, info := range item.SeasonInfo {
		if info.SeasonNumber < 0 || info.SeasonNumber > maxMetadataSeason ||
			info.EpisodeCount < 0 || info.EpisodeCount > maxMetadataEpisodeNumber {
			return map[int]any{}, []metadata.SeasonInfo{}
		}
		if _, exists := seen[info.SeasonNumber]; exists || total > maxMetadataEpisodes-info.EpisodeCount {
			return map[int]any{}, []metadata.SeasonInfo{}
		}
		seen[info.SeasonNumber] = struct{}{}
		total += info.EpisodeCount
		seasonInfo = append(seasonInfo, info)
		seasons[info.SeasonNumber] = v2EpisodeNumbers(info.EpisodeCount)
	}
	return seasons, seasonInfo
}

func v2EpisodeNumbers(count int) []int {
	episodes := make([]int, count)
	for index := range episodes {
		episodes[index] = index + 1
	}
	return episodes
}

func setV2SourceID(info map[string]any, item metadata.Media) {
	id := strings.TrimPrefix(strings.TrimPrefix(item.ID, "movie:"), "tv:")
	switch item.Source {
	case "themoviedb", "tmdb":
		info["tmdb_id"], _ = strconv.Atoi(id)
	case "douban":
		info["douban_id"] = id
	case "bangumi":
		info["bangumi_id"], _ = strconv.Atoi(id)
	case "anilist":
		info["anilist_id"], _ = strconv.Atoi(id)
	}
}

func metadataMediaType(value media.MediaType) string {
	switch value {
	case media.TypeMovie:
		return "movie"
	case media.TypeTV:
		return "tv"
	default:
		return ""
	}
}

func metadataMatchSource(source string) string {
	switch source {
	case "themoviedb", "tmdb":
		return "tmdbid"
	case "douban":
		return "doubanid"
	case "bangumi":
		return "bangumiid"
	case "anilist":
		return "anilistid"
	default:
		return "unknown"
	}
}
