package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func metadataDeps(t *testing.T, providers ...metadata.Provider) (Dependencies, *bytes.Buffer) {
	t.Helper()
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	coordinator, err := metadata.NewCoordinator(providers, metadata.Options{
		CacheTTL:       time.Minute,
		RequestTimeout: 5 * time.Second,
		MaxResults:     10,
	})
	if err != nil {
		t.Fatalf("创建协调器失败: %v", err)
	}
	return Dependencies{
		Config:   config.Config{Environment: "test", MaxRequestBody: 1 << 20},
		Metadata: coordinator,
		Logger:   logger,
	}, &buffer
}

func newMetadataHTTPClient(t *testing.T) *metadata.HTTPClient {
	t.Helper()
	client, err := metadata.NewHTTPClient(2, 5*time.Second, 1<<20)
	if err != nil {
		t.Fatalf("创建元数据客户端失败: %v", err)
	}
	return client
}

func TestMetaAuxWithoutMetadataSource(t *testing.T) {
	deps := Dependencies{Config: config.Config{Environment: "test", MaxRequestBody: 1 << 20}}

	recorder := httptest.NewRecorder()
	deps.bangumiSubject(recorder, chiRequest("GET", "", map[string]string{"bangumiid": "7"}, ""))
	envelope := decodeBody[Response](t, recorder)
	if envelope.Success || !strings.Contains(envelope.Message, "未配置元数据源") {
		t.Fatalf("Bangumi 信封 = %+v", envelope)
	}

	recorder = httptest.NewRecorder()
	deps.anilistSubject(recorder, chiRequest("GET", "", map[string]string{"anilist_id": "7"}, ""))
	envelope = decodeBody[Response](t, recorder)
	if envelope.Success || !strings.Contains(envelope.Message, "未配置元数据源") {
		t.Fatalf("AniList 信封 = %+v", envelope)
	}
}

func TestMetaAuxWithoutMatchingProvider(t *testing.T) {
	// 只注册 Bangumi 时请求 AniList 必须给出专属提示，而不是笼统的网络失败。
	anilistServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer anilistServer.Close()
	deps, _ := metadataDeps(t, metadata.NewAniList(newMetadataHTTPClient(t), anilistServer.URL))

	recorder := httptest.NewRecorder()
	deps.bangumiSubject(recorder, chiRequest("GET", "", map[string]string{"bangumiid": "7"}, ""))
	envelope := decodeBody[Response](t, recorder)
	if envelope.Success || !strings.Contains(envelope.Message, "未配置 Bangumi 数据源") {
		t.Fatalf("信封 = %+v", envelope)
	}

	onlyBangumi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer onlyBangumi.Close()
	deps, _ = metadataDeps(t, metadata.NewBangumi(newMetadataHTTPClient(t), onlyBangumi.URL, ""))
	recorder = httptest.NewRecorder()
	deps.anilistSubject(recorder, chiRequest("GET", "", map[string]string{"anilist_id": "7"}, ""))
	envelope = decodeBody[Response](t, recorder)
	if envelope.Success || !strings.Contains(envelope.Message, "未配置 AniList 数据源") {
		t.Fatalf("信封 = %+v", envelope)
	}
}

