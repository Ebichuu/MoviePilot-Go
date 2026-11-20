package metadata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本文件实现「发现 / 推荐」页面需要的列表型元数据接口。V2 对应
// app/chain/recommend.py 与 app/chain/discover.py：条目形状统一是
// schemas.MediaInfo，因此这里全部返回 Media，由 HTTP 层转换成 V2 字段。

// TMDBListQuery 是 V2 `tmdb_discover` 的查询参数
// （app/modules/themoviedb 的 discover 分支）。
type TMDBListQuery struct {
	SortBy               string
	WithGenres           string
	WithOriginalLanguage string
	WithKeywords         string
	WithWatchProviders   string
	VoteAverage          float64
	VoteCount            int
	ReleaseDate          string
	Page                 int
}

type tmdbListItem struct {
	ID               int      `json:"id"`
	MediaType        string   `json:"media_type"`
	Title            string   `json:"title"`
	Name             string   `json:"name"`
	OriginalTitle    string   `json:"original_title"`
	OriginalName     string   `json:"original_name"`
	OriginalLanguage string   `json:"original_language"`
	ReleaseDate      string   `json:"release_date"`
	FirstAirDate     string   `json:"first_air_date"`
	Overview         string   `json:"overview"`
	PosterPath       string   `json:"poster_path"`
	BackdropPath     string   `json:"backdrop_path"`
	VoteAverage      float64  `json:"vote_average"`
	GenreIDs         []int    `json:"genre_ids"`
	OriginCountries  []string `json:"origin_country"`
}

// media 把 TMDB 列表条目转换成统一媒体结构；mediaType 用于 trending/all
// 这类条目自身不带 media_type 的接口。
func (item tmdbListItem) media(mediaType string) Media {
	kind := item.MediaType
	if kind == "" {
		kind = mediaType
	}
	title, original, date := item.Title, item.OriginalTitle, item.ReleaseDate
	if kind == "tv" {
		title, original, date = item.Name, item.OriginalName, item.FirstAirDate
	}
	return Media{
		Source: "themoviedb", ID: strconv.Itoa(item.ID), Type: kind,
		Title: title, OriginalTitle: original, OriginalLanguage: item.OriginalLanguage,
		Year: yearFromDate(date), Overview: item.Overview,
		PosterURL: tmdbPoster(item.PosterPath), BackdropURL: tmdbBackdrop(item.BackdropPath),
		VoteAverage:     item.VoteAverage,
		GenreIDs:        append([]int(nil), item.GenreIDs...),
		OriginCountries: append([]string(nil), item.OriginCountries...),
	}
}

// Trending 对应 V2 `TmdbChain.tmdb_trending`：/3/trending/all/day。
func (c *TMDB) Trending(ctx context.Context, page int) ([]Media, error) {
	if c.http == nil || c.baseURL == "" {
		return nil, errors.New("TMDB 客户端未配置")
	}
	if page < 1 {
		page = 1
	}
	query := url.Values{"language": {"zh-CN"}, "page": {strconv.Itoa(page)}}
	var response struct {
		Results []tmdbListItem `json:"results"`
	}
	endpoint := c.signed(c.baseURL + "/3/trending/all/day?" + query.Encode())
	if err := c.http.DoJSON(ctx, http.MethodGet, endpoint, c.headers(), nil, &response); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Results))
	for _, item := range response.Results {
		if item.ID <= 0 || (item.MediaType != "" && item.MediaType != "movie" && item.MediaType != "tv") {
			continue
		}
		result = append(result, item.media(item.MediaType))
	}
	return result, nil
}

