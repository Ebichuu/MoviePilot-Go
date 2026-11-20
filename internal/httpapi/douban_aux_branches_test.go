package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// silenceDoubanLogs 把默认 slog 输出丢弃，避免失败路径的 WARN 污染测试输出。
func silenceDoubanLogs(t *testing.T) {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1})))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

// doubanAuxServer 按路径后缀返回固定内容；`mode` 决定是否整体失败。
type doubanAuxServer struct {
	server *httptest.Server
	fail   bool
}

func newDoubanAuxServer(t *testing.T) *doubanAuxServer {
	t.Helper()
	aux := &doubanAuxServer{}
	aux.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if aux.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/celebrities"):
			_, _ = w.Write([]byte(`{"actors":"不是数组"}`))
		case strings.HasSuffix(r.URL.Path, "/recommendations"):
			_, _ = w.Write([]byte(`[{"id":"21","title":"推荐一","card_subtitle":"2019-05-01","pic":{"large":"u"}},{"id":""}]`))
		case strings.HasSuffix(r.URL.Path, "/celebrity/bad"):
			_, _ = w.Write([]byte(`"不是对象"`))
		case strings.HasSuffix(r.URL.Path, "/celebrity/col-ok"):
			_, _ = w.Write([]byte(`{"id":"col-ok","modules":[{"type":"work_collections","payload":{"id":"c1"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/celebrity/col-empty"):
			_, _ = w.Write([]byte(`{"id":"col-empty","modules":[{"type":"work_collections","payload":{"id":""}}]}`))
		case strings.HasSuffix(r.URL.Path, "/work_collections/c1/works"):
			_, _ = w.Write([]byte(`{"works":[{"subject":{"id":"11","title":"作品A","card_subtitle":"2021 / 中国大陆"}},{"subject":{"id":""}}]}`))
		case strings.HasSuffix(r.URL.Path, "/work_collections/cbad/works"):
			_, _ = w.Write([]byte(`"不是对象"`))
		case strings.HasSuffix(r.URL.Path, "/celebrity/col-badfetch"):
			_, _ = w.Write([]byte(`{"id":"col-badfetch","modules":[{"type":"work_collections","payload":{"id":"cbad"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(aux.server.Close)
	return aux
}

func doubanAuxDeps(t *testing.T, baseURL string) Dependencies {
	t.Helper()
	client, err := metadata.NewHTTPClient(2, 3*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建 HTTP 客户端: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	deps, _ := metadataDeps(t, metadata.NewDouban(client, baseURL, "k", "s"))
	return deps
}

// TestDoubanHandlersWithoutClient 覆盖四个端点在豆瓣数据源缺失时的提前返回。
func TestDoubanHandlersWithoutClient(t *testing.T) {
	deps := Dependencies{}
	handlers := map[string]func(http.ResponseWriter, *http.Request){
		"演职员":  deps.doubanCredits,
		"推荐":   deps.doubanRecommend,
		"人物":   deps.doubanPerson,
		"人物作品": deps.doubanPersonCredits,
	}
	for name, call := range handlers {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			call(recorder, chiRequest(http.MethodGet, "", map[string]string{
				"doubanid": "1", "person_id": "1", "type_name": "电影",
			}, ""))
			if !strings.Contains(recorder.Body.String(), "未配置豆瓣数据源") {
				t.Fatalf("响应 = %s", recorder.Body.String())
			}
		})
	}
}

// TestDoubanClientUnavailable 覆盖「未配置豆瓣数据源」的三条来源。
func TestDoubanClientUnavailable(t *testing.T) {
	cases := map[string]Dependencies{
		"没有元数据协调器": {},
		"没有豆瓣提供者":  mustMetadataDeps(t, &mediaExtraFakeProvider{name: "tmdb"}),
		"提供者类型不匹配": mustMetadataDeps(t, &mediaExtraFakeProvider{name: "douban"}),
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if _, ok := deps.doubanClient(recorder, httptest.NewRequest(http.MethodGet, "/x", nil)); ok {
				t.Fatal("应返回 false")
			}
			if !strings.Contains(recorder.Body.String(), "未配置豆瓣数据源") {
				t.Fatalf("响应 = %s", recorder.Body.String())
			}
		})
	}
}

func mustMetadataDeps(t *testing.T, providers ...metadata.Provider) Dependencies {
	t.Helper()
	deps, _ := metadataDeps(t, providers...)
	return deps
}

// TestDoubanFetchFailure 覆盖上游失败时的可读错误。
func TestDoubanFetchFailure(t *testing.T) {
	silenceDoubanLogs(t)
	aux := newDoubanAuxServer(t)
	deps := doubanAuxDeps(t, aux.server.URL)
	aux.fail = true

	recorder := httptest.NewRecorder()
	deps.doubanCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"doubanid": "1", "type_name": "电影",
	}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣请求失败，请检查豆瓣数据源配置") {
		t.Fatalf("演职员抓取失败 = %s", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	deps.doubanRecommend(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"doubanid": "1", "type_name": "电视剧",
	}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣请求失败，请检查豆瓣数据源配置") {
		t.Fatalf("推荐抓取失败 = %s", recorder.Body.String())
	}
}

// TestDoubanHandlerIDGuards 覆盖四个端点的空 ID 提示。
func TestDoubanHandlerIDGuards(t *testing.T) {
	aux := newDoubanAuxServer(t)
	deps := doubanAuxDeps(t, aux.server.URL)

	cases := []struct {
		name    string
		call    func(http.ResponseWriter, *http.Request)
		message string
	}{
		{name: "演职员", call: deps.doubanCredits, message: "豆瓣ID不能为空"},
		{name: "推荐", call: deps.doubanRecommend, message: "豆瓣ID不能为空"},
		{name: "人物", call: deps.doubanPerson, message: "豆瓣人物ID不能为空"},
		{name: "人物作品", call: deps.doubanPersonCredits, message: "豆瓣人物ID不能为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.call(recorder, chiRequest(http.MethodGet, "", map[string]string{
				"doubanid": "  ", "person_id": " ", "type_name": "电影",
			}, ""))
			if !strings.Contains(recorder.Body.String(), tc.message) {
				t.Fatalf("响应 = %s", recorder.Body.String())
			}
		})
	}
}

