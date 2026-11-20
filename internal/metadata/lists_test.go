package metadata

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newListTestClient(t *testing.T) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// 这些用例锁定「发现 / 推荐」列表接口到统一 Media 结构的映射：上游响应形状
// 来自 V2 模块的实测返回，字段缺失时必须降级而不是报错。

func TestTMDBTrendingAndDiscoverMapItems(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/3/trending/all/day" && got != "/3/discover/movie" {
			t.Errorf("unexpected path %q", got)
		}
		if r.URL.Path == "/3/discover/movie" && r.URL.Query().Get("vote_count.gte") != "100" {
			t.Errorf("discover 未透传 vote_count.gte: %q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"results":[
			{"id":550,"media_type":"movie","title":"搏击俱乐部","original_title":"Fight Club",
			 "release_date":"1999-10-15","vote_average":8.4,"poster_path":"/p.jpg"},
			{"id":1399,"media_type":"tv","name":"权力的游戏","original_name":"Game of Thrones",
			 "first_air_date":"2011-04-17","vote_average":8.4}
		]}`)
	}))
	t.Cleanup(server.Close)

	client := NewTMDB(newListTestClient(t), server.URL, "token")

	trending, err := client.Trending(t.Context(), 1)
	if err != nil {
		t.Fatalf("Trending() error = %v", err)
	}
	if len(trending) != 2 {
		t.Fatalf("Trending() = %d 条, want 2", len(trending))
	}
	if trending[0].Source != "themoviedb" || trending[0].Title != "搏击俱乐部" || trending[0].Year != 1999 {
		t.Fatalf("电影条目映射错误: %#v", trending[0])
	}
	if !strings.HasPrefix(trending[0].PosterURL, "http") {
		t.Fatalf("海报路径未展开: %q", trending[0].PosterURL)
	}
	if trending[1].Type != "tv" || trending[1].Title != "权力的游戏" || trending[1].Year != 2011 {
		t.Fatalf("剧集条目映射错误: %#v", trending[1])
	}

	discovered, err := client.Discover(t.Context(), "movie", TMDBListQuery{SortBy: "popularity.desc", VoteCount: 100})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(discovered) != 2 {
		t.Fatalf("Discover() = %d 条, want 2", len(discovered))
	}
}

func TestDoubanCollectionMapsSubjects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subject_collection/movie_showing/items" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"subject_collection_items":[
			{"id":"1292052","type":"movie","title":"肖申克的救赎","year":"1994",
			 "rating":{"value":9.7},"pic":{"large":"https://img.example/a.jpg"}},
			{"id":"","title":"脏数据"}
		]}`)
	}))
	t.Cleanup(server.Close)

	client := NewDouban(newListTestClient(t), server.URL, "key", "secret")
	items, err := client.Collection(t.Context(), "movie_showing", 1, 10)
	if err != nil {
		t.Fatalf("Collection() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Collection() = %d 条, want 1（缺 ID 的条目必须跳过）", len(items))
	}
	if items[0].Source != "douban" || items[0].Type != "movie" || items[0].Year != 1994 {
		t.Fatalf("豆瓣条目映射错误: %#v", items[0])
	}
}