func TestBangumiPassthroughSuccess(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"id":7,"name":"テスト"}`))
	}))
	defer server.Close()

	deps, buffer := metadataDeps(t, metadata.NewBangumi(newMetadataHTTPClient(t), server.URL, ""))
	params := map[string]string{"bangumiid": "7"}

	recorder := httptest.NewRecorder()
	deps.bangumiSubject(recorder, chiRequest("GET", "", params, ""))
	if body := recorder.Body.String(); !strings.Contains(body, `"name":"テスト"`) {
		t.Fatalf("Bangumi 主体应原样透传: %s", body)
	}

	recorder = httptest.NewRecorder()
	deps.bangumiCredits(recorder, chiRequest("GET", "", params, ""))
	recorder = httptest.NewRecorder()
	deps.bangumiRecommend(recorder, chiRequest("GET", "", params, ""))

	recorder = httptest.NewRecorder()
	deps.bangumiPerson(recorder, chiRequest("GET", "", map[string]string{"person_id": "9"}, ""))
	recorder = httptest.NewRecorder()
	deps.bangumiPersonCredits(recorder, chiRequest("GET", "", map[string]string{"person_id": "9"}, ""))

	want := []string{"/v0/subjects/7", "/v0/subjects/7/persons", "/v0/subjects/7/subjects", "/v0/persons/9", "/v0/persons/9/subjects"}
	if len(paths) != len(want) {
		t.Fatalf("请求路径 = %v", paths)
	}
	for index, path := range want {
		if paths[index] != path {
			t.Fatalf("第 %d 条路径 = %q，期望 %q", index, paths[index], path)
		}
	}
	if strings.Contains(buffer.String(), "Bangumi 请求失败") {
		t.Fatalf("成功路径不应记录告警: %s", buffer.String())
	}
}

func TestBangumiPassthroughFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	deps, buffer := metadataDeps(t, metadata.NewBangumi(newMetadataHTTPClient(t), server.URL, ""))
	recorder := httptest.NewRecorder()
	deps.bangumiSubject(recorder, chiRequest("GET", "", map[string]string{"bangumiid": "7"}, ""))

	// 上游失败对前端是 200 + success:false（不是 500），但要落一条可诊断的告警。
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", recorder.Code)
	}
	envelope := decodeBody[Response](t, recorder)
	if envelope.Success || !strings.Contains(envelope.Message, "请检查网络") {
		t.Fatalf("信封 = %+v", envelope)
	}
	logged := buffer.String()
	if !strings.Contains(logged, "Bangumi 请求失败") || !strings.Contains(logged, "v0/subjects/7") {
		t.Fatalf("告警日志应带失败路径: %s", logged)
	}
}

func TestAniListPassthroughSuccess(t *testing.T) {
	var lastBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(payload, &lastBody)
		_, _ = w.Write([]byte(`{"data":{"Media":{"id":7,"title":{"romaji":"Test"}}}}`))
	}))
	defer server.Close()

	deps, _ := metadataDeps(t, metadata.NewAniList(newMetadataHTTPClient(t), server.URL))

	recorder := httptest.NewRecorder()
	deps.anilistSubject(recorder, chiRequest("GET", "", map[string]string{"anilist_id": "7"}, ""))
	if body := recorder.Body.String(); !strings.Contains(body, `"Media"`) {
		t.Fatalf("AniList 应原样透传 data: %s", body)
	}
	variables, ok := lastBody["variables"].(map[string]any)
	if !ok {
		t.Fatalf("请求体缺少 variables: %#v", lastBody)
	}
	if id, ok := variables["id"].(float64); !ok || id != 7 {
		t.Fatalf("variables.id = %#v", variables["id"])
	}
	if query, _ := lastBody["query"].(string); !strings.Contains(query, "Media(id:$id)") {
		t.Fatalf("查询语句不对: %q", query)
	}
}

func TestAniListPassthroughFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	deps, buffer := metadataDeps(t, metadata.NewAniList(newMetadataHTTPClient(t), server.URL))
	recorder := httptest.NewRecorder()
	deps.anilistPerson(recorder, chiRequest("GET", "", map[string]string{"person_id": "9"}, ""))

	envelope := decodeBody[Response](t, recorder)
	if envelope.Success || !strings.Contains(envelope.Message, "请检查网络") {
		t.Fatalf("信封 = %+v", envelope)
	}
	if !strings.Contains(buffer.String(), "AniList 请求失败") {
		t.Fatalf("应记录 AniList 告警: %s", buffer.String())
	}
}

func TestAniListIDParamClamping(t *testing.T) {
	cases := map[string]int{
		"7":          7,
		"abc":        0,
		"-5":         0,
		"":           0,
		"2000000001": 2_000_000_000,
	}
	for raw, want := range cases {
		request := chiRequest("GET", "", map[string]string{"anilist_id": raw}, "")
		if got := anilistIDParam(request, "anilist_id"); got != want {
			t.Fatalf("anilistIDParam(%q) = %d，期望 %d", raw, got, want)
		}
	}
}

func TestAniListPassthroughCreditsAndRecommend(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(payload, &body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"data":{"Page":{"media":[{"id":7}]}}}`))
	}))
	defer server.Close()

	deps, _ := metadataDeps(t, metadata.NewAniList(newMetadataHTTPClient(t), server.URL))
	handlers := []struct {
		name    string
		handler http.HandlerFunc
		param   map[string]string
	}{
		{name: "人物作品", handler: deps.anilistPersonCredits, param: map[string]string{"person_id": "9"}},
		{name: "推荐", handler: deps.anilistRecommend, param: map[string]string{"anilist_id": "7"}},
	}
	for _, test := range handlers {
		recorder := httptest.NewRecorder()
		test.handler(recorder, chiRequest("GET", "", test.param, ""))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"Page"`) {
			t.Fatalf("%s = (%d, %s)", test.name, recorder.Code, recorder.Body.String())
		}
	}

	if len(bodies) != len(handlers) {
		t.Fatalf("上游请求数 = %d", len(bodies))
	}
	for index, body := range bodies {
		variables, ok := body["variables"].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 个请求缺少 variables: %#v", index, body)
		}
		if id, ok := variables["id"].(float64); !ok || id <= 0 {
			t.Fatalf("第 %d 个请求 variables.id = %#v", index, variables["id"])
		}
		if query, _ := body["query"].(string); !strings.Contains(query, "query") && !strings.Contains(query, "{") {
			t.Fatalf("第 %d 个请求缺少 GraphQL query: %#v", index, body["query"])
		}
	}
}

// TestAniListCreditsReturnsMediaPersonList 锁住 anilist/credits 的 V2 响应契约。
//
// 和 tmdb/credits 同一类坑：前端把响应当数组 concat，透传 GraphQL 的
// {Media:{characters:{edges:[...]}}} 对象会让演员卡片整块空白。
func TestAniListCreditsReturnsMediaPersonList(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(payload, &body)
		gotQuery, _ = body["query"].(string)
		_, _ = w.Write([]byte(`{"data":{"Media":{"characters":{"edges":[
			{"role":"MAIN","node":{"name":{"full":"艾伦·耶格尔","native":"エレン・イェーガー"}},
			 "voiceActors":[{"id":123,"name":{"full":"梶裕贵"},"image":{"large":"https://img/l.jpg","medium":"https://img/m.jpg"},"siteUrl":"https://anilist.co/staff/123"}]},
			{"role":"SUPPORTING","node":{"name":{"full":"无配音角色"}},"voiceActors":[]}
		]}}}}`))
	}))
	defer server.Close()

	deps, _ := metadataDeps(t, metadata.NewAniList(newMetadataHTTPClient(t), server.URL))
	recorder := httptest.NewRecorder()
	deps.anilistCredits(recorder, chiRequest("GET", "", map[string]string{"anilist_id": "7"}, ""))

	if !strings.Contains(gotQuery, "voiceActors") {
		t.Fatalf("查询应取日语配音演员: %s", gotQuery)
	}
	// V2 的演职员接口返回裸数组，前端 PersonCardListView 直接 concat。
	var parsed []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("响应不是列表: %v，原文 %s", err, recorder.Body.String())
	}
	if len(parsed) != 1 {
		t.Fatalf("演职员列表 = %s", recorder.Body.String())
	}
	person := parsed[0]
	if person["source"] != "anilist" || person["name"] != "梶裕贵" || person["character"] != "艾伦·耶格尔" {
		t.Fatalf("人物字段 = %#v", person)
	}
	if id, ok := person["id"].(float64); !ok || id != 123 {
		t.Fatalf("人物 id 应为配音演员的 Staff id，实际 %#v", person["id"])
	}
	images, ok := person["images"].(map[string]any)
	if !ok || images["large"] != "https://img/l.jpg" {
		t.Fatalf("人物头像 = %#v", person["images"])
	}
}

// TestAniListCreditsGuards 覆盖未配置数据源与上游失败两种降级：都返回空数组。
func TestAniListCreditsGuards(t *testing.T) {
	recorder := httptest.NewRecorder()
	Dependencies{}.anilistCredits(recorder, chiRequest("GET", "", map[string]string{"anilist_id": "7"}, ""))
	if !strings.Contains(recorder.Body.String(), `[]`) {
		t.Fatalf("未配置数据源响应 = %s", recorder.Body.String())
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	deps, _ := metadataDeps(t, metadata.NewAniList(newMetadataHTTPClient(t), server.URL))
	recorder = httptest.NewRecorder()
	deps.anilistCredits(recorder, chiRequest("GET", "", map[string]string{"anilist_id": "7"}, ""))
	if !strings.Contains(recorder.Body.String(), `[]`) {
		t.Fatalf("上游失败响应 = %s", recorder.Body.String())
	}
}