// TestDoubanDecodeFailures 覆盖上游返回结构不符时的兜底。
func TestDoubanDecodeFailures(t *testing.T) {
	silenceDoubanLogs(t)
	aux := newDoubanAuxServer(t)
	deps := doubanAuxDeps(t, aux.server.URL)

	// 演职员：actors 不是数组。
	recorder := httptest.NewRecorder()
	deps.doubanCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"doubanid": "1", "type_name": "电影",
	}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣返回数据无法解析") {
		t.Fatalf("演职员解析失败 = %s", recorder.Body.String())
	}

	// 人物：详情不是对象。
	recorder = httptest.NewRecorder()
	deps.doubanPerson(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"person_id": "bad",
	}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣返回数据无法解析") {
		t.Fatalf("人物解析失败 = %s", recorder.Body.String())
	}

	// 推荐：列表不是数组。
	auxWithBadRecommend := newDoubanAuxServer(t)
	auxWithBadRecommend.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`"不是数组"`))
	})
	badDeps := doubanAuxDeps(t, auxWithBadRecommend.server.URL)
	recorder = httptest.NewRecorder()
	badDeps.doubanRecommend(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"doubanid": "1", "type_name": "电影",
	}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣返回数据无法解析") {
		t.Fatalf("推荐解析失败 = %s", recorder.Body.String())
	}

	// 人物作品：作品列表不是对象 → 空数组。
	recorder = httptest.NewRecorder()
	deps.doubanPersonCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"person_id": "col-badfetch",
	}, ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("作品解析失败 = %s", recorder.Body.String())
	}
}

// TestDoubanPersonCreditsWorksFailure 覆盖人物存在但作品集查询失败的分支。
func TestDoubanPersonCreditsWorksFailure(t *testing.T) {
	silenceDoubanLogs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/works") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"id":"p","modules":[{"type":"work_collections","payload":{"id":"c9"}}]}`))
	}))
	defer server.Close()

	recorder := httptest.NewRecorder()
	doubanAuxDeps(t, server.URL).doubanPersonCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"person_id": "p",
	}, ""))
	if !strings.Contains(recorder.Body.String(), "豆瓣请求失败") {
		t.Fatalf("作品集抓取失败 = %s", recorder.Body.String())
	}
}