// Discover 对应 V2 `tmdb_discover`：/3/discover/movie 或 /3/discover/tv。
func (c *TMDB) Discover(ctx context.Context, mediaType string, list TMDBListQuery) ([]Media, error) {
	if c.http == nil || c.baseURL == "" {
		return nil, errors.New("TMDB 客户端未配置")
	}
	if mediaType != "movie" && mediaType != "tv" {
		return nil, errors.New("TMDB 发现类型必须是 movie 或 tv")
	}
	page := list.Page
	if page < 1 {
		page = 1
	}
	query := url.Values{"language": {"zh-CN"}, "page": {strconv.Itoa(page)}}
	if value := strings.TrimSpace(list.SortBy); value != "" {
		query.Set("sort_by", value)
	}
	if value := strings.TrimSpace(list.WithGenres); value != "" {
		query.Set("with_genres", value)
	}
	if value := strings.TrimSpace(list.WithOriginalLanguage); value != "" {
		query.Set("with_original_language", value)
	}
	if value := strings.TrimSpace(list.WithKeywords); value != "" {
		query.Set("with_keywords", value)
	}
	if value := strings.TrimSpace(list.WithWatchProviders); value != "" {
		query.Set("with_watch_providers", value)
	}
	if list.VoteAverage > 0 {
		query.Set("vote_average.gte", strconv.FormatFloat(list.VoteAverage, 'f', -1, 64))
	}
	if list.VoteCount > 0 {
		query.Set("vote_count.gte", strconv.Itoa(list.VoteCount))
	}
	if value := strings.TrimSpace(list.ReleaseDate); value != "" {
		if mediaType == "movie" {
			query.Set("primary_release_date.gte", value)
		} else {
			query.Set("first_air_date.gte", value)
		}
	}
	var response struct {
		Results []tmdbListItem `json:"results"`
	}
	// v3 API Key 必须走 api_key 查询参数，否则 TMDB 直接返回 401：
	// 趋势接口签名了、发现接口漏签，就会表现为「趋势有数据、发现页空白」。
	endpoint := c.signed(c.baseURL + "/3/discover/" + mediaType + "?" + query.Encode())
	if err := c.http.DoJSON(ctx, http.MethodGet, endpoint, c.headers(), nil, &response); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Results))
	for _, item := range response.Results {
		if item.ID <= 0 {
			continue
		}
		result = append(result, item.media(mediaType))
	}
	return result, nil
}

