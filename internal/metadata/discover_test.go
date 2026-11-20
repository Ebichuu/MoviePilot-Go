package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newDiscoverServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/3/trending/all/day":
			_, _ = io.WriteString(w, `{"results":[{"id":550,"media_type":"movie","title":"搏击俱乐部","release_date":"1999-10-15"}]}`)
		case "/3/discover/movie":
			_, _ = io.WriteString(w, `{"results":[{"id":27205,"title":"盗梦空间","release_date":"2010-07-16"}]}`)
		case "/calendar":
			_, _ = io.WriteString(w, `[{"weekday":{"id":1},"items":[
				{"id":253,"type":2,"name":"Cowboy Bebop","name_cn":"星际牛仔","date":"1998-04-03"}]}]`)
		case "/v0/search/subjects":
			_, _ = io.WriteString(w, `{"data":[{"id":8,"type":2,"name":"Steins;Gate","name_cn":"命运石之门","date":"2011-04-06"}]}`)
		case "/subject_collection/movie_showing/items":
			_, _ = io.WriteString(w, `{"subject_collection_items":[{"id":"1292052","type":"movie","title":"肖申克的救赎","year":"1994"}]}`)
		case "/movie/recommend":
			_, _ = io.WriteString(w, `{"items":[{"id":"1291546","type":"movie","title":"霸王别姬","year":"1993"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newDiscoverAniListServer(t *testing.T, seen *map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析 AniList 请求失败: %v", err)
		}
		if seen != nil {
			*seen = body
		}
		_, _ = io.WriteString(w, `{"data":{"Page":{"media":[
			{"id":1,"title":{"romaji":"Cowboy Bebop","native":"カウボーイビバップ"},
			 "seasonYear":1998,"startDate":{"year":1998},"averageScore":86,"episodes":26}]}}}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestProviderNames(t *testing.T) {
	client := newListTestClient(t)
	cases := map[string]Provider{
		"TMDB":    NewTMDB(client, "https://example.test", "token"),
		"Bangumi": NewBangumi(client, "https://example.test", ""),
		"AniList": NewAniList(client, "https://example.test"),
		"Douban":  NewDouban(client, "https://example.test", "key", "secret"),
	}
	want := map[string]string{
		"TMDB": "themoviedb", "Bangumi": "bangumi", "AniList": "anilist", "Douban": "douban",
	}
	for name, provider := range cases {
		if got := provider.Name(); got != want[name] {
			t.Fatalf("%s.Name() = %q, want %q", name, got, want[name])
		}
	}
}

func TestCoordinatorProviderLookup(t *testing.T) {
	client := newListTestClient(t)
	bangumi := NewBangumi(client, "https://example.test", "")
	anilist := NewAniList(client, "https://example.test")
	coordinator, err := NewCoordinator([]Provider{bangumi, anilist}, Options{
		RequestTimeout: 2 * time.Second, CacheTTL: time.Minute, MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("NewCoordinator() = %v", err)
	}
	if provider, ok := coordinator.Provider("bangumi"); !ok || provider != bangumi {
		t.Fatalf("Provider(bangumi) = (%v, %v)", provider, ok)
	}
	if provider, ok := coordinator.Provider("anilist"); !ok || provider != anilist {
		t.Fatalf("Provider(anilist) = (%v, %v)", provider, ok)
	}
	if _, ok := coordinator.Provider("missing"); ok {
		t.Fatal("未注册的来源不应命中")
	}
	if _, ok := coordinator.Provider("BANGUMI"); ok {
		t.Fatal("来源名区分大小写")
	}
}

func TestDiscoverAvailability(t *testing.T) {
	var nilDiscover *Discover
	if nilDiscover.TMDBAvailable() || nilDiscover.BangumiAvailable() ||
		nilDiscover.AniListAvailable() || nilDiscover.DoubanAvailable() {
		t.Fatal("nil 门面不应报告任何来源可用")
	}
	empty := &Discover{}
	if empty.TMDBAvailable() || empty.DoubanAvailable() {
		t.Fatal("空客户端不应可用")
	}

	client := newListTestClient(t)
	// 没有 Token / API Key 时即使客户端存在也视为不可用。
	discover := NewDiscover(
		NewTMDB(client, "https://example.test", ""),
		NewDouban(client, "https://example.test", "", ""),
		NewBangumi(client, "https://example.test", ""),
		NewAniList(client, "https://example.test"),
	)
	if discover.TMDBAvailable() {
		t.Fatal("缺少 Token 时 TMDB 应不可用")
	}
	if discover.DoubanAvailable() {
		t.Fatal("缺少 API Key 时豆瓣应不可用")
	}
	if !discover.BangumiAvailable() || !discover.AniListAvailable() {
		t.Fatal("Bangumi/AniList 只要客户端存在即可用")
	}

	configured := NewDiscover(
		NewTMDB(client, "https://example.test", "token"),
		NewDouban(client, "https://example.test", "key", ""),
		nil, nil,
	)
	if !configured.TMDBAvailable() || !configured.DoubanAvailable() {
		t.Fatal("配置齐全时应可用")
	}
}

func TestDiscoverTMDBDelegates(t *testing.T) {
	server := newDiscoverServer(t)
	client := newListTestClient(t)
	bangumi := NewBangumi(client, server.URL, "")
	anilist := NewAniList(client, server.URL)
	discover := NewDiscover(NewTMDB(client, server.URL, "token"), nil, bangumi, anilist)

	trending, err := discover.TMDBTrending(t.Context(), 1)
	if err != nil || len(trending) != 1 || trending[0].Title != "搏击俱乐部" {
		t.Fatalf("TMDBTrending() = (%#v, %v)", trending, err)
	}
	discovered, err := discover.TMDBDiscover(t.Context(), "movie", TMDBListQuery{SortBy: "popularity.desc"})
	if err != nil || len(discovered) != 1 || discovered[0].Year != 2010 {
		t.Fatalf("TMDBDiscover() = (%#v, %v)", discovered, err)
	}
	// 参数错误由底层客户端返回。
	if _, err := discover.TMDBDiscover(t.Context(), "person", TMDBListQuery{}); err == nil {
		t.Fatal("非法发现类型应报错")
	}

	unavailable := NewDiscover(nil, nil, nil, nil)
	if _, err := unavailable.TMDBTrending(t.Context(), 1); err == nil || !strings.Contains(err.Error(), "TMDB 未配置") {
		t.Fatalf("未配置 TMDB = %v", err)
	}
	if _, err := unavailable.TMDBDiscover(t.Context(), "movie", TMDBListQuery{}); err == nil {
		t.Fatal("未配置 TMDB 发现应报错")
	}
}

func TestDiscoverBangumiDelegates(t *testing.T) {
	server := newDiscoverServer(t)
	client := newListTestClient(t)
	discover := NewDiscover(nil, nil, NewBangumi(client, server.URL, ""), nil)

	calendar, err := discover.BangumiCalendar(t.Context(), 1, 10)
	if err != nil || len(calendar) != 1 || calendar[0].Title == "" {
		t.Fatalf("BangumiCalendar() = (%#v, %v)", calendar, err)
	}
	// 第二页越界时返回空切片。
	paged, err := discover.BangumiCalendar(t.Context(), 2, 10)
	if err != nil || len(paged) != 0 {
		t.Fatalf("越界分页 = (%#v, %v)", paged, err)
	}
	browsed, err := discover.BangumiBrowse(t.Context(), "2", "", "", "2011", 0, 0)
	if err != nil || len(browsed) != 1 {
		t.Fatalf("BangumiBrowse() = (%#v, %v)", browsed, err)
	}

	unavailable := NewDiscover(nil, nil, nil, nil)
	if _, err := unavailable.BangumiCalendar(t.Context(), 1, 10); err == nil ||
		!strings.Contains(err.Error(), "Bangumi 未配置") {
		t.Fatalf("未配置 Bangumi = %v", err)
	}
	if _, err := unavailable.BangumiBrowse(t.Context(), "", "", "", "", 1, 0); err == nil {
		t.Fatal("未配置 Bangumi 浏览应报错")
	}
}

func TestDiscoverAniListDelegates(t *testing.T) {
	seen := map[string]any{}
	server := newDiscoverAniListServer(t, &seen)
	client := newListTestClient(t)
	discover := NewDiscover(nil, nil, nil, NewAniList(client, server.URL))
	discover.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }

	trending, err := discover.AniListTrending(t.Context(), 2)
	if err != nil || len(trending) != 1 || trending[0].Title != "Cowboy Bebop" {
		t.Fatalf("AniListTrending() = (%#v, %v)", trending, err)
	}
	variables, _ := seen["variables"].(map[string]any)
	if variables["page"] != float64(2) || variables["perPage"] != float64(30) {
		t.Fatalf("趋势分页变量 = %#v", variables)
	}

	seasonal, err := discover.AniListPopularThisSeason(t.Context(), 1)
	if err != nil || len(seasonal) != 1 {
		t.Fatalf("AniListPopularThisSeason() = (%#v, %v)", seasonal, err)
	}
	variables, _ = seen["variables"].(map[string]any)
	if variables["season"] != "SPRING" || variables["seasonYear"] != float64(2026) {
		t.Fatalf("当季变量 = %#v", variables)
	}

	unavailable := NewDiscover(nil, nil, nil, nil)
	if _, err := unavailable.AniListTrending(t.Context(), 1); err == nil ||
		!strings.Contains(err.Error(), "AniList 未配置") {
		t.Fatalf("未配置 AniList = %v", err)
	}
	if _, err := unavailable.AniListPopularThisSeason(t.Context(), 1); err == nil {
		t.Fatal("未配置 AniList 当季应报错")
	}
}

func TestDiscoverDoubanDelegates(t *testing.T) {
	server := newDiscoverServer(t)
	client := newListTestClient(t)
	discover := NewDiscover(nil, NewDouban(client, server.URL, "key", ""), nil, nil)

	collection, err := discover.DoubanCollection(t.Context(), "movie_showing", 1, 10)
	if err != nil || len(collection) != 1 || collection[0].Year != 1994 {
		t.Fatalf("DoubanCollection() = (%#v, %v)", collection, err)
	}
	recommend, err := discover.DoubanRecommend(t.Context(), "movie", "U", "", 1, 10)
	if err != nil || len(recommend) != 1 || recommend[0].Title != "霸王别姬" {
		t.Fatalf("DoubanRecommend() = (%#v, %v)", recommend, err)
	}
	// 非法合集名由底层返回错误。
	if _, err := discover.DoubanCollection(t.Context(), "unknown", 1, 10); err == nil {
		t.Fatal("非法合集名应报错")
	}

	unavailable := NewDiscover(nil, nil, nil, nil)
	if _, err := unavailable.DoubanCollection(t.Context(), "movie_showing", 1, 10); err == nil ||
		!strings.Contains(err.Error(), "豆瓣 API 未配置") {
		t.Fatalf("未配置豆瓣 = %v", err)
	}
	if _, err := unavailable.DoubanRecommend(t.Context(), "movie", "", "", 1, 10); err == nil {
		t.Fatal("未配置豆瓣推荐应报错")
	}
}

func TestSlicePageAndDescribe(t *testing.T) {
	items := []Media{{ID: "1"}, {ID: "2"}, {ID: "3"}}
	cases := []struct {
		page, count int
		want        int
	}{
		{0, 2, 2},
		{1, 0, 3},
		{2, 2, 1},
		{3, 2, 0},
		{-1, -5, 3},
	}
	for _, testCase := range cases {
		if got := slicePage(items, testCase.page, testCase.count); len(got) != testCase.want {
			t.Fatalf("slicePage(%d, %d) = %d 条，期望 %d", testCase.page, testCase.count, len(got), testCase.want)
		}
	}
	if slicePage(nil, 1, 10) != nil {
		t.Fatal("空列表分页应返回 nil")
	}

	if err := describe("bangumi", nil); err != nil {
		t.Fatalf("describe(nil) = %v", err)
	}
	wrapped := describe("bangumi", errors.New("boom"))
	if wrapped == nil || !strings.HasPrefix(wrapped.Error(), "bangumi: ") {
		t.Fatalf("describe() = %v", wrapped)
	}
	if !errors.Is(wrapped, wrapped) || !strings.Contains(wrapped.Error(), "boom") {
		t.Fatalf("describe() 丢失原因: %v", wrapped)
	}
}

func TestAniListSeasonBoundaries(t *testing.T) {
	cases := []struct {
		month  time.Month
		season string
	}{
		{time.January, "WINTER"},
		{time.March, "WINTER"},
		{time.April, "SPRING"},
		{time.June, "SPRING"},
		{time.July, "SUMMER"},
		{time.September, "SUMMER"},
		{time.October, "FALL"},
		{time.December, "FALL"},
	}
	for _, testCase := range cases {
		season, year := AniListSeason(time.Date(2026, testCase.month, 15, 0, 0, 0, 0, time.UTC))
		if season != testCase.season || year != 2026 {
			t.Fatalf("%v 映射为 (%q, %d)", testCase.month, season, year)
		}
	}
}

func TestRawPassthrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/3/movie/550"):
			if r.URL.Query().Get("language") != "zh-CN" {
				t.Errorf("TMDB Raw 未补语言: %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"id":550,"title":"搏击俱乐部"}`)
		case r.URL.Path == "/subjects/253":
			_, _ = io.WriteString(w, `{"id":253,"name":"Cowboy Bebop"}`)
		case r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"Media":{"id":1}}}`)
		case r.URL.Path == "/movie/550/credits":
			_, _ = io.WriteString(w, `{"id":550,"cast":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newListTestClient(t)

	tmdb := NewTMDB(client, server.URL, "token")
	raw, err := tmdb.Raw(t.Context(), "/movie/550", nil)
	if err != nil || raw["title"] != "搏击俱乐部" {
		t.Fatalf("TMDB.Raw() = (%#v, %v)", raw, err)
	}
	if _, err := tmdb.Raw(t.Context(), "  ", nil); err == nil {
		t.Fatal("TMDB.Raw 空路径应报错")
	}
	if _, err := NewTMDB(client, "", "token").Raw(t.Context(), "movie/550", nil); err == nil {
		t.Fatal("TMDB.Raw 未配置应报错")
	}

	bangumi := NewBangumi(client, server.URL, "")
	subject, err := bangumi.Raw(t.Context(), "/subjects/253", url.Values{"limit": {"1"}})
	if err != nil || subject == nil {
		t.Fatalf("Bangumi.Raw() = (%#v, %v)", subject, err)
	}
	if _, err := bangumi.Raw(t.Context(), "   ", nil); err == nil {
		t.Fatal("Bangumi.Raw 空路径应报错")
	}

	anilist := NewAniList(client, server.URL+"/graphql")
	data, err := anilist.Raw(t.Context(), "query { Media(id: 1) { id } }", nil)
	if err != nil || data["Media"] == nil {
		t.Fatalf("AniList.Raw() = (%#v, %v)", data, err)
	}

	douban := NewDouban(client, server.URL, "key", "")
	credits, err := douban.Raw(t.Context(), "/movie/550/credits", nil)
	if err != nil || credits == nil {
		t.Fatalf("Douban.Raw() = (%#v, %v)", credits, err)
	}
}

// TestRawPassthroughErrorPaths 覆盖透传接口的上游失败与空 data 分支。
func TestRawPassthroughErrorPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graphql-empty":
			_, _ = io.WriteString(w, `{"data":null}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	client := newListTestClient(t)

	if _, err := NewTMDB(client, server.URL, "token").Raw(t.Context(), "movie/550", nil); err == nil {
		t.Fatal("TMDB.Raw 上游 500 应报错")
	}
	if _, err := NewBangumi(client, server.URL, "").Raw(t.Context(), "subjects/253", nil); err == nil {
		t.Fatal("Bangumi.Raw 上游 500 应报错")
	}
	if _, err := NewDouban(client, server.URL, "key", "").Raw(t.Context(), "/movie/550", nil); err == nil {
		t.Fatal("Douban.Raw 上游 500 应报错")
	}
	if _, err := NewAniList(client, server.URL).Raw(t.Context(), "query", nil); err == nil {
		t.Fatal("AniList.Raw 上游 500 应报错")
	}

	// data 为 null 时退化为空对象而不是 nil。
	data, err := NewAniList(client, server.URL+"/graphql-empty").Raw(t.Context(), "query", nil)
	if err != nil {
		t.Fatalf("AniList.Raw(空 data) = %v", err)
	}
	if data == nil || len(data) != 0 {
		t.Fatalf("空 data 应归一化为空对象，实际 %#v", data)
	}

	// 列表接口的上游失败同样要冒泡。
	discover := NewDiscover(
		NewTMDB(client, server.URL, "token"),
		NewDouban(client, server.URL, "key", ""),
		NewBangumi(client, server.URL, ""),
		NewAniList(client, server.URL),
	)
	if _, err := discover.BangumiCalendar(t.Context(), 1, 10); err == nil {
		t.Fatal("BangumiCalendar 上游 500 应报错")
	}
	if _, err := discover.TMDBTrending(t.Context(), 1); err == nil {
		t.Fatal("TMDBTrending 上游 500 应报错")
	}
}

func TestDiscoverDefaultsToTimeNow(t *testing.T) {
	discover := NewDiscover(nil, nil, nil, nil)
	if discover.now == nil || discover.now().IsZero() {
		t.Fatal("NewDiscover 必须注入时钟")
	}
}

func TestDiscoverNilReceiverAvailabilityChain(t *testing.T) {
	var discover *Discover
	if _, err := discover.TMDBTrending(context.Background(), 1); err == nil {
		t.Fatal("nil 门面调用应返回错误而不是 panic")
	}
	if _, err := discover.BangumiCalendar(context.Background(), 1, 1); err == nil {
		t.Fatal("nil 门面 Bangumi 调用应返回错误")
	}
}

// TestDiscoverDoubanRecommendDefaultsSort 锁住探索页豆瓣列表的 sort 默认值。
//
// V2 的 `/recommend/douban_movies`、`/recommend/douban_tvs` 默认 sort="R"，
// 探索页本身不传 sort。少了这个默认值豆瓣会返回片单（豆列）条目——实测首条是
// 《10分钟就让你【欲罢不能】的高分电影》，标题/年份/海报全对不上。
func TestDiscoverDoubanRecommendDefaultsSort(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RequestURI())
		_, _ = io.WriteString(w, `{"items":[{"id":"1291546","type":"movie","title":"霸王别姬","year":"1993"}]}`)
	}))
	defer server.Close()

	client := newListTestClient(t)
	discover := NewDiscover(nil, NewDouban(client, server.URL, "key", "secret"), nil, nil)
	if _, err := discover.DoubanRecommend(t.Context(), "movie", "", "", 1, 10); err != nil {
		t.Fatalf("空 sort 请求失败: %v", err)
	}
	if _, err := discover.DoubanRecommend(t.Context(), "tv", "T", "", 1, 10); err != nil {
		t.Fatalf("显式 sort 请求失败: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("上游请求数 = %d (%v)", len(queries), queries)
	}
	first, err := url.ParseQuery(strings.SplitN(queries[0], "?", 2)[1])
	if err != nil {
		t.Fatalf("解析首次查询失败: %v", err)
	}
	if first.Get("sort") != "R" {
		t.Fatalf("空 sort 应补默认 R，实际请求 %q", queries[0])
	}
	if !strings.HasPrefix(queries[1], "/tv/recommend") {
		t.Fatalf("剧集应走 /tv/recommend，实际 %q", queries[1])
	}
	if !strings.Contains(queries[1], "sort=T") {
		t.Fatalf("显式 sort 应透传，实际 %q", queries[1])
	}
}
