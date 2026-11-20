package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// recommendFakeTMDB 提供趋势与发现两个端点，未匹配路径回 404。
func recommendFakeTMDB(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/trending/all/day":
			_, _ = w.Write([]byte(`{"results":[
				{"id":1,"media_type":"movie","title":"趋势电影","original_title":"Trend Movie","release_date":"2024-05-01","vote_average":7.5,"poster_path":"/p.jpg"},
				{"id":2,"media_type":"tv","name":"趋势剧","original_name":"Trend TV","first_air_date":"2023-01-01"},
				{"id":0,"media_type":"movie"},
				{"id":3,"media_type":"person","name":"人物"}
			]}`))
		case "/3/discover/movie":
			_, _ = w.Write([]byte(`{"results":[
				{"id":11,"title":"发现电影","release_date":"2022-02-02","vote_average":8.1}
			]}`))
		case "/3/discover/tv":
			_, _ = w.Write([]byte(`{"results":[
				{"id":12,"name":"发现剧","first_air_date":"2021-03-03"}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func recommendTestDeps(t *testing.T, server *httptest.Server) (Dependencies, *bytes.Buffer) {
	t.Helper()
	client, err := metadata.NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建元数据 HTTP 客户端失败: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	discover := metadata.NewDiscover(metadata.NewTMDB(client, server.URL, "test-token"), nil, nil, nil)
	return Dependencies{Discover: discover, Logger: logger}, &logs
}

func decodeMediaArray(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("解析媒体数组: %v; body=%s", err, body)
	}
	return items
}

func TestRecommendTMDBEndpointsMapV2Shapes(t *testing.T) {
	server := recommendFakeTMDB(t)
	deps, _ := recommendTestDeps(t, server)

	if !deps.tmdbReady() {
		t.Fatal("配置了 Token 的 TMDB 应视为可用")
	}

	recorder := httptest.NewRecorder()
	deps.recommendTMDBTrending(recorder, httptest.NewRequest(http.MethodGet, "/recommend/tmdb_trending", nil))
	items := decodeMediaArray(t, recorder.Body.Bytes())
	// 无效 ID 与 person 类型在数据源层被过滤。
	if len(items) != 2 {
		t.Fatalf("趋势条数 = %d, body=%s", len(items), recorder.Body.String())
	}
	if items[0]["title"] != "趋势电影" || items[0]["type"] != "电影" || items[0]["year"] != "2024" {
		t.Fatalf("趋势电影 = %#v", items[0])
	}
	if items[1]["title"] != "趋势剧" || items[1]["type"] != "电视剧" {
		t.Fatalf("趋势剧 = %#v", items[1])
	}

	movieDiscover := deps.tmdbDiscover("movie")
	recorder = httptest.NewRecorder()
	movieDiscover(recorder, httptest.NewRequest(http.MethodGet, "/discover/tmdb_movies?page=2&count=10", nil))
	items = decodeMediaArray(t, recorder.Body.Bytes())
	if len(items) != 1 || items[0]["title"] != "发现电影" {
		t.Fatalf("发现电影 = %#v", items)
	}

	tvDiscover := deps.tmdbDiscover("tv")
	recorder = httptest.NewRecorder()
	tvDiscover(recorder, httptest.NewRequest(http.MethodGet, "/discover/tmdb_tvs", nil))
	items = decodeMediaArray(t, recorder.Body.Bytes())
	if len(items) != 1 || items[0]["title"] != "发现剧" || items[0]["type"] != "电视剧" {
		t.Fatalf("发现剧 = %#v", items)
	}
}

func TestRecommendEndpointsDegradeWithoutSources(t *testing.T) {
	// Discover 存在但没有任何数据源：每个端点都要降级为空数组并记录一条 warn。
	deps, logs := recommendTestDeps(t, recommendFakeTMDB(t))
	deps.Discover = metadata.NewDiscover(nil, nil, nil, nil)
	if deps.tmdbReady() {
		t.Fatal("没有 TMDB 时不应视为可用")
	}

	handlers := []struct {
		name    string
		handler http.HandlerFunc
		op      string
	}{
		{name: "Bangumi 放送", handler: deps.recommendBangumiCalendar, op: "bangumi_calendar"},
		{name: "Bangumi 探索", handler: deps.discoverBangumi, op: "bangumi_discover"},
		{name: "豆瓣合集", handler: deps.doubanCollection("movie_hot"), op: "douban_movie_hot"},
		{name: "豆瓣探索", handler: deps.doubanDiscover("movie"), op: "douban_discover"},
		{name: "AniList 趋势", handler: deps.anilistTrending, op: "anilist_trending"},
		{name: "AniList 探索", handler: deps.anilistDiscover, op: "anilist_discover"},
		{name: "AniList 本季", handler: deps.anilistPopularThisSeason, op: "anilist_season"},
		{name: "TMDB 趋势", handler: deps.recommendTMDBTrending, op: ""},
		{name: "TMDB 发现", handler: deps.tmdbDiscover("movie"), op: ""},
	}
	for _, test := range handlers {
		recorder := httptest.NewRecorder()
		test.handler(recorder, httptest.NewRequest(http.MethodGet, "/stub", nil))
		if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "[]" {
			t.Fatalf("%s = (%d, %s)", test.name, recorder.Code, recorder.Body.String())
		}
	}
	for _, operation := range []string{"bangumi_calendar", "bangumi_discover", "douban_movie_hot", "douban_discover", "anilist_trending", "anilist_discover", "anilist_season"} {
		if !strings.Contains(logs.String(), operation) {
			t.Fatalf("日志缺少 operation=%s: %s", operation, logs.String())
		}
	}

	// Discover 完全没配置时同样返回空数组，且不记录 warn。
	empty := Dependencies{Logger: deps.Logger}
	logs.Reset()
	for _, handler := range []http.HandlerFunc{
		empty.recommendBangumiCalendar, empty.discoverBangumi, empty.doubanCollection("movie_hot"),
		empty.doubanDiscover("movie"), empty.anilistTrending, empty.anilistDiscover, empty.anilistPopularThisSeason,
		empty.recommendTMDBTrending, empty.tmdbDiscover("movie"),
	} {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodGet, "/stub", nil))
		if strings.TrimSpace(recorder.Body.String()) != "[]" {
			t.Fatalf("无 Discover body = %s", recorder.Body.String())
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("无 Discover 时不应记录 warn: %s", logs.String())
	}
}

func TestRecommendHelpers(t *testing.T) {
	// mediaPage：非法/缺省回落到默认值，count 上限 100。
	tests := []struct {
		query        string
		defaultCount int
		wantPage     int
		wantCount    int
	}{
		{"", 20, 1, 20},
		{"?page=0&count=0", 20, 1, 20},
		{"?page=-3&count=-1", 30, 1, 30},
		{"?page=4&count=500", 20, 4, 20},
		{"?page=abc&count=xyz", 8, 1, 8},
		{"?page=2&count=50", 20, 2, 50},
	}
	for _, test := range tests {
		page, count := mediaPage(httptest.NewRequest(http.MethodGet, "/x"+test.query, nil), test.defaultCount)
		if page != test.wantPage || count != test.wantCount {
			t.Fatalf("mediaPage(%q, %d) = (%d, %d), want (%d, %d)",
				test.query, test.defaultCount, page, count, test.wantPage, test.wantCount)
		}
	}

	// writeMediaList：空列表是 []，非空列表逐项映射。
	recorder := httptest.NewRecorder()
	writeMediaList(recorder, nil)
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("空列表 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	writeMediaList(recorder, []metadata.Media{{Source: "themoviedb", ID: "9", Type: "tv", Title: "某剧", Year: 2020}})
	items := decodeMediaArray(t, recorder.Body.Bytes())
	if len(items) != 1 || items[0]["title"] != "某剧" || items[0]["type"] != "电视剧" {
		t.Fatalf("映射列表 = %#v", items)
	}

	// emptySourceList：无 Logger 与有 Logger 都要返回 []。
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, deps := range []Dependencies{{}, {Logger: logger}} {
		recorder := httptest.NewRecorder()
		deps.emptySourceList(recorder, httptest.NewRequest(http.MethodGet, "/recommend/source", nil))
		if strings.TrimSpace(recorder.Body.String()) != "[]" {
			t.Fatalf("emptySourceList = %s", recorder.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "没有插件注册额外的推荐") {
		t.Fatalf("缺少调试日志: %s", logs.String())
	}

	// logRecommendError：nil Logger 不 panic，有 Logger 时带 operation 与 error。
	Dependencies{}.logRecommendError(t.Context(), "op", errRecommendTest)
	logs.Reset()
	Dependencies{Logger: logger}.logRecommendError(t.Context(), "op", errRecommendTest)
	if !strings.Contains(logs.String(), "op") || !strings.Contains(logs.String(), errRecommendTest.Error()) {
		t.Fatalf("错误日志 = %s", logs.String())
	}
}

// errRecommendTest 用于验证错误日志字段。
var errRecommendTest = &recommendTestError{}

type recommendTestError struct{}

func (*recommendTestError) Error() string { return "数据源不可用" }

// TestAniListDiscoverReturnsMediaList 锁住探索页 AniList 一行的形状。
//
// 之前 /anilist/discover 是 GraphQL 透传，返回 {Page:{media:[...]}} 对象，
// 而探索页按数组消费，于是这一行永远空白。
func TestAniListDiscoverReturnsMediaList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"Page":{"media":[
			{"id":21,"title":{"romaji":"One Piece","english":"One Piece","native":"ONE PIECE"},
			 "seasonYear":1999,"startDate":{"year":1999},"description":"海贼王",
			 "coverImage":{"extraLarge":"https://img/xl.jpg","large":"https://img/l.jpg"},
			 "averageScore":88,"episodes":1100,"synonyms":[]}
		]}}}`))
	}))
	defer server.Close()

	client := newMetadataHTTPClient(t)
	deps := Dependencies{Discover: metadata.NewDiscover(nil, nil, nil, metadata.NewAniList(client, server.URL))}
	recorder := httptest.NewRecorder()
	deps.anilistDiscover(recorder, httptest.NewRequest(http.MethodGet, "/anilist/discover", nil))

	items := decodeMediaArray(t, recorder.Body.Bytes())
	if len(items) != 1 {
		t.Fatalf("条数 = %d, body=%s", len(items), recorder.Body.String())
	}
	if items[0]["title"] != "One Piece" {
		t.Fatalf("标题 = %#v", items[0]["title"])
	}
	switch id := items[0]["anilist_id"].(type) {
	case float64:
		if id != 21 {
			t.Fatalf("anilist_id = %v", id)
		}
	case string:
		if id != "21" {
			t.Fatalf("anilist_id = %q", id)
		}
	default:
		t.Fatalf("anilist_id 类型异常: %#v", items[0]["anilist_id"])
	}
	if poster, _ := items[0]["poster_path"].(string); poster == "" {
		t.Fatalf("缺少海报: %#v", items[0])
	}
}