// TestDoubanPersonCreditsBranches 覆盖作品集缺失、正常作品与空 subject。
func TestDoubanPersonCreditsBranches(t *testing.T) {
	silenceDoubanLogs(t)
	aux := newDoubanAuxServer(t)
	deps := doubanAuxDeps(t, aux.server.URL)

	// 没有作品集模块 → 空数组。
	recorder := httptest.NewRecorder()
	deps.doubanPersonCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"person_id": "unknown",
	}, ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("无作品集 = %s", recorder.Body.String())
	}

	// 作品集 ID 为空 → 空数组。
	recorder = httptest.NewRecorder()
	deps.doubanPersonCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"person_id": "col-empty",
	}, ""))
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("空作品集 = %s", recorder.Body.String())
	}

	// 正常作品：空 subject 被跳过，年份取自 card_subtitle。
	recorder = httptest.NewRecorder()
	deps.doubanPersonCredits(recorder, chiRequest(http.MethodGet, "", map[string]string{
		"person_id": "col-ok",
	}, ""))
	body := recorder.Body.String()
	if !strings.Contains(body, "作品A") || strings.Contains(body, `"id":""`) {
		t.Fatalf("正常作品 = %s", body)
	}
	if !strings.Contains(body, `"year":2021`) || !strings.Contains(body, `"type":"movie"`) {
		t.Fatalf("作品投影 = %s", body)
	}
}

// TestDoubanWorkCollectionBranches 覆盖作品集查询的三条退化路径与成功路径。
func TestDoubanWorkCollectionBranches(t *testing.T) {
	silenceDoubanLogs(t)
	aux := newDoubanAuxServer(t)
	deps := doubanAuxDeps(t, aux.server.URL)

	unreachableClient, err := metadata.NewHTTPClient(1, 500*time.Millisecond, 1<<20)
	if err != nil {
		t.Fatalf("创建客户端: %v", err)
	}
	defer unreachableClient.CloseIdleConnections()
	unreachable := metadata.NewDouban(unreachableClient, "http://127.0.0.1:1", "k", "s")
	if id := deps.doubanWorkCollection(context.Background(), unreachable, "1"); id != "" {
		t.Fatalf("不可达客户端 = %q", id)
	}

	realClient, err := metadata.NewHTTPClient(1, 2*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建客户端: %v", err)
	}
	defer realClient.CloseIdleConnections()
	douban := metadata.NewDouban(realClient, aux.server.URL, "k", "s")
	if id := deps.doubanWorkCollection(context.Background(), douban, "bad"); id != "" {
		t.Fatalf("解析失败 = %q", id)
	}
	if id := deps.doubanWorkCollection(context.Background(), douban, "col-ok"); id != "c1" {
		t.Fatalf("作品集 ID = %q", id)
	}
}

// TestDoubanSlugAndProjection 覆盖 type_name 映射与推荐条目投影。
func TestDoubanSlugAndProjection(t *testing.T) {
	if doubanSlug("电影") != "movie" || doubanSlug("电视剧") != "tv" || doubanSlug("") != "tv" {
		t.Fatal("type_name 映射错误")
	}
	person := doubanCreditEntry{ID: "a1", Title: "导演甲", LatinName: "Dir A", Character: "角色",
		Avatar: doubanAvatar{Large: "u"}}.person()
	if person.Name != "导演甲" || person.Avatar != "u" || len(person.AlsoKnownAs) != 1 {
		t.Fatalf("演职员投影 = %+v", person)
	}
	if plain := (doubanCreditEntry{ID: "a2", Name: "演员乙"}).person(); len(plain.AlsoKnownAs) != 0 {
		t.Fatalf("别名应为空: %+v", plain)
	}

	media := doubanRecommendMedia(doubanRecommendEntry{
		ID: "21", Title: "推荐一", CardSubtitle: "2019-05-01",
		Pic: struct {
			Large string `json:"large"`
		}{Large: "u"}, Rating: struct {
			Value float64 `json:"value"`
		}{Value: 7.5},
	}, "tv")
	if media.Type != "tv" || media.Year != 2019 || media.VoteAverage != 7.5 {
		t.Fatalf("推荐投影 = %+v", media)
	}
	if short := doubanRecommendMedia(doubanRecommendEntry{CardSubtitle: "20"}, "movie"); short.Year != 0 {
		t.Fatalf("短副标题应得 0 年: %+v", short)
	}
}
