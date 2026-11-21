package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

type fakeSchedulerRunner struct {
	triggered bool
	err       error
	jobs      []string
}

func (f *fakeSchedulerRunner) RunScheduler(_ context.Context, jobID string) (bool, error) {
	f.jobs = append(f.jobs, jobID)
	return f.triggered, f.err
}

type fakeMediaServerPlayback struct {
	playback MediaServerPlayback
	found    bool
	err      error
	items    []string
}

func (f *fakeMediaServerPlayback) PlayMediaServerItem(_ context.Context, itemID string) (MediaServerPlayback, bool, error) {
	f.items = append(f.items, itemID)
	return f.playback, f.found, f.err
}

func TestRuleTestMetaInfoParsesTitle(t *testing.T) {
	info := ruleTestMetaInfo("Show.Name.S01E02.2020.1080p", "简中字幕")
	if info["title"] != "Show Name" || info["year"] != "2020" {
		t.Fatalf("meta_info = %#v", info)
	}
	if info["type"] != "电视剧" {
		t.Fatalf("type = %#v", info["type"])
	}
	if info["season"] == "" || info["episode"] == "" {
		t.Fatalf("季集未识别: %#v", info)
	}
	if info["original_name"] == nil || info["en_title"] != info["title"] {
		t.Fatalf("original/en 字段 = %#v", info)
	}

	// 只有副标题时也不 panic，字段齐全。
	empty := ruleTestMetaInfo("", "")
	for _, key := range []string{"title", "en_title", "year", "season", "episode", "type", "original_name"} {
		if _, ok := empty[key]; !ok {
			t.Fatalf("缺少字段 %q: %#v", key, empty)
		}
	}
}

func TestSystemRuleTestGuards(t *testing.T) {
	// 空标题。
	recorder := httptest.NewRecorder()
	Dependencies{}.systemRuleTest(recorder, httptest.NewRequest(http.MethodGet, "/system/ruletest?rulegroup_name=默认", nil))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "标题不能为空" {
		t.Fatalf("空标题 = %+v", body)
	}

	// 没有设置存储 → 规则组为空 → 报「不存在」并带上回显数据。
	recorder = httptest.NewRecorder()
	Dependencies{}.systemRuleTest(recorder, httptest.NewRequest(http.MethodGet,
		"/system/ruletest?title=Show.S01E01&subtitle=%E5%AD%97%E5%B9%95&rulegroup_name=%E9%BB%98%E8%AE%A4", nil))
	var body struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, recorder.Body.String())
	}
	if body.Success || body.Message != "过滤规则组 默认 不存在！" {
		t.Fatalf("缺规则组 = %s", recorder.Body.String())
	}
	if body.Data["title"] != "Show.S01E01" || body.Data["subtitle"] != "字幕" ||
		body.Data["rulegroup_name"] != "默认" || body.Data["matched"] != false ||
		body.Data["rulegroup"] != nil || body.Data["media_info"] != nil || body.Data["priority"] != nil {
		t.Fatalf("回显数据 = %#v", body.Data)
	}
	if meta, ok := body.Data["meta_info"].(map[string]any); !ok || meta["title"] == "" {
		t.Fatalf("meta_info = %#v", body.Data["meta_info"])
	}
}

func TestSystemUpgradeAndWikiStubs(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		message string
	}{
		{name: "升级", handler: systemUpgrade, message: "请通过容器编排更新镜像"},
		{name: "Wiki 同步", handler: systemSyncPluginWiki, message: "当前版本不支持插件市场 Wiki 同步"},
	} {
		recorder := httptest.NewRecorder()
		test.handler(recorder, httptest.NewRequest(http.MethodPost, "/system", nil))
		body := decodeResponseBody(t, recorder.Body.Bytes())
		if body.Success || body.Message != test.message {
			t.Fatalf("%s = %+v", test.name, body)
		}
	}
}

