package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// mediaGroups 对应 V2 `GET /media/groups/{tmdbid}`：返回 TMDB 的剧集组列表。
func (d Dependencies) mediaGroups(w http.ResponseWriter, r *http.Request) {
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	provider, ok := d.Metadata.Provider("themoviedb")
	tmdb, assertOK := provider.(*metadata.TMDB)
	if !ok || !assertOK || tmdb == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	result, err := tmdb.Raw(r.Context(), "tv/"+chi.URLParam(r, "tmdbid")+"/episode_groups", nil)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	groups, _ := result["results"].([]any)
	if groups == nil {
		groups = []any{}
	}
	writeJSON(w, http.StatusOK, groups)
}

// mediaGroupSeasons 对应 V2 `GET /media/group/seasons/{episode_group}`：返回某个剧集组的详情。
func (d Dependencies) mediaGroupSeasons(w http.ResponseWriter, r *http.Request) {
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	provider, ok := d.Metadata.Provider("themoviedb")
	tmdb, assertOK := provider.(*metadata.TMDB)
	if !ok || !assertOK || tmdb == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	result, err := tmdb.Raw(r.Context(), "tv/episode_group/"+chi.URLParam(r, "episode_group"), nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
