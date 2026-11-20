package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// tmdbGroupsFake 提供剧集组（Raw）端点。
func tmdbGroupsFake(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/tv/123/episode_groups":
			_, _ = w.Write([]byte(`{"results":[
				{"id":"g1","name":"组一","episode_count":5},
				{"id":"g2","name":"组二","episode_count":7}
			]}`))
		case "/3/tv/episode_group/g1":
			_, _ = w.Write([]byte(`{"id":"g1","name":"组一","groups":[{"order":1,"episodes":[]}]}`))
		case "/3/tv/321/episode_groups":
			// 结果字段类型不对：按空数组降级，不返回 5xx。
			_, _ = w.Write([]byte(`{"results":"不是数组"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func tmdbCoordinatorDeps(t *testing.T, baseURL string) Dependencies {
	t.Helper()
	client, err := metadata.NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建元数据 HTTP 客户端失败: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	deps, _ := metadataDeps(t, metadata.NewTMDB(client, baseURL, "test-token"))
	return deps
}

func TestMediaGroupsFromTMDBRaw(t *testing.T) {
	server := tmdbGroupsFake(t)
	deps := tmdbCoordinatorDeps(t, server.URL)
	router := chi.NewRouter()
	router.Get("/media/groups/{tmdbid}", deps.mediaGroups)
	router.Get("/media/group/seasons/{episode_group}", deps.mediaGroupSeasons)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/groups/123", nil))
	var groups []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &groups); err != nil {
		t.Fatalf("解析剧集组: %v; body=%s", err, recorder.Body.String())
	}
	if len(groups) != 2 || groups[0]["id"] != "g1" || groups[1]["episode_count"] != float64(7) {
		t.Fatalf("剧集组 = %#v", groups)
	}

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/group/seasons/g1", nil))
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析剧集组详情: %v", err)
	}
	if detail["id"] != "g1" || detail["name"] != "组一" {
		t.Fatalf("剧集组详情 = %#v", detail)
	}

	// 上游失败 → [] / {}
	for _, target := range []string{"/media/groups/999", "/media/group/seasons/unknown"} {
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		body := strings.TrimSpace(recorder.Body.String())
		if body != "[]" && body != "{}" {
			t.Fatalf("%s = %s", target, body)
		}
	}

	// results 不是数组时同样降级为空数组。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/media/groups/321", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("results 类型不符 = %s", recorder.Body.String())
	}

	// 没有元数据源：[] / {}
	emptyRouter := chi.NewRouter()
	emptyRouter.Get("/media/groups/{tmdbid}", Dependencies{}.mediaGroups)
	emptyRouter.Get("/media/group/seasons/{episode_group}", Dependencies{}.mediaGroupSeasons)
	for target, want := range map[string]string{
		"/media/groups/123":                  "[]",
		"/media/group/seasons/episode_group": "{}",
	} {
		recorder = httptest.NewRecorder()
		emptyRouter.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if strings.TrimSpace(recorder.Body.String()) != want {
			t.Fatalf("%s = %s, want %s", target, recorder.Body.String(), want)
		}
	}
}

func TestTMDBSeasonEndpointsProjectSeasonInfo(t *testing.T) {
	provider := &mediaExtraFakeProvider{
		name: "themoviedb",
		details: map[string]metadata.Media{
			"tv:123": {
				Source: "themoviedb", ID: "123", Type: "tv", Title: "某剧", PosterURL: "https://img/tv.jpg",
				SeasonInfo: []metadata.SeasonInfo{
					{SeasonNumber: 1, EpisodeCount: 12, AirDate: "2020-01-01"},
					{SeasonNumber: 2, EpisodeCount: 10},
				},
			},
		},
	}
	deps := mediaExtraDeps(t, provider)
	router := chi.NewRouter()
	router.Get("/tmdb/seasons/{tmdbid}", deps.tmdbSeasons)
	router.Get("/tmdb/{tmdbid}/{season}", deps.tmdbSeasonDetail)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/tmdb/seasons/123", nil))
	var seasons []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &seasons); err != nil {
		t.Fatalf("解析季列表: %v; body=%s", err, recorder.Body.String())
	}
	if len(seasons) != 2 || seasons[0]["name"] != "第 1 季" || seasons[0]["air_date"] != "2020-01-01" ||
		seasons[0]["poster_path"] != "https://img/tv.jpg" {
		t.Fatalf("季列表 = %#v", seasons)
	}

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/tmdb/123/2", nil))
	var season map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &season); err != nil {
		t.Fatalf("解析季详情: %v", err)
	}
	if season["season_number"] != float64(2) || season["episode_count"] != float64(10) {
		t.Fatalf("季详情 = %#v", season)
	}

	// 不存在的季 → {}
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/tmdb/123/9", nil))
	if strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("未知季 = %s", recorder.Body.String())
	}

	// 未知剧集 ID 与空 ID 都降级为空列表。
	for _, target := range []string{"/tmdb/seasons/999", "/tmdb/seasons/%20"} {
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if strings.TrimSpace(recorder.Body.String()) != "[]" {
			t.Fatalf("%s = %s", target, recorder.Body.String())
		}
	}

	// 没有元数据源 → [] / {}
	emptyRouter := chi.NewRouter()
	emptyRouter.Get("/tmdb/seasons/{tmdbid}", Dependencies{}.tmdbSeasons)
	emptyRouter.Get("/tmdb/{tmdbid}/{season}", Dependencies{}.tmdbSeasonDetail)
	recorder = httptest.NewRecorder()
	emptyRouter.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/tmdb/seasons/123", nil))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无元数据季列表 = %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	emptyRouter.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/tmdb/123/1", nil))
	if strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("无元数据季详情 = %s", recorder.Body.String())
	}
}

func TestTMDBCompatibilityCacheEndpoints(t *testing.T) {
	recorder := httptest.NewRecorder()
	tmdbCache(recorder, httptest.NewRequest(http.MethodGet, "/tmdb/cache", nil))
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Count int   `json:"count"`
			Data  []any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析缓存响应: %v; body=%s", err, recorder.Body.String())
	}
	if !body.Success || body.Data.Count != 0 || body.Data.Data == nil || len(body.Data.Data) != 0 {
		t.Fatalf("缓存列表 = %s", recorder.Body.String())
	}

	for _, handler := range []http.HandlerFunc{tmdbCacheClear, tmdbCacheDelete} {
		recorder = httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodDelete, "/tmdb/cache", nil))
		decoded := decodeResponseBody(t, recorder.Body.Bytes())
		if !decoded.Success {
			t.Fatalf("缓存清理 = %s", recorder.Body.String())
		}
	}
}
