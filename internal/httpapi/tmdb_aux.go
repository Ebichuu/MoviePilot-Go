package httpapi

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// tmdbPassthrough 把「透传 TMDB 形状」的接口统一实现：拿底层 TMDB 供应商直接请求
// V2 对应的 REST 路径，原始 JSON 原样返回。
func (d Dependencies) tmdbPassthrough(
	w http.ResponseWriter, r *http.Request,
	build func(*http.Request) (string, url.Values),
) {
	if d.Metadata == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置元数据源"})
		return
	}
	provider, ok := d.Metadata.Provider("themoviedb")
	tmdb, assertOK := provider.(*metadata.TMDB)
	if !ok || !assertOK || tmdb == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "未配置 TMDB 数据源"})
		return
	}
	path, query := build(r)
	result, err := tmdb.Raw(r.Context(), path, query)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Warn("TMDB 透传失败", "path", path, "error", err)
		}
		if d.Logger != nil {
			d.Logger.Warn("TMDB 请求失败", "path", path, "error", err)
		}
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "TMDB 请求失败，请检查网络与凭据"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func tmdbMediaType(typeName string) string {
	if typeName == string(media.TypeTV) {
		return "tv"
	}
	return "movie"
}

// tmdbCollection 对应 V2 `GET /tmdb/collection/{collection_id}`。
func (d Dependencies) tmdbCollection(w http.ResponseWriter, r *http.Request) {
	d.tmdbPassthrough(w, r, func(r *http.Request) (string, url.Values) {
		return "collection/" + chi.URLParam(r, "collection_id"), nil
	})
}

// tmdbCreditsPageSize 是演职员分页大小，与 V2 的 count=24 一致。
const tmdbCreditsPageSize = 24

// tmdbCredits 对应 V2 `GET /tmdb/credits/{tmdbid}/{type_name}`。
//
// 这个接口不能像其它 TMDB 接口那样原样透传：V2 返回的是 `List[MediaPerson]`
// （只含 cast，每条补 source=themoviedb，profile_path 相对路径）。前端
// PersonCardListView 是把响应当数组 concat 进列表的，透传 TMDB 的
// `{id, cast, crew}` 对象会让整张演员卡片渲染成空白。
func (d Dependencies) tmdbCredits(w http.ResponseWriter, r *http.Request) {
	persons := []metadata.MediaPerson{}
	tmdbID, err := strconv.Atoi(strings.TrimSpace(chi.URLParam(r, "tmdbid")))
	if err != nil || tmdbID <= 0 {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	kind := creditsMediaType(chi.URLParam(r, "type_name"))
	if kind == "" {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	client, ok := d.tmdbClient()
	if !ok {
		writeJSON(w, http.StatusOK, persons)
		return
	}
	list, callErr := client.Credits(r.Context(), kind, tmdbID, creditsPageFromRequest(r), tmdbCreditsPageSize)
	if callErr != nil {
		// V2 在模块内部吞掉异常返回空表；这里同样降级为空表，但留下日志便于排查。
		slog.WarnContext(r.Context(), "TMDB 演职员查询失败", "tmdbid", tmdbID, "type", kind, "error", callErr)
		writeJSON(w, http.StatusOK, persons)
		return
	}
	writeJSON(w, http.StatusOK, append(persons, list...))
}

// tmdbClient 取当前生效的 TMDB 客户端；未配置密钥时 ok 为 false。
func (d Dependencies) tmdbClient() (*metadata.TMDB, bool) {
	if d.Metadata == nil {
		return nil, false
	}
	provider, ok := d.Metadata.Provider("themoviedb")
	if !ok {
		return nil, false
	}
	client, assertOK := provider.(*metadata.TMDB)
	if !assertOK || client == nil {
		return nil, false
	}
	return client, true
}

// creditsMediaType 把前端传来的类型名换成 TMDB 的 movie/tv。
// 中文（电影/电视剧）和英文（movie/tv）都接受，避免前端两种取值任一种落空。
func creditsMediaType(typeName string) string {
	switch strings.ToLower(strings.TrimSpace(typeName)) {
	case "电影", "movie":
		return "movie"
	case "电视剧", "tv":
		return "tv"
	default:
		return ""
	}
}

// creditsPageFromRequest 读取 page 参数，非法值按第 1 页处理（V2 默认 page=1）。
func creditsPageFromRequest(r *http.Request) int {
	page, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page")))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

// tmdbPerson 对应 V2 `GET /tmdb/person/{person_id}`。
func (d Dependencies) tmdbPerson(w http.ResponseWriter, r *http.Request) {
	d.tmdbPassthrough(w, r, func(r *http.Request) (string, url.Values) {
		return "person/" + chi.URLParam(r, "person_id"), nil
	})
}

// tmdbPersonCredits 对应 V2 `GET /tmdb/person/credits/{person_id}`。
func (d Dependencies) tmdbPersonCredits(w http.ResponseWriter, r *http.Request) {
	d.tmdbPassthrough(w, r, func(r *http.Request) (string, url.Values) {
		return "person/" + chi.URLParam(r, "person_id") + "/combined_credits", nil
	})
}

// tmdbRecommend 对应 V2 `GET /tmdb/recommend/{tmdbid}/{type_name}`。
func (d Dependencies) tmdbRecommend(w http.ResponseWriter, r *http.Request) {
	d.tmdbPassthrough(w, r, func(r *http.Request) (string, url.Values) {
		mediaType := tmdbMediaType(chi.URLParam(r, "type_name"))
		return mediaType + "/" + chi.URLParam(r, "tmdbid") + "/recommendations", nil
	})
}

// tmdbSimilar 对应 V2 `GET /tmdb/similar/{tmdbid}/{type_name}`。
func (d Dependencies) tmdbSimilar(w http.ResponseWriter, r *http.Request) {
	d.tmdbPassthrough(w, r, func(r *http.Request) (string, url.Values) {
		mediaType := tmdbMediaType(chi.URLParam(r, "type_name"))
		return mediaType + "/" + chi.URLParam(r, "tmdbid") + "/similar", url.Values{"page": []string{"1"}}
	})
}

var _ = strings.TrimSpace
