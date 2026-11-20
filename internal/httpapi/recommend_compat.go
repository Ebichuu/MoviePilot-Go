package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// 本文件实现「发现 / 推荐」页面（V2 app/api/endpoints/recommend.py 与 discover.py）
// 的数据入口。所有列表统一返回 schemas.MediaInfo 数组，字段由 v2MediaInfo 生成。
// 没有配置对应来源时返回空数组而不是 404：页面显示「暂无数据」，与 V2 的
// 「来源不可用」表现一致。

func writeMediaList(w http.ResponseWriter, items []metadata.Media) {
	list := make([]map[string]any, 0, len(items))
	for _, item := range items {
		list = append(list, v2MediaInfo(item, 0))
	}
	writeJSON(w, http.StatusOK, list)
}

// mediaPage 读取 V2 的 page/count 查询参数。
func mediaPage(r *http.Request, defaultCount int) (int, int) {
	page, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page")))
	if page < 1 {
		page = 1
	}
	count, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("count")))
	if count <= 0 || count > 100 {
		count = defaultCount
	}
	return page, count
}

// emptySourceList 对应 V2 `/recommend/source` 与 `/discover/source`。
//
// V2 的语义是「列出插件额外注册的推荐/探索数据源」（RecommendSource /
// DiscoverSource 链事件）；没有插件时它同样返回 `[]`，内置的 TMDB、豆瓣、
// Bangumi、AniList 卡片由前端直接调用各自的列表接口。因此这里返回空数组
// 是行为对齐，而不是缺陷——真正的空白来自列表接口本身取不到数据。
func (d Dependencies) emptySourceList(w http.ResponseWriter, r *http.Request) {
	if d.Logger != nil {
		d.Logger.DebugContext(r.Context(), "没有插件注册额外的推荐/探索数据源",
			"path", r.URL.Path)
	}
	writeJSON(w, http.StatusOK, []any{})
}

// tmdbReady 报告 TMDB 是否可用；未配置 Token 时 V2 也拿不到数据，
// 这里直接返回空列表，避免整块卡片报错。
func (d Dependencies) tmdbReady() bool {
	return d.Discover != nil && d.Discover.TMDBAvailable()
}