func TestBangumiCalendarMapsSubjects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendar" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `[{"weekday":{"id":1},"items":[
			{"id":253,"type":2,"name":"Cowboy Bebop","name_cn":"星际牛仔","date":"1998-04-03",
			 "images":{"large":"https://img.example/b.jpg"},"rating":{"score":9.1},"total_episodes":26}
		]}]`)
	}))
	t.Cleanup(server.Close)

	client := NewBangumi(newListTestClient(t), server.URL, "")
	items, err := client.Calendar(t.Context())
	if err != nil {
		t.Fatalf("Calendar() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Calendar() = %d 条, want 1", len(items))
	}
	if items[0].Source != "bangumi" || items[0].Type != "tv" || items[0].Year != 1998 || items[0].EpisodeCount != 26 {
		t.Fatalf("Bangumi 条目映射错误: %#v", items[0])
	}
}

func TestAniListListQueriesAndSeasonWindow(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		queries = append(queries, string(body))
		_, _ = io.WriteString(w, `{"data":{"Page":{"media":[
			{"id":21,"title":{"romaji":"ONE PIECE","english":"One Piece"},"seasonYear":1999,
			 "averageScore":88,"episodes":1000,"coverImage":{"extraLarge":"https://img.example/c.jpg"}}
		]}}}`)
	}))
	t.Cleanup(server.Close)

	client := NewAniList(newListTestClient(t), server.URL)
	items, err := client.Trending(t.Context(), 1, 30)
	if err != nil {
		t.Fatalf("Trending() error = %v", err)
	}
	if len(items) != 1 || items[0].Type != "tv" || items[0].VoteAverage != 8.8 {
		t.Fatalf("AniList 条目映射错误: %#v", items)
	}
	if _, err := client.SeasonPopular(t.Context(), "FALL", 2025, 1, 30); err != nil {
		t.Fatalf("SeasonPopular() error = %v", err)
	}
	if !strings.Contains(queries[0], "TRENDING_DESC") {
		t.Fatalf("trending 查询未使用 TRENDING_DESC: %s", queries[0])
	}
	if !strings.Contains(queries[1], "POPULARITY_DESC") || !strings.Contains(queries[1], "FALL") {
		t.Fatalf("当季查询参数错误: %s", queries[1])
	}

	if season, year := AniListSeason(time.Date(2025, 11, 17, 0, 0, 0, 0, time.UTC)); season != "FALL" || year != 2025 {
		t.Fatalf("AniListSeason() = %s %d, want FALL 2025", season, year)
	}
	if season, _ := AniListSeason(time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)); season != "WINTER" {
		t.Fatalf("二月应落在 WINTER，实际 %s", season)
	}
}

// TMDB v3 API Key（32 位十六进制）只能用 api_key 查询参数认证；任何一个
// 端点漏签都会表现为 401，进而让「发现 / 推荐」空白或刮削全部失败。
// 该用例逐个打满列表接口与刮削接口，防止再次漏签。
func TestTMDBV3KeySignsEveryEndpoint(t *testing.T) {
	const v3Key = "0123456789abcdef0123456789abcdef"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("api_key"); got != v3Key {
			t.Errorf("%s 缺少 api_key 查询参数: %q", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("%s v3 Key 不应使用 Authorization 头: %q", r.URL.Path, got)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/3/discover/"), r.URL.Path == "/3/trending/all/day":
			_, _ = io.WriteString(w, `{"results":[{"id":550,"media_type":"movie","title":"x"}]}`)
		case strings.Contains(r.URL.Path, "/images"):
			_, _ = io.WriteString(w, `{"id":550,"posters":[],"backdrops":[]}`)
		case strings.Contains(r.URL.Path, "/season/"):
			_, _ = io.WriteString(w, `{"id":1,"name":"season","poster_path":"/p.jpg","episodes":[]}`)
		default:
			_, _ = io.WriteString(w, `{"id":550,"title":"x","name":"x","seasons":[],"episodes":[]}`)
		}
	}))
	t.Cleanup(server.Close)

	client := NewTMDB(newListTestClient(t), server.URL, v3Key)
	if _, err := client.Trending(t.Context(), 1); err != nil {
		t.Fatalf("Trending() error = %v", err)
	}
	if _, err := client.Discover(t.Context(), "movie", TMDBListQuery{SortBy: "popularity.desc"}); err != nil {
		t.Fatalf("Discover(movie) error = %v", err)
	}
	if _, err := client.Discover(t.Context(), "tv", TMDBListQuery{SortBy: "popularity.desc"}); err != nil {
		t.Fatalf("Discover(tv) error = %v", err)
	}

	adapter := NewTMDBScraperAdapter(client)
	if _, err := adapter.GetMovieDetails(t.Context(), 550); err != nil {
		t.Fatalf("GetMovieDetails() error = %v", err)
	}
	if _, err := adapter.GetTVShowDetails(t.Context(), 1399); err != nil {
		t.Fatalf("GetTVShowDetails() error = %v", err)
	}
	if _, err := adapter.GetEpisodeDetails(t.Context(), 1399, 1, 1); err != nil {
		t.Fatalf("GetEpisodeDetails() error = %v", err)
	}
	if _, err := adapter.GetSeasonPosterURL(t.Context(), 1399, 1); err != nil {
		t.Fatalf("GetSeasonPosterURL() error = %v", err)
	}
	if _, err := adapter.getMediaImages(t.Context(), "movie", 550, "zh"); err != nil {
		t.Fatalf("getMediaImages() error = %v", err)
	}
	if requests < 8 {
		t.Fatalf("只发出 %d 个请求，用例未覆盖全部端点", requests)
	}
}
