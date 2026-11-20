//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
)

type shareResponse struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func TestSubscribeShareStoreIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	seedHTTPAPIUser(t, deps, "alice", false)
	seedHTTPAPIUser(t, deps, "bob", false)
	aliceClaims := auth.Claims{Username: "alice"}
	bobClaims := auth.Claims{Username: "bob"}

	season := 2
	showID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "alice", Name: "某剧", MediaType: "电视剧", Year: "2024", Season: &season,
		MediaSource: "themoviedb", MediaID: "550",
		Config: map[string]any{"sites": []any{float64(1), float64(2)}, "filter": "1080p", "poster": "https://img.test/p.jpg"},
	})

	// 守卫：未登录、无存储、坏 JSON、非法 subscribe_id、订阅不存在。
	recorder := httptest.NewRecorder()
	deps.createSubscribeShare(recorder, httptest.NewRequest(http.MethodPost, "/subscribe/share", strings.NewReader(`{}`)))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "未登录" {
		t.Fatalf("未登录 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	Dependencies{}.createSubscribeShare(recorder, httpapiJSONRequest(http.MethodPost, "/subscribe/share", nil, aliceClaims, `{"subscribe_id":1}`))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "存储未配置" {
		t.Fatalf("无存储 = %+v", body)
	}
	for _, test := range []struct{ payload, message string }{
		{`{"subscribe_id":`, "请求体不是合法 JSON"},
		{`{"subscribe_id":0}`, "subscribe_id 无效"},
		{`{"subscribe_id":99999}`, "订阅不存在"},
	} {
		recorder = httptest.NewRecorder()
		deps.createSubscribeShare(recorder, httpapiJSONRequest(http.MethodPost, "/subscribe/share", nil, aliceClaims, test.payload))
		if body := decodeShareResponse(t, recorder); body.Success || body.Message != test.message {
			t.Fatalf("%s = %+v", test.payload, body)
		}
	}

	// 创建成功：媒体字段固化、sites/filter/poster 从订阅配置带过来、描述去空白。
	recorder = httptest.NewRecorder()
	deps.createSubscribeShare(recorder, httpapiJSONRequest(http.MethodPost, "/subscribe/share", nil, aliceClaims,
		`{"subscribe_id":`+intToString(showID)+`,"description":"  自留  "}`))
	created := decodeShareResponse(t, recorder)
	if !created.Success || created.Message != "分享创建成功" {
		t.Fatalf("创建分享 = %+v", created)
	}
	shareID := int64(created.Data["id"].(float64))
	shareKey, _ := created.Data["share_key"].(string)
	if len(shareKey) != 16 || created.Data["title"] != "某剧" || created.Data["season"] != float64(2) {
		t.Fatalf("分享数据 = %#v", created.Data)
	}
	if created.Data["description"] != "自留" || created.Data["poster"] != "https://img.test/p.jpg" ||
		created.Data["filter"] != "1080p" || created.Data["media_id"] != "550" {
		t.Fatalf("分享扩展字段 = %#v", created.Data)
	}
	sites, ok := created.Data["sites"].([]any)
	if !ok || len(sites) != 2 || sites[1] != float64(2) {
		t.Fatalf("sites = %#v", created.Data["sites"])
	}

	// 列表与统计。
	recorder = httptest.NewRecorder()
	deps.listSubscribeShares(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/shares", nil, aliceClaims))
	var shares []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &shares); err != nil || len(shares) != 1 {
		t.Fatalf("分享列表 = (%s, %v)", recorder.Body.String(), err)
	}
	recorder = httptest.NewRecorder()
	deps.listSubscribeShares(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/shares", nil, bobClaims))
	if shares := decodeMapList(t, recorder); len(shares) != 0 {
		t.Fatalf("bob 分享列表 = %d", len(shares))
	}
	recorder = httptest.NewRecorder()
	deps.subscribeShareStatistics(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/share/statistics", nil, aliceClaims))
	var statistics map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &statistics); err != nil {
		t.Fatalf("解析统计: %v", err)
	}
	if statistics["total"] != float64(1) || statistics["follow_total"] != float64(0) || statistics["follow_count"] != float64(0) {
		t.Fatalf("统计 = %#v", statistics)
	}

	// 关注：非法 ID、成功、重复关注幂等、统计跟着涨。
	recorder = httptest.NewRecorder()
	deps.followSubscribeShare(recorder, httpapiJSONRequest(http.MethodPost, "/subscribe/follow", nil, bobClaims, `{"share_id":0}`))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "share_id 无效" {
		t.Fatalf("非法 share_id = %+v", body)
	}
	for attempt := 0; attempt < 2; attempt++ {
		recorder = httptest.NewRecorder()
		deps.followSubscribeShare(recorder, httpapiClaimsRequest(http.MethodPost,
			"/subscribe/follow?share_id="+intToString(shareID), nil, bobClaims))
		if body := decodeShareResponse(t, recorder); !body.Success {
			t.Fatalf("第 %d 次关注 = %+v", attempt+1, body)
		}
	}
	recorder = httptest.NewRecorder()
	deps.subscribeShareStatistics(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/share/statistics", nil, aliceClaims))
	if err := json.Unmarshal(recorder.Body.Bytes(), &statistics); err != nil {
		t.Fatalf("解析统计: %v", err)
	}
	if statistics["follow_total"] != float64(1) {
		t.Fatalf("关注后的统计 = %#v", statistics)
	}
	recorder = httptest.NewRecorder()
	deps.listSubscribeFollows(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/follow", nil, bobClaims))
	if follows := decodeMapList(t, recorder); len(follows) != 1 || follows[0]["id"] != float64(shareID) {
		t.Fatalf("关注列表 = %#v", follows)
	}

	// 取消关注：未关注 → 提示，已关注 → 成功。
	recorder = httptest.NewRecorder()
	deps.unfollowSubscribeShare(recorder, httpapiClaimsRequest(http.MethodDelete,
		"/subscribe/follow?share_id="+intToString(shareID), nil, aliceClaims))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "尚未关注该分享" {
		t.Fatalf("未关注取消 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.unfollowSubscribeShare(recorder, httpapiClaimsRequest(http.MethodDelete,
		"/subscribe/follow?share_id="+intToString(shareID), nil, bobClaims))
	if body := decodeShareResponse(t, recorder); !body.Success || body.Message != "已取消关注" {
		t.Fatalf("取消关注 = %+v", body)
	}

	// 按 key 校验分享：空 key、未知 key、命中。
	recorder = httptest.NewRecorder()
	deps.checkSubscribeShare(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/check", nil))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "share_key 不能为空" {
		t.Fatalf("空 key = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.checkSubscribeShare(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/check?sharekey=missing", nil))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "分享不存在" {
		t.Fatalf("未知 key = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.checkSubscribeShare(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/check?share_key="+shareKey, nil))
	checked := decodeShareResponse(t, recorder)
	if !checked.Success || checked.Data["share_key"] != shareKey || checked.Data["title"] != "某剧" {
		t.Fatalf("校验分享 = %+v", checked)
	}

	// 刷新：非法 ID、未知 ID、命中。
	recorder = httptest.NewRecorder()
	deps.refreshSubscribeShare(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/refresh", nil))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "share_id 无效" {
		t.Fatalf("刷新非法 ID = %+v", body)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Store: deps.Store}.refreshSubscribeShare(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/refresh?share_id=99999", nil))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "分享不存在" {
		t.Fatalf("刷新未知 ID = %+v", body)
	}
	recorder = httptest.NewRecorder()
	Dependencies{Store: deps.Store}.refreshSubscribeShare(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/refresh?share_id="+intToString(shareID), nil))
	if body := decodeShareResponse(t, recorder); !body.Success || body.Data["id"] != float64(shareID) {
		t.Fatalf("刷新 = %+v", body)
	}

	// Fork 成自己的订阅：成功并把 sites/filter 带进新订阅配置，未知分享报错。
	recorder = httptest.NewRecorder()
	deps.forkSubscribeShare(recorder, httpapiClaimsRequest(http.MethodPost, "/subscribe/fork", nil, bobClaims))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "share_id 无效" {
		t.Fatalf("fork 缺 share_id = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.forkSubscribeShare(recorder, httpapiJSONRequest(http.MethodPost, "/subscribe/fork", nil, bobClaims,
		`{"share_id":`+intToString(shareID)+`}`))
	forked := decodeShareResponse(t, recorder)
	if !forked.Success || forked.Message != "已将分享转为订阅" {
		t.Fatalf("fork = %+v", forked)
	}
	bobSubs, err := deps.Store.ListSubscriptions(t.Context(), "bob", false)
	if err != nil || len(bobSubs) != 1 || bobSubs[0].Name != "某剧" || bobSubs[0].Season == nil || *bobSubs[0].Season != 2 {
		t.Fatalf("fork 后的订阅 = (%+v, %v)", bobSubs, err)
	}
	if _, ok := bobSubs[0].Config["sites"]; !ok {
		t.Fatalf("fork 配置未带 sites: %#v", bobSubs[0].Config)
	}
	recorder = httptest.NewRecorder()
	deps.forkSubscribeShare(recorder, httpapiClaimsRequest(http.MethodPost,
		"/subscribe/fork?share_id=99999", nil, bobClaims))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "分享不存在" {
		t.Fatalf("fork 未知分享 = %+v", body)
	}

	// 订阅文件清单：非法 ID、未知 ID、已知订阅（无文件）。
	recorder = httptest.NewRecorder()
	deps.subscribeFileList(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/files/abc", nil))
	if files := decodeMapList(t, recorder); len(files) != 0 {
		t.Fatalf("非法订阅文件 = %d", len(files))
	}
	recorder = httptest.NewRecorder()
	Dependencies{Store: deps.Store}.subscribeFileList(recorder, httpapiClaimsRequest(http.MethodGet,
		"/subscribe/files/99999", map[string]string{"subscribe_id": "99999"}, aliceClaims))
	if files := decodeMapList(t, recorder); len(files) != 0 {
		t.Fatalf("未知订阅文件 = %d", len(files))
	}
	recorder = httptest.NewRecorder()
	Dependencies{Store: deps.Store}.subscribeFileList(recorder, httpapiClaimsRequest(http.MethodGet,
		"/subscribe/files/1", map[string]string{"subscribe_id": intToString(showID)}, aliceClaims))
	if files := decodeMapList(t, recorder); len(files) != 0 {
		t.Fatalf("已知订阅文件 = %d", len(files))
	}

	// 删除：非法 ID、他人分享、本人分享。
	recorder = httptest.NewRecorder()
	deps.deleteSubscribeShare(recorder, httpapiClaimsRequest(http.MethodDelete, "/subscribe/share/0",
		map[string]string{"share_id": "0"}, aliceClaims))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "share_id 无效" {
		t.Fatalf("删除非法 ID = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.deleteSubscribeShare(recorder, httpapiClaimsRequest(http.MethodDelete, "/subscribe/share/1",
		map[string]string{"share_id": intToString(shareID)}, bobClaims))
	if body := decodeShareResponse(t, recorder); body.Success || body.Message != "分享不存在或无权删除" {
		t.Fatalf("越权删除 = %+v", body)
	}
	recorder = httptest.NewRecorder()
	deps.deleteSubscribeShare(recorder, httpapiClaimsRequest(http.MethodDelete, "/subscribe/share/1",
		map[string]string{"share_id": intToString(shareID)}, aliceClaims))
	if body := decodeShareResponse(t, recorder); !body.Success || body.Message != "分享已删除" {
		t.Fatalf("删除分享 = %+v", body)
	}
	if shares := decodeMapList(t, func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		deps.listSubscribeShares(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/shares", nil, aliceClaims))
		return recorder
	}()); len(shares) != 0 {
		t.Fatalf("删除后分享列表 = %d", len(shares))
	}
}

func decodeShareResponse(t *testing.T, recorder *httptest.ResponseRecorder) shareResponse {
	t.Helper()
	var body shareResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析分享响应: %v; body=%s", err, recorder.Body.String())
	}
	return body
}

func decodeMapList(t *testing.T, recorder *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("解析列表: %v; body=%s", err, recorder.Body.String())
	}
	return items
}

func intToString(value int64) string {
	return strconv.FormatInt(value, 10)
}
