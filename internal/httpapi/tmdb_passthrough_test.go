package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func tmdbDeps(t *testing.T, baseURL string) (Dependencies, *bytes.Buffer) {
	t.Helper()
	client, err := metadata.NewHTTPClient(4, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建元数据 HTTP 客户端失败: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return metadataDeps(t, metadata.NewTMDB(client, baseURL, "tmdb-token"))
}

func TestTMDBPassthroughEndpointPaths(t *testing.T) {
	cases := []struct {
		name    string
		handler func(Dependencies, http.ResponseWriter, *http.Request)
		params  map[string]string
		path    string
		query   map[string]string
	}{
		{
			name:    "合集",
			handler: func(d Dependencies, w http.ResponseWriter, r *http.Request) { d.tmdbCollection(w, r) },
			params:  map[string]string{"collection_id": "42"},
			path:    "/3/collection/42",
		},
		{
			name:    "人物",
			handler: func(d Dependencies, w http.ResponseWriter, r *http.Request) { d.tmdbPerson(w, r) },
			params:  map[string]string{"person_id": "5"},
			path:    "/3/person/5",
		},
		{
			name:    "人物作品",
			handler: func(d Dependencies, w http.ResponseWriter, r *http.Request) { d.tmdbPersonCredits(w, r) },
			params:  map[string]string{"person_id": "5"},
			path:    "/3/person/5/combined_credits",
		},
		{
			name:    "推荐",
			handler: func(d Dependencies, w http.ResponseWriter, r *http.Request) { d.tmdbRecommend(w, r) },
			params:  map[string]string{"tmdbid": "3", "type_name": string(media.TypeTV)},
			path:    "/3/tv/3/recommendations",
		},
		{
			name:    "相似",
			handler: func(d Dependencies, w http.ResponseWriter, r *http.Request) { d.tmdbSimilar(w, r) },
			params:  map[string]string{"tmdbid": "3", "type_name": "电影"},
			path:    "/3/movie/3/similar",
			// 相似接口必须带 page=1，否则 TMDB 会返回默认页但与 V2 语义不一致。
			query: map[string]string{"page": "1"},
		},
	}

	for _, testCase := range cases {
		var gotPath string
		var gotQuery string
		var gotAuth string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "origin": "tmdb"})
		}))

		deps, _ := tmdbDeps(t, server.URL)
		recorder := httptest.NewRecorder()
		testCase.handler(deps, recorder, chiRequest(http.MethodGet, "", testCase.params, ""))
		server.Close()

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: 状态码 = %d", testCase.name, recorder.Code)
		}
		// 原始 JSON 必须原样透传，不能被包装或裁剪。
		if !strings.Contains(recorder.Body.String(), `"origin":"tmdb"`) {
			t.Fatalf("%s: 响应体 = %s", testCase.name, recorder.Body.String())
		}
		if gotPath != testCase.path {
			t.Fatalf("%s: 上游路径 = %q，期望 %q", testCase.name, gotPath, testCase.path)
		}
		for key, want := range testCase.query {
			if !strings.Contains(gotQuery, key+"="+want) {
				t.Fatalf("%s: 查询参数 = %q，缺少 %s=%s", testCase.name, gotQuery, key, want)
			}
		}
		// 凭据与语言必须由客户端补上，不能指望调用方传。
		// v4 Read Access Token 走 Authorization 头，绝不能出现在 URL 里（会进日志）。
		if gotAuth != "Bearer tmdb-token" {
			t.Fatalf("%s: Authorization = %q", testCase.name, gotAuth)
		}
		for _, needle := range []string{"language=zh-CN"} {
			if !strings.Contains(gotQuery, needle) {
				t.Fatalf("%s: 查询参数 = %q，缺少 %s", testCase.name, gotQuery, needle)
			}
		}
		if strings.Contains(gotQuery, "tmdb-token") {
			t.Fatalf("%s: 凭据不应出现在查询串: %q", testCase.name, gotQuery)
		}
	}
}

