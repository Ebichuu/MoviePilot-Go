package httpapi

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// mediaSearch 对应 V2 `GET /media/search?title=&type=media|collection|person`。
// 本实现只有媒体数据源，合集/人物搜索返回空列表（V2 的这些类型来自插件提供的
// 探索数据源），媒体搜索按页码与数量截断后返回 V2 MediaInfo 数组。
func (d Dependencies) mediaSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	title := strings.TrimSpace(query.Get("title"))
	searchType := strings.ToLower(strings.TrimSpace(query.Get("type")))
	if searchType == "" {
		searchType = "media"
	}
	if title == "" || searchType != "media" || d.Metadata == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	result, err := d.Metadata.Search(r.Context(), metadata.Query{
		Keyword: title,
		Source:  strings.TrimSpace(query.Get("source")),
	})
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	page := parseHistoryInt(query.Get("page"), 1, 1, 1000)
	count := parseHistoryInt(query.Get("count"), 8, 1, 50)
	start := (page - 1) * count
	items := make([]map[string]any, 0, count)
	for index := start; index < len(result.Matches) && len(items) < count; index++ {
		items = append(items, v2MediaInfo(result.Matches[index].Media, 0))
	}
	writeJSON(w, http.StatusOK, items)
}

// mediaDetail 对应 V2 `GET /media/{mediaid}?type_name=电影|电视剧&title=&year=`：
// 先按 `source:id` 直查数据源，失败且带 title 时退回按标题识别，
// 仍无结果时返回全空的 MediaInfo（V2 同样返回 `schemas.MediaInfo()`）。
func (d Dependencies) mediaDetail(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	mediaID := strings.TrimSpace(mediaKeyParam(r, "mediaid"))
	typeName := strings.TrimSpace(query.Get("type_name"))
	title := strings.TrimSpace(query.Get("title"))
	year := strings.TrimSpace(query.Get("year"))

	mediaType := media.TypeUnknown
	switch typeName {
	case "电影":
		mediaType = media.TypeMovie
	case "电视剧":
		mediaType = media.TypeTV
	}

	var item metadata.Media
	found := false
	if d.Metadata != nil {
		if source, id, ok := splitMediaKey(mediaID); ok {
			if resolved, err := d.Metadata.Detail(
				r.Context(), source, typedMetadataID(source, id, mediaType),
			); err == nil {
				item, found = resolved, true
			}
		}
		if !found && title != "" {
			parsed := media.Parse(title, media.Options{})
			if year != "" {
				parsed.Year = year
			}
			if mediaType != media.TypeUnknown {
				parsed.Type = mediaType
			}
			if resolved, ok, err := d.identifyMedia(r.Context(), parsed, "", title); err == nil && ok {
				item, found = resolved, true
			}
			// 中文名在 TMDB 上常与原名不同，识别不中时按电视剧结果回退，
			// 避免「能搜到但拿不到季信息」。
			if !found && d.Metadata != nil {
				if result, searchErr := d.Metadata.Search(r.Context(), metadata.Query{Keyword: title}); searchErr == nil {
					for _, match := range result.Matches {
						if match.Media.Type != string(media.TypeTV) && !strings.EqualFold(match.Media.Type, "tv") {
							continue
						}
						if detail, detailErr := d.Metadata.Detail(r.Context(), match.Media.Source, typedMetadataID(match.Media.Source, match.Media.ID, media.TypeTV)); detailErr == nil {
							item, found = detail, true
						}
						break
					}
				}
			}
		}
	}
	if !found {
		writeJSON(w, http.StatusOK, emptyMediaInfo())
		return
	}
	writeJSON(w, http.StatusOK, v2MediaInfo(item, 0))
}

// emptyMediaInfo 返回 V2 空 MediaInfo：字段齐全但全部为空，前端据此显示「未识别」。
//
// 数组类字段必须给空数组（`episode_run_time`/`genres`/`names` 等），否则详情页模板
// 直接下标或遍历时抛异常，整页空白。
func emptyMediaInfo() map[string]any {
	info := v2MediaInfoDefaults()
	info["source"] = nil
	info["type"] = nil
	info["title"] = nil
	info["en_title"] = nil
	info["year"] = nil
	info["original_title"] = nil
	info["overview"] = nil
	info["poster_path"] = nil
	info["names"] = []string{}
	info["mediaid_prefix"] = nil
	info["media_id"] = nil
	info["seasons"] = map[string]any{}
	info["season_info"] = []any{}
	info["title_year"] = nil
	return info
}