func (d Dependencies) recommendTMDBTrending(w http.ResponseWriter, r *http.Request) {
	if !d.tmdbReady() {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	page, _ := mediaPage(r, 20)
	items, err := d.Discover.TMDBTrending(r.Context(), page)
	if err != nil {
		d.logRecommendError(r.Context(), "tmdb_trending", err)
		writeError(w, http.StatusBadGateway, "TMDB请求失败")
		return
	}
	writeMediaList(w, items)
}

func (d Dependencies) tmdbDiscover(mediaType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.tmdbReady() {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		query := r.URL.Query()
		rate, _ := strconv.ParseFloat(strings.TrimSpace(query.Get("vote_average")), 64)
		votes, _ := strconv.Atoi(strings.TrimSpace(query.Get("vote_count")))
		page, _ := mediaPage(r, 20)
		items, err := d.Discover.TMDBDiscover(r.Context(), mediaType, metadata.TMDBListQuery{
			SortBy:               query.Get("sort_by"),
			WithGenres:           query.Get("with_genres"),
			WithOriginalLanguage: query.Get("with_original_language"),
			WithKeywords:         query.Get("with_keywords"),
			WithWatchProviders:   query.Get("with_watch_providers"),
			VoteAverage:          rate,
			VoteCount:            votes,
			ReleaseDate:          query.Get("release_date"),
			Page:                 page,
		})
		if err != nil {
			// 发现页在 V2 里对 TMDB 异常只返回空列表，推荐页才升级为 502。
			d.logRecommendError(r.Context(), "tmdb_discover", err)
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeMediaList(w, items)
	}
}

func (d Dependencies) recommendBangumiCalendar(w http.ResponseWriter, r *http.Request) {
	if d.Discover == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	page, count := mediaPage(r, 30)
	items, err := d.Discover.BangumiCalendar(r.Context(), page, count)
	if err != nil {
		d.logRecommendError(r.Context(), "bangumi_calendar", err)
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeMediaList(w, items)
}

func (d Dependencies) discoverBangumi(w http.ResponseWriter, r *http.Request) {
	if d.Discover == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	query := r.URL.Query()
	page, count := mediaPage(r, 30)
	items, err := d.Discover.BangumiBrowse(
		r.Context(), query.Get("type"), query.Get("cat"), query.Get("sort"), query.Get("year"), page, count,
	)
	if err != nil {
		d.logRecommendError(r.Context(), "bangumi_discover", err)
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeMediaList(w, items)
}

// doubanCollection 对应 V2 的主题合集接口（正在热映、TOP250、口碑周榜等）。
func (d Dependencies) doubanCollection(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Discover == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		page, count := mediaPage(r, 30)
		items, err := d.Discover.DoubanCollection(r.Context(), name, page, count)
		if err != nil {
			d.logRecommendError(r.Context(), "douban_"+name, err)
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeMediaList(w, items)
	}
}

// doubanDiscover 对应探索页的豆瓣电影/剧集列表。
func (d Dependencies) doubanDiscover(mediaType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Discover == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		query := r.URL.Query()
		page, count := mediaPage(r, 30)
		items, err := d.Discover.DoubanRecommend(
			r.Context(), mediaType, query.Get("sort"), query.Get("tags"), page, count,
		)
		if err != nil {
			d.logRecommendError(r.Context(), "douban_discover", err)
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeMediaList(w, items)
	}
}

func (d Dependencies) anilistTrending(w http.ResponseWriter, r *http.Request) {
	if d.Discover == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	page, _ := mediaPage(r, 30)
	items, err := d.Discover.AniListTrending(r.Context(), page)
	if err != nil {
		d.logRecommendError(r.Context(), "anilist_trending", err)
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeMediaList(w, items)
}

// anilistDiscover 对应 V2 `GET /anilist/discover`：探索页的 AniList 列表。
//
// 之前这里是 GraphQL 透传，返回 `{Page:{media:[...]}}` 对象——探索页拿到的不是数组，
// 于是 AniList 一行永远是空的。现在按 V2 的 `List[MediaInfo]` 输出。
func (d Dependencies) anilistDiscover(w http.ResponseWriter, r *http.Request) {
	if d.Discover == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	page, _ := mediaPage(r, 30)
	items, err := d.Discover.AniListDiscover(r.Context(), page)
	if err != nil {
		d.logRecommendError(r.Context(), "anilist_discover", err)
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeMediaList(w, items)
}

func (d Dependencies) anilistPopularThisSeason(w http.ResponseWriter, r *http.Request) {
	if d.Discover == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	page, _ := mediaPage(r, 30)
	items, err := d.Discover.AniListPopularThisSeason(r.Context(), page)
	if err != nil {
		d.logRecommendError(r.Context(), "anilist_season", err)
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeMediaList(w, items)
}

func (d Dependencies) logRecommendError(ctx context.Context, operation string, err error) {
	if d.Logger == nil {
		return
	}
	d.Logger.WarnContext(ctx, "推荐/探索数据源请求失败", "operation", operation, "error", err)
}

func (d Dependencies) registerRecommendRoutes(router chi.Router) {
	router.Get("/recommend/source", d.emptySourceList)
	router.Get("/discover/source", d.emptySourceList)
	router.Get("/recommend/tmdb_trending", d.recommendTMDBTrending)
	router.Get("/recommend/tmdb_movies", d.tmdbDiscover("movie"))
	router.Get("/recommend/tmdb_tvs", d.tmdbDiscover("tv"))
	router.Get("/discover/tmdb_movies", d.tmdbDiscover("movie"))
	router.Get("/discover/tmdb_tvs", d.tmdbDiscover("tv"))
	router.Get("/recommend/bangumi_calendar", d.recommendBangumiCalendar)
	router.Get("/discover/bangumi", d.discoverBangumi)
	router.Get("/recommend/douban_showing", d.doubanCollection("movie_showing"))
	router.Get("/recommend/douban_movie_hot", d.doubanCollection("movie_hot"))
	router.Get("/recommend/douban_movie_top250", d.doubanCollection("movie_top250"))
	router.Get("/recommend/douban_tv_hot", d.doubanCollection("tv_hot"))
	router.Get("/recommend/douban_tv_animation", d.doubanCollection("tv_animation"))
	router.Get("/recommend/douban_tv_weekly_chinese", d.doubanCollection("tv_weekly_chinese"))
	router.Get("/recommend/douban_tv_weekly_global", d.doubanCollection("tv_weekly_global"))
	router.Get("/recommend/douban_movies", d.doubanDiscover("movie"))
	router.Get("/recommend/douban_tvs", d.doubanDiscover("tv"))
	router.Get("/discover/douban_movies", d.doubanDiscover("movie"))
	router.Get("/discover/douban_tvs", d.doubanDiscover("tv"))
	router.Get("/anilist/trending", d.anilistTrending)
	router.Get("/anilist/popular-this-season", d.anilistPopularThisSeason)
}