func TestTMDBPassthroughProviderGuards(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{}.tmdbPerson(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": "5"}, ""))
	if !strings.Contains(recorder.Body.String(), "未配置元数据源") {
		t.Fatalf("缺少元数据源时响应 = %s", recorder.Body.String())
	}

	// 同名供应商但不是 TMDB 实现：必须明确拒绝，不能退化成别的数据源。
	deps, _ := metadataDeps(t, recognizeProvider{})
	recorder = httptest.NewRecorder()
	deps.tmdbPerson(recorder, chiRequest(http.MethodGet, "", map[string]string{"person_id": "5"}, ""))
	if !strings.Contains(recorder.Body.String(), "未配置 TMDB 数据源") {
		t.Fatalf("非 TMDB 供应商响应 = %s", recorder.Body.String())
	}
}

func TestTMDBPassthroughUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	deps, logs := tmdbDeps(t, server.URL)

	recorder := httptest.NewRecorder()
	deps.tmdbCollection(recorder, chiRequest(http.MethodGet, "", map[string]string{"collection_id": "8"}, ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("上游失败仍应返回 200 以符合 V2 前端约定，实际 %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "TMDB 请求失败") {
		t.Fatalf("响应 = %s", body)
	}
	if strings.Contains(body, "tmdb-token") {
		t.Fatal("响应绝不能泄露 TMDB 凭据")
	}
	// 日志里要能定位是哪个路径失败，但不含凭据。
	logOutput := logs.String()
	if !strings.Contains(logOutput, "collection/8") {
		t.Fatalf("日志缺少请求路径: %s", logOutput)
	}
	if strings.Contains(logOutput, "tmdb-token") {
		t.Fatalf("日志泄露凭据: %s", logOutput)
	}
}

func TestTMDBMediaType(t *testing.T) {
	cases := map[string]string{
		string(media.TypeTV):    "tv",
		string(media.TypeMovie): "movie",
		"":                      "movie",
		"unknown":               "movie",
	}
	for typeName, want := range cases {
		if got := tmdbMediaType(typeName); got != want {
			t.Fatalf("tmdbMediaType(%q) = %q，期望 %q", typeName, got, want)
		}
	}
}

// TestTMDBCreditsReturnsV2MediaPersonList 锁住 tmdb/credits 的 V2 响应契约。
//
// 2026-11-20 反馈「TMDB 的演员卡片不显示」：根因是这个接口把 TMDB 的
// {id, cast, crew} 原样透传，而前端 PersonCardListView 是把响应当数组 concat 的，
// 对象进不去列表，整张演员卡片就空白了。
func TestTMDBCreditsReturnsV2MediaPersonList(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{
			"id": 603,
			"cast": [
				{"id": 6384, "name": "基努·里维斯", "character": "Neo", "profile_path": "/k.jpg"},
				{"id": 3894, "name": "劳伦斯·菲什伯恩", "character": "Morpheus", "profile_path": "/l.jpg"}
			],
			"crew": [{"id": 9330, "name": "沃卓斯基姐妹", "job": "Director"}]
		}`))
	}))
	defer server.Close()

	deps, _ := tmdbDeps(t, server.URL)
	recorder := httptest.NewRecorder()
	deps.tmdbCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"tmdbid": "603", "type_name": string(media.TypeMovie),
	}, ""))

	if gotPath != "/3/movie/603/credits" {
		t.Fatalf("上游路径 = %q，期望 /3/movie/603/credits", gotPath)
	}
	if !strings.Contains(gotQuery, "language=zh-CN") {
		t.Fatalf("缺少语言参数: %q", gotQuery)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "Director") || strings.Contains(body, `"cast"`) {
		t.Fatalf("响应不应透传 TMDB 原始结构: %s", body)
	}
	// V2 的演职员接口返回裸数组：前端 PersonCardListView 直接 concat 响应。
	var parsed []struct {
		Source      string `json:"source"`
		ID          string `json:"id"`
		Name        string `json:"name"`
		Character   string `json:"character"`
		ProfilePath string `json:"profile_path"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("响应不是 V2 列表: %v，原文 %s", err, body)
	}
	if len(parsed) != 2 {
		t.Fatalf("演职员列表 = %+v", parsed)
	}
	first := parsed[0]
	if first.Source != "themoviedb" || first.Name != "基努·里维斯" ||
		first.Character != "Neo" || first.ID != "6384" {
		t.Fatalf("首条演职员 = %+v", first)
	}
	// 相对路径：前端用 TMDB_IMAGE_DOMAIN 拼完整 URL，服务端不能擅自改写。
	if first.ProfilePath != "/k.jpg" {
		t.Fatalf("profile_path 应为相对路径，实际 %q", first.ProfilePath)
	}
}

// TestTMDBCreditsPaginationAndGuards 覆盖分页、未知类型、非法 ID 与未配置数据源四种降级。
func TestTMDBCreditsPaginationAndGuards(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"cast": [{"id": 1, "name": "甲", "character": "", "profile_path": ""}]}`))
	}))
	defer server.Close()
	deps, _ := tmdbDeps(t, server.URL)

	// 第 2 页超出总量：返回空数组而不是 null。
	recorder := httptest.NewRecorder()
	deps.tmdbCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"tmdbid": "603", "type_name": string(media.TypeMovie),
	}, "page=2"))
	if !strings.Contains(recorder.Body.String(), `[]`) {
		t.Fatalf("第 2 页响应 = %s", recorder.Body.String())
	}

	// 未知类型直接空表，不发上游请求。
	recorder = httptest.NewRecorder()
	deps.tmdbCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"tmdbid": "603", "type_name": "未知",
	}, ""))
	if !strings.Contains(recorder.Body.String(), `[]`) {
		t.Fatalf("未知类型响应 = %s", recorder.Body.String())
	}

	// 未配置 TMDB（没有密钥）时同样空表：页面显示「暂无数据」，不是网络错误。
	recorder = httptest.NewRecorder()
	Dependencies{}.tmdbCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"tmdbid": "603", "type_name": string(media.TypeMovie),
	}, ""))
	if !strings.Contains(recorder.Body.String(), `[]`) {
		t.Fatalf("未配置数据源响应 = %s", recorder.Body.String())
	}

	// 非法 ID 不 panic，同样空表。
	recorder = httptest.NewRecorder()
	deps.tmdbCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"tmdbid": "abc", "type_name": string(media.TypeMovie),
	}, ""))
	if !strings.Contains(recorder.Body.String(), `[]`) {
		t.Fatalf("非法 ID 响应 = %s", recorder.Body.String())
	}
}