// Calendar 对应 V2 `BangumiChain.calendar`：GET /calendar 返回一周七天的放送列表。
func (c *Bangumi) Calendar(ctx context.Context) ([]Media, error) {
	var response []struct {
		Items []bangumiSubject `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/calendar", nil, &response); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response)*4)
	for _, day := range response {
		for _, subject := range day.Items {
			if subject.ID <= 0 {
				continue
			}
			result = append(result, subject.media())
		}
	}
	return result, nil
}

// Browse 对应 V2 探索页的 Bangumi 浏览：按排名/类型/分类/年份分页查询。
func (c *Bangumi) Browse(ctx context.Context, mediaType, cat, sort, year string, limit, offset int) ([]Media, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if offset < 0 {
		offset = 0
	}
	if strings.TrimSpace(sort) == "" {
		sort = "rank"
	}
	filter := map[string]any{"type": []int{2}}
	if value, err := strconv.Atoi(strings.TrimSpace(mediaType)); err == nil && value > 0 {
		filter["type"] = []int{value}
	}
	if value, err := strconv.Atoi(strings.TrimSpace(cat)); err == nil && value > 0 {
		filter["cat"] = value
	}
	if value := strings.TrimSpace(year); value != "" {
		filter["year"] = value
	}
	var response struct {
		Data []bangumiSubject `json:"data"`
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	body := map[string]any{"keyword": "", "sort": sort, "filter": filter}
	if err := c.do(ctx, http.MethodPost, "/v0/search/subjects?"+query.Encode(), body, &response); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Data))
	for _, subject := range response.Data {
		if subject.ID <= 0 {
			continue
		}
		result = append(result, subject.media())
	}
	return result, nil
}

const aniListListFields = `id title { romaji english native } seasonYear startDate { year }
      description(asHtml: false) coverImage { extraLarge large } bannerImage averageScore episodes synonyms`

const aniListTrendingQuery = `query ($page: Int!, $perPage: Int!) {
  Page(page: $page, perPage: $perPage) {
    media(type: ANIME, sort: TRENDING_DESC) {
      ` + aniListListFields + `
    }
  }
}`

const aniListSeasonQuery = `query ($page: Int!, $perPage: Int!, $season: MediaSeason!, $seasonYear: Int!) {
  Page(page: $page, perPage: $perPage) {
    media(type: ANIME, season: $season, seasonYear: $seasonYear, sort: POPULARITY_DESC) {
      ` + aniListListFields + `
    }
  }
}`

// aniListDiscoverQuery 对应探索页的 AniList 列表：只按热度排序，不限定季度。
const aniListDiscoverQuery = `query ($page: Int!, $perPage: Int!) {
  Page(page: $page, perPage: $perPage) {
    media(type: ANIME, sort: POPULARITY_DESC) {
      ` + aniListListFields + `
    }
  }
}`

// Trending 对应 V2 `/anilist/trending`：AniList 当前流行番剧。
func (c *AniList) Trending(ctx context.Context, page, perPage int) ([]Media, error) {
	return c.list(ctx, aniListTrendingQuery, map[string]any{
		"page": normalizeAniListPage(page), "perPage": normalizeAniListPageSize(perPage),
	})
}

// DiscoverBrowse 对应 V2 `/anilist/discover`：按热度排序的番剧列表（探索页用）。
func (c *AniList) DiscoverBrowse(ctx context.Context, page, perPage int) ([]Media, error) {
	return c.list(ctx, aniListDiscoverQuery, map[string]any{
		"page": normalizeAniListPage(page), "perPage": normalizeAniListPageSize(perPage),
	})
}

// SeasonPopular 对应 V2 `/anilist/popular-this-season`：当季热门番剧。
func (c *AniList) SeasonPopular(ctx context.Context, season string, year, page, perPage int) ([]Media, error) {
	if season == "" || year <= 0 {
		return nil, errors.New("AniList 季度参数无效")
	}
	return c.list(ctx, aniListSeasonQuery, map[string]any{
		"page": normalizeAniListPage(page), "perPage": normalizeAniListPageSize(perPage),
		"season": season, "seasonYear": year,
	})
}

func (c *AniList) list(ctx context.Context, query string, variables map[string]any) ([]Media, error) {
	var response struct {
		Data struct {
			Page struct {
				Media []aniListMedia `json:"media"`
			} `json:"Page"`
		} `json:"data"`
		Errors []aniListError `json:"errors"`
	}
	if err := c.do(ctx, query, variables, &response); err != nil {
		return nil, err
	}
	if err := aniListResponseError(response.Errors); err != nil {
		return nil, err
	}
	result := make([]Media, 0, len(response.Data.Page.Media))
	for _, item := range response.Data.Page.Media {
		if item.ID > 0 {
			result = append(result, item.media())
		}
	}
	return result, nil
}

func normalizeAniListPage(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

func normalizeAniListPageSize(perPage int) int {
	if perPage < 1 || perPage > 50 {
		return 30
	}
	return perPage
}

// AniListSeason 把时间映射成 AniList 的 MediaSeason 枚举与年份。
func AniListSeason(now time.Time) (string, int) {
	switch month := now.Month(); {
	case month <= 3:
		return "WINTER", now.Year()
	case month <= 6:
		return "SPRING", now.Year()
	case month <= 9:
		return "SUMMER", now.Year()
	default:
		return "FALL", now.Year()
	}
}

// Discover 汇集「发现 / 推荐」需要的列表接口，持有与识别层相同的客户端。
type Discover struct {
	// mu 保护 tmdb：系统设置里改了 TMDB 密钥后运行期会换掉这个客户端
	// （V2 的 CONFIG_WATCH 同样会重建 TMDB 客户端），发现/推荐列表立刻生效。
	mu      sync.RWMutex
	tmdb    *TMDB
	douban  *Douban
	bangumi *Bangumi
	anilist *AniList
	now     func() time.Time
}

// NewDiscover 创建列表型元数据门面；任一客户端为空时对应来源视为不可用。
func NewDiscover(tmdb *TMDB, douban *Douban, bangumi *Bangumi, anilist *AniList) *Discover {
	return &Discover{tmdb: tmdb, douban: douban, bangumi: bangumi, anilist: anilist, now: time.Now}
}

// SetTMDB 替换 TMDB 客户端；nil 表示 TMDB 不可用（例如密钥被清空）。
func (d *Discover) SetTMDB(client *TMDB) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.tmdb = client
	d.mu.Unlock()
}

// tmdbClient 取当前 TMDB 客户端快照。
func (d *Discover) tmdbClient() *TMDB {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.tmdb
}

// TMDBAvailable 报告 TMDB 是否可用（未配置 Token 时 V2 也拿不到数据）。
func (d *Discover) TMDBAvailable() bool {
	client := d.tmdbClient()
	return client != nil && client.token != ""
}

// BangumiAvailable 报告 Bangumi 是否可用。
func (d *Discover) BangumiAvailable() bool {
	return d != nil && d.bangumi != nil
}

// AniListAvailable 报告 AniList 是否可用。
func (d *Discover) AniListAvailable() bool {
	return d != nil && d.anilist != nil
}

// DoubanAvailable 报告豆瓣 API 是否可用（需要 API Key）。
func (d *Discover) DoubanAvailable() bool {
	return d != nil && d.douban != nil && d.douban.apiKey != ""
}

// TMDBTrending 返回 TMDB 流行趋势。
func (d *Discover) TMDBTrending(ctx context.Context, page int) ([]Media, error) {
	client := d.tmdbClient()
	if client == nil || client.token == "" {
		return nil, errors.New("TMDB 未配置")
	}
	return client.Trending(ctx, page)
}

// TMDBDiscover 返回 TMDB 发现结果。
func (d *Discover) TMDBDiscover(ctx context.Context, mediaType string, list TMDBListQuery) ([]Media, error) {
	client := d.tmdbClient()
	if client == nil || client.token == "" {
		return nil, errors.New("TMDB 未配置")
	}
	return client.Discover(ctx, mediaType, list)
}

// BangumiCalendar 返回 Bangumi 每日放送，并按 V2 的 page/count 切片。
func (d *Discover) BangumiCalendar(ctx context.Context, page, count int) ([]Media, error) {
	if !d.BangumiAvailable() {
		return nil, errors.New("Bangumi 未配置")
	}
	items, err := d.bangumi.Calendar(ctx)
	if err != nil {
		return nil, err
	}
	return slicePage(items, page, count), nil
}

// BangumiBrowse 返回 Bangumi 浏览结果。
func (d *Discover) BangumiBrowse(ctx context.Context, mediaType, cat, sort, year string, page, count int) ([]Media, error) {
	if !d.BangumiAvailable() {
		return nil, errors.New("Bangumi 未配置")
	}
	if page < 1 {
		page = 1
	}
	if count <= 0 || count > 100 {
		count = 30
	}
	return d.bangumi.Browse(ctx, mediaType, cat, sort, year, count, (page-1)*count)
}

// AniListTrending 返回 AniList 流行番剧。
func (d *Discover) AniListTrending(ctx context.Context, page int) ([]Media, error) {
	if !d.AniListAvailable() {
		return nil, errors.New("AniList 未配置")
	}
	return d.anilist.Trending(ctx, page, 30)
}

// AniListDiscover 返回 AniList 探索列表（按热度，不限季度）。
func (d *Discover) AniListDiscover(ctx context.Context, page int) ([]Media, error) {
	if !d.AniListAvailable() {
		return nil, errors.New("AniList 未配置")
	}
	return d.anilist.DiscoverBrowse(ctx, page, 30)
}

// AniListPopularThisSeason 返回 AniList 当季热门番剧。
func (d *Discover) AniListPopularThisSeason(ctx context.Context, page int) ([]Media, error) {
	if !d.AniListAvailable() {
		return nil, errors.New("AniList 未配置")
	}
	season, year := AniListSeason(d.now())
	return d.anilist.SeasonPopular(ctx, season, year, page, 30)
}

func slicePage(items []Media, page, count int) []Media {
	if page < 1 {
		page = 1
	}
	if count <= 0 {
		count = 30
	}
	start := (page - 1) * count
	if start >= len(items) {
		return nil
	}
	end := start + count
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// describe 便于错误信息带上来来源名称。
func describe(source string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", source, err)
}

// doubanCollectionPaths 复刻 V2 `DoubanApi._urls`
// （app/modules/douban/apiv2.py）里的主题合集路径。
var doubanCollectionPaths = map[string]string{
	"movie_showing":     "/subject_collection/movie_showing/items",
	"movie_hot":         "/subject_collection/movie_hot_gaia/items",
	"movie_top250":      "/subject_collection/movie_top250/items",
	"tv_hot":            "/subject_collection/tv_hot/items",
	"tv_animation":      "/subject_collection/tv_animation/items",
	"tv_weekly_chinese": "/subject_collection/tv_chinese_best_weekly/items",
	"tv_weekly_global":  "/subject_collection/tv_global_best_weekly/items",
}

// Collection 查询豆瓣主题合集（正在热映、TOP250、口碑周榜等）。
func (c *Douban) Collection(ctx context.Context, name string, page, count int) ([]Media, error) {
	path, ok := doubanCollectionPaths[name]
	if !ok {
		return nil, errors.New("豆瓣合集名称无效")
	}
	mediaType := "movie"
	if strings.HasPrefix(name, "tv_") {
		mediaType = "tv"
	}
	start, size := doubanWindow(page, count)
	var response struct {
		Items []doubanSubject `json:"subject_collection_items"`
	}
	query := url.Values{"start": {strconv.Itoa(start)}, "count": {strconv.Itoa(size)}}
	if err := c.get(ctx, path, query, &response); err != nil {
		return nil, err
	}
	return doubanMedias(response.Items, mediaType), nil
}

// RecommendList 查询豆瓣电影/剧集推荐，探索页使用 /movie/recommend 与 /tv/recommend。
func (c *Douban) RecommendList(ctx context.Context, mediaType, sort, tags string, page, count int) ([]Media, error) {
	path := "/movie/recommend"
	if mediaType == "tv" {
		path = "/tv/recommend"
	}
	start, size := doubanWindow(page, count)
	var response struct {
		Items []doubanSubject `json:"items"`
	}
	query := url.Values{"start": {strconv.Itoa(start)}, "count": {strconv.Itoa(size)}}
	if value := strings.TrimSpace(sort); value != "" {
		query.Set("sort", value)
	}
	if value := strings.TrimSpace(tags); value != "" {
		query.Set("tags", value)
	}
	if err := c.get(ctx, path, query, &response); err != nil {
		return nil, err
	}
	return doubanMedias(response.Items, mediaType), nil
}

func doubanMedias(items []doubanSubject, defaultType string) []Media {
	result := make([]Media, 0, len(items))
	for _, item := range items {
		mediaType := doubanMediaType("", item.Type)
		if mediaType == "" {
			mediaType = defaultType
		}
		if mediaType == "" || string(item.ID) == "" || item.Title == "" {
			continue
		}
		result = append(result, item.media(mediaType))
	}
	return result
}

func doubanWindow(page, count int) (int, int) {
	if page < 1 {
		page = 1
	}
	if count <= 0 || count > 100 {
		count = 30
	}
	return (page - 1) * count, count
}

// DoubanCollection 查询豆瓣主题合集。
func (d *Discover) DoubanCollection(ctx context.Context, name string, page, count int) ([]Media, error) {
	if !d.DoubanAvailable() {
		return nil, errors.New("豆瓣 API 未配置")
	}
	return d.douban.Collection(ctx, name, page, count)
}

// DoubanRecommend 查询豆瓣探索列表。
func (d *Discover) DoubanRecommend(ctx context.Context, mediaType, sort, tags string, page, count int) ([]Media, error) {
	if !d.DoubanAvailable() {
		return nil, errors.New("豆瓣 API 未配置")
	}
	// V2 里 `/recommend/douban_movies`、`/recommend/douban_tvs` 的 sort 默认是 "R"，
	// 探索页不传 sort。少了这个默认值豆瓣会返回「片单/杂项」条目——实测首条是
	// 《10分钟就让你【欲罢不能】的高分电影》这种豆列，标题、年份、海报全对不上，
	// 页面上就是一排怪条目。所以空 sort 必须补 "R"。
	if strings.TrimSpace(sort) == "" {
		sort = "R"
	}
	return d.douban.RecommendList(ctx, mediaType, sort, tags, page, count)
}
