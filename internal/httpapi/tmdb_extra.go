package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// tmdbSeasons 对应 V2 `GET /tmdb/seasons/{tmdbid}`：返回该剧集的季列表。
func (d Dependencies) tmdbSeasons(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.tmdbSeasonList(r, chi.URLParam(r, "tmdbid"), 0))
}

// tmdbSeasonDetail 对应 V2 `GET /tmdb/{tmdbid}/{season}`：返回指定季的详情。
func (d Dependencies) tmdbSeasonDetail(w http.ResponseWriter, r *http.Request) {
	season := parseHistoryInt(chi.URLParam(r, "season"), 0, 0, maxMetadataSeason)
	list := d.tmdbSeasonList(r, chi.URLParam(r, "tmdbid"), season)
	if len(list) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, list[0])
}

func (d Dependencies) tmdbSeasonList(r *http.Request, tmdbID string, season int) []map[string]any {
	seasons := []map[string]any{}
	if d.Metadata == nil || strings.TrimSpace(tmdbID) == "" {
		return seasons
	}
	item, err := d.Metadata.Detail(r.Context(), "themoviedb", typedMetadataID("themoviedb", tmdbID, media.TypeTV))
	if err != nil {
		return seasons
	}
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
	return seasons
}

// tmdbCache 对应 V2 `GET /tmdb/cache`：本项目不对 TMDB 识别结果做持久缓存。
func tmdbCache(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{"count": 0, "data": []any{}}})
}

// tmdbCacheClear 对应 V2 `DELETE /tmdb/cache`，无缓存可清，按 V2 语义成功。
func tmdbCacheClear(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// tmdbCacheDelete 对应 V2 `DELETE /tmdb/cache/{cache_key}`，同上。
func tmdbCacheDelete(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Success: true})
}

var _ = metadata.Media{}