func TestSystemRunScheduler(t *testing.T) {
	// 缺 jobid。
	recorder := httptest.NewRecorder()
	Dependencies{}.systemRunScheduler(recorder, httptest.NewRequest(http.MethodGet, "/system/runscheduler", nil))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "命令不能为空！" {
		t.Fatalf("空 jobid = %+v", body)
	}

	// 没有调度器。
	recorder = httptest.NewRecorder()
	Dependencies{}.systemRunScheduler(recorder, httptest.NewRequest(http.MethodGet, "/system/runscheduler?jobid=rss", nil))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "后台服务不可用" {
		t.Fatalf("无调度器 = %+v", body)
	}

	// 调度器不认识该服务 / 执行失败都是同一文案。
	for _, runner := range []*fakeSchedulerRunner{
		{triggered: false},
		{triggered: true, err: errors.New("boom")},
	} {
		recorder = httptest.NewRecorder()
		Dependencies{Scheduler: runner}.systemRunScheduler(recorder, httptest.NewRequest(http.MethodGet, "/system/runscheduler?jobid=rss", nil))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "后台服务不存在" {
			t.Fatalf("调度失败 = %+v", body)
		}
		if len(runner.jobs) != 1 || runner.jobs[0] != "rss" {
			t.Fatalf("jobid 未透传: %v", runner.jobs)
		}
	}

	// 成功触发。
	runner := &fakeSchedulerRunner{triggered: true}
	recorder = httptest.NewRecorder()
	Dependencies{Scheduler: runner}.systemRunScheduler(recorder, httptest.NewRequest(http.MethodGet, "/system/runscheduler?jobid=rss", nil))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("成功触发 = %+v", body)
	}
}

func TestMediaServerPlaybackResolution(t *testing.T) {
	playRequest := func(itemID string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/mediaserver/play/item", nil)
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add("*", itemID)
		return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	}

	// 空 itemid。
	recorder := httptest.NewRecorder()
	Dependencies{}.mediaServerPlay(recorder, playRequest("   "))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "参数错误" {
		t.Fatalf("空 itemid = %+v", body)
	}

	// 未配置媒体服务器。
	recorder = httptest.NewRecorder()
	Dependencies{}.mediaServerPlay(recorder, playRequest("/library/item/1"))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "未配置媒体服务器" {
		t.Fatalf("无媒体服务器 = %+v", body)
	}

	// 未找到 / 报错 / 空 URL 同一文案。
	for _, source := range []*fakeMediaServerPlayback{
		{found: false},
		{found: true, err: errors.New("boom")},
		{found: true, playback: MediaServerPlayback{URL: "  "}},
	} {
		recorder = httptest.NewRecorder()
		Dependencies{MediaServerPlayback: source}.mediaServerPlay(recorder, playRequest("/library/item/1"))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Success || body.Message != "未找到播放地址" {
			t.Fatalf("未找到播放地址 = %+v", body)
		}
	}

	// 命中：itemid 去掉首尾斜杠后透传，返回成功与播放结构。
	source := &fakeMediaServerPlayback{
		found:    true,
		playback: MediaServerPlayback{URL: "https://emby.test/web/index.html#!/item?id=1", ItemID: "1", ServerType: "emby"},
	}
	recorder = httptest.NewRecorder()
	Dependencies{MediaServerPlayback: source}.mediaServerPlay(recorder, playRequest("/library/item/1/"))
	if len(source.items) != 1 || source.items[0] != "library/item/1" {
		t.Fatalf("itemid = %v", source.items)
	}
	var body struct {
		Success bool                `json:"success"`
		Data    MediaServerPlayback `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析播放响应: %v; body=%s", err, recorder.Body.String())
	}
	if !body.Success || body.Data.URL != source.playback.URL || body.Data.ServerType != "emby" {
		t.Fatalf("播放响应 = %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "error") {
		t.Fatalf("响应不应包含错误字段: %s", recorder.Body.String())
	}
}

func TestSystemRuleTestBuiltinBestVersionPreset(t *testing.T) {
	rec := httptest.NewRecorder()
	Dependencies{}.systemRuleTest(rec, httptest.NewRequest(
		http.MethodGet,
		"/system/ruletest?title=Dune.2021.2160p.UHD.BluRay.REMUX.HEVC&subtitle=%E4%B8%AD%E5%AD%97&rulegroup_name=%E5%86%85%E7%BD%AE%E6%B4%97%E7%89%88",
		nil,
	))
	var resp struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if !resp.Success || resp.Data["matched"] != true || resp.Data["priority"] != float64(2) || resp.Data["pri_order"] != float64(99) || resp.Data["will_complete_best_version"] != false {
		t.Fatalf("unexpected builtin wash ruletest response: %+v", resp)
	}
}