// mediaRecognizeFile 对应 V2 `GET /media/recognize_file?path=`：按文件路径识别媒体信息，
// 返回 `schemas.Context` 的字段子集（meta_info + media_info）。
func (d Dependencies) mediaRecognizeFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeError(w, http.StatusBadRequest, "path不能为空")
		return
	}
	parsed := media.Parse(filepath.Base(strings.ReplaceAll(path, "\\", "/")), media.Options{})
	metaInfo := map[string]any{
		"title": strings.TrimSpace(parsed.Name()), "en_title": strings.TrimSpace(parsed.Name()),
		"year": strings.TrimSpace(parsed.Year), "season": strings.TrimSpace(parsed.Season()),
		"episode": strings.TrimSpace(parsed.Episode()), "type": string(parsed.Type),
		"original_name": strings.TrimSpace(parsed.OriginalName), "path": path,
	}
	var mediaInfo any
	if d.Metadata != nil {
		if item, found, err := d.identifyMedia(r.Context(), parsed, r.URL.Query().Get("source"), parsed.Name()); err == nil && found {
			mediaInfo = v2MediaInfo(item, parsed.BeginSeason)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"meta_info": metaInfo, "media_info": mediaInfo,
	})
}

// mediaSeasons 对应 V2 `GET /media/seasons?mediaid=&title=&year=&season=`：
// 返回 V2 `MediaSeason[]`。本项目的数据源只提供季号/集数/首播日期，
// 名称按「第 N 季」生成，其余描述字段留空。
func (d Dependencies) mediaSeasons(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	title := strings.TrimSpace(query.Get("title"))
	year := strings.TrimSpace(query.Get("year"))
	mediaKey := strings.TrimSpace(query.Get("mediaid"))
	season := parseHistoryInt(query.Get("season"), 0, 0, maxMetadataSeason)

	var item metadata.Media
	found := false
	if d.Metadata != nil {
		if source, id, ok := splitMediaKey(mediaKey); ok {
			if resolved, err := d.Metadata.Detail(
				r.Context(), source, typedMetadataID(source, id, media.TypeTV),
			); err == nil {
				item, found = resolved, true
			}
		}
		if !found && title != "" {
			// 季信息只有 TMDB 提供：标题识别优先走 themoviedb，避免命中没有季表的源。
			if result, searchErr := d.Metadata.Search(r.Context(), metadata.Query{Keyword: title, Source: "themoviedb"}); searchErr == nil {
				for _, match := range result.Matches {
					detail, detailErr := d.Metadata.Detail(r.Context(), match.Media.Source, typedMetadataID(match.Media.Source, match.Media.ID, media.TypeTV))
					if detailErr == nil && len(detail.SeasonInfo) > 0 {
						item, found = detail, true
						break
					}
				}
			}
			parsed := media.Parse(title, media.Options{})
			parsed.Type = media.TypeTV
			if year != "" {
				parsed.Year = year
			}
			if resolved, ok, err := d.identifyMedia(r.Context(), parsed, "", title); err == nil && ok {
				item, found = resolved, true
			}
			// 中文名在 TMDB 上常与原名不同，识别不中时按电视剧结果回退，
			// 避免「能搜到但拿不到季信息」。
			if !found && d.Metadata != nil {
				if result, searchErr := d.Metadata.Search(r.Context(), metadata.Query{Keyword: title}); searchErr == nil {
					for _, match := range result.Matches {
						if match.Media.Type != string(media.TypeTV) && !strings.EqualFold(match.Media.Type, "tv") {
							continue
						}
						if detail, detailErr := d.Metadata.Detail(r.Context(), match.Media.Source, typedMetadataID(match.Media.Source, match.Media.ID, media.TypeTV)); detailErr == nil {
							item, found = detail, true
						}
						break
					}
				}
			}
		}
	}

	// 搜索结果只有基础信息，季列表要再取一次详情。
	if found && len(item.SeasonInfo) == 0 && item.ID != "" && d.Metadata != nil {
		if detail, detailErr := d.Metadata.Detail(r.Context(), item.Source, typedMetadataID(item.Source, item.ID, media.TypeTV)); detailErr == nil {
			item = detail
		}
	}
	seasons := make([]map[string]any, 0, len(item.SeasonInfo))
	if found {
		for _, info := range item.SeasonInfo {
			if season > 0 && info.SeasonNumber != season {
				continue
			}
			seasons = append(seasons, map[string]any{
				"air_date": info.AirDate, "episode_count": info.EpisodeCount,
				"name": fmt.Sprintf("第 %d 季", info.SeasonNumber), "overview": nil,
				"poster_path": item.PosterURL, "season_number": info.SeasonNumber,
				"vote_average": nil,
			})
		}
	}
	writeJSON(w, http.StatusOK, seasons)
}
