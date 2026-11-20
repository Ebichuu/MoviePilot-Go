//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func mustCreateHTTPAPISubscription(t *testing.T, deps Dependencies, input database.NewSubscription) int64 {
	t.Helper()
	if input.State == "" {
		input.State = subscription.PendingSearch
	}
	id, err := deps.Store.CreateSubscription(t.Context(), input)
	if err != nil {
		t.Fatalf("创建订阅 %q: %v", input.Name, err)
	}
	return id
}

func decodeSubscriptionList(t *testing.T, recorder *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("解析订阅列表: %v; body=%s", err, recorder.Body.String())
	}
	return items
}

func TestSubscriptionReadStoreIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	seedHTTPAPIUser(t, deps, "admin", true)
	seedHTTPAPIUser(t, deps, "alice", false)
	seedHTTPAPIUser(t, deps, "bob", false)

	aliceClaims := auth.Claims{Username: "alice"}
	adminClaims := auth.Claims{Username: "admin", SuperUser: true}

	seasonOne := 1
	seasonTwo := 2
	showID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "alice", Name: "某剧", MediaType: "电视剧", Year: "2024", Season: &seasonOne,
		MediaSource: "themoviedb", MediaID: "550",
	})
	movieID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "alice", Name: "某电影", MediaType: "电影", Year: "2023",
		MediaSource: "themoviedb", MediaID: "278",
	})
	mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "bob", Name: "别人的剧", MediaType: "电视剧", Season: &seasonTwo,
		MediaSource: "douban", MediaID: "1292052",
	})

	// 列表：普通用户只看自己的，管理员看全部。
	recorder := httptest.NewRecorder()
	deps.listSubscriptions(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/", nil, aliceClaims))
	if items := decodeSubscriptionList(t, recorder); len(items) != 2 {
		t.Fatalf("alice 订阅数 = %d", len(items))
	}
	recorder = httptest.NewRecorder()
	deps.listSubscriptions(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/", nil, adminClaims))
	if items := decodeSubscriptionList(t, recorder); len(items) != 3 {
		t.Fatalf("admin 订阅数 = %d", len(items))
	}
	recorder = httptest.NewRecorder()
	deps.listAllSubscriptions(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/list?token=x", nil))
	if items := decodeSubscriptionList(t, recorder); len(items) != 3 {
		t.Fatalf("全量订阅数 = %d", len(items))
	}

	// 指定用户：管理员可查任意人，普通用户越权返回空数组。
	recorder = httptest.NewRecorder()
	deps.listUserSubscriptions(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/user/alice",
		map[string]string{"username": "alice"}, adminClaims))
	if items := decodeSubscriptionList(t, recorder); len(items) != 2 {
		t.Fatalf("管理员查 alice = %d", len(items))
	}
	recorder = httptest.NewRecorder()
	deps.listUserSubscriptions(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/user/bob",
		map[string]string{"username": "bob"}, aliceClaims))
	if items := decodeSubscriptionList(t, recorder); len(items) != 0 {
		t.Fatalf("越权查询应返回空数组，实际 %d", len(items))
	}
	recorder = httptest.NewRecorder()
	deps.listUserSubscriptions(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/user/bob",
		map[string]string{"username": "bob"}, auth.Claims{Username: "bob"}))
	if items := decodeSubscriptionList(t, recorder); len(items) != 1 {
		t.Fatalf("bob 查自己 = %d", len(items))
	}

	// 详情：命中、非法 ID、未知 ID 都是 200。
	recorder = httptest.NewRecorder()
	deps.getSubscription(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/1",
		map[string]string{"subscribe_id": strconv.FormatInt(showID, 10)}, aliceClaims))
	var detail map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解析订阅详情: %v", err)
	}
	if detail["name"] != "某剧" || detail["media_id"] != "550" {
		t.Fatalf("订阅详情 = %#v", detail)
	}
	for _, raw := range []string{"0", "abc", "99999"} {
		recorder = httptest.NewRecorder()
		deps.getSubscription(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/x",
			map[string]string{"subscribe_id": raw}, aliceClaims))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); len(body.Message) != 0 {
			t.Fatalf("空订阅分支不该带错误: %+v", body)
		}
		var empty map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &empty); err != nil {
			t.Fatalf("解析空订阅: %v", err)
		}
		if empty["id"] != nil || empty["total_episode"] != float64(0) {
			t.Fatalf("%s 空订阅 = %#v", raw, empty)
		}
	}

	// 按媒体键查询：命中、季不匹配、未知媒体、非法键。
	mediaCases := []struct {
		name   string
		target string
		param  string
		wantID any
	}{
		{name: "命中", target: "/subscribe/media/themoviedb:550", param: "themoviedb:550", wantID: showID},
		{name: "季不匹配", target: "/subscribe/media/themoviedb:550?season=2", param: "themoviedb:550", wantID: nil},
		{name: "未知媒体", target: "/subscribe/media/themoviedb:999", param: "themoviedb:999", wantID: nil},
		{name: "非法键", target: "/subscribe/media/nokey", param: "nokey", wantID: nil},
	}
	for _, test := range mediaCases {
		recorder = httptest.NewRecorder()
		deps.subscriptionByMedia(recorder, httpapiClaimsRequest(http.MethodGet, test.target,
			map[string]string{"mediaid": test.param}, aliceClaims))
		var item map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &item); err != nil {
			t.Fatalf("%s 解析: %v; body=%s", test.name, err, recorder.Body.String())
		}
		switch expected := test.wantID.(type) {
		case int64:
			if item["id"] != float64(expected) {
				t.Fatalf("%s = %#v", test.name, item)
			}
		default:
			if item["id"] != nil {
				t.Fatalf("%s 应为空订阅: %#v", test.name, item)
			}
		}
	}

	// 关键字分页搜索。
	recorder = httptest.NewRecorder()
	deps.subscribeSearch(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/search?keyword=%E5%89%A7", nil, aliceClaims))
	if items := decodeSubscriptionList(t, recorder); len(items) != 1 || items[0]["name"] != "某剧" {
		t.Fatalf("关键字搜索 = %#v", items)
	}
	recorder = httptest.NewRecorder()
	deps.subscribeSearch(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/search?count=1&page=2", nil, aliceClaims))
	if items := decodeSubscriptionList(t, recorder); len(items) != 1 {
		t.Fatalf("第二页 = %d", len(items))
	}
	recorder = httptest.NewRecorder()
	deps.subscribeSearch(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/search?page=99", nil, aliceClaims))
	if items := decodeSubscriptionList(t, recorder); len(items) != 0 {
		t.Fatalf("超出页码 = %d", len(items))
	}

	// 按媒体键删除：命中、季不匹配、非法键、未知媒体都是幂等成功。
	for _, test := range []struct {
		name  string
		path  string
		param string
	}{
		{name: "季不匹配", path: "/subscribe/media/themoviedb:278?season=3", param: "themoviedb:278"},
		{name: "非法键", path: "/subscribe/media/nokey", param: "nokey"},
		{name: "未知媒体", path: "/subscribe/media/douban:1", param: "douban:1"},
		{name: "命中", path: "/subscribe/media/themoviedb:278", param: "themoviedb:278"},
	} {
		recorder = httptest.NewRecorder()
		deps.deleteSubscriptionByMedia(recorder, httpapiClaimsRequest(http.MethodDelete, test.path,
			map[string]string{"mediaid": test.param}, aliceClaims))
		if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
			t.Fatalf("%s 删除 = %+v", test.name, body)
		}
	}
	if _, err := deps.Store.ListSubscriptions(t.Context(), "alice", false); err != nil {
		t.Fatalf("删除后查询: %v", err)
	}
	remaining, err := deps.Store.ListSubscriptions(t.Context(), "alice", false)
	if err != nil || len(remaining) != 1 || remaining[0].ID != showID {
		t.Fatalf("删除后剩余 = (%+v, %v)", remaining, err)
	}

	// 按 ID 删除：非法 ID 400、越权（bob 的订阅）幂等成功、命中成功。
	recorder = httptest.NewRecorder()
	deps.deleteSubscription(recorder, httpapiClaimsRequest(http.MethodDelete, "/subscribe/abc",
		map[string]string{"subid": "abc"}, aliceClaims))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("非法订阅 ID status = %d", recorder.Code)
	}
	other, err := deps.Store.ListSubscriptions(t.Context(), "bob", false)
	if err != nil || len(other) != 1 {
		t.Fatalf("bob 订阅 = (%+v, %v)", other, err)
	}
	recorder = httptest.NewRecorder()
	deps.deleteSubscription(recorder, httpapiClaimsRequest(http.MethodDelete, "/subscribe/x",
		map[string]string{"subid": strconv.FormatInt(other[0].ID, 10)}, aliceClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("越权删除 = %+v", body)
	}
	if items, err := deps.Store.ListSubscriptions(t.Context(), "bob", false); err != nil || len(items) != 1 {
		t.Fatalf("越权删除不应生效: (%d, %v)", len(items), err)
	}
	recorder = httptest.NewRecorder()
	deps.deleteSubscription(recorder, httpapiClaimsRequest(http.MethodDelete, "/subscribe/x",
		map[string]string{"subid": strconv.FormatInt(movieID, 10)}, aliceClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); !body.Success {
		t.Fatalf("按 ID 删除 = %+v", body)
	}

	// 订阅搜索：坏 ID/未知 ID 返回 []，命中时因未配置搜索协调器返回提示。
	for _, raw := range []string{"abc", "0", "99999"} {
		recorder = httptest.NewRecorder()
		deps.subscribeSearchByID(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/search/x",
			map[string]string{"subscribe_id": raw}, aliceClaims))
		if items := decodeSubscriptionList(t, recorder); len(items) != 0 {
			t.Fatalf("%s 订阅搜索 = %d", raw, len(items))
		}
	}
	recorder = httptest.NewRecorder()
	deps.subscribeSearchByID(recorder, httpapiClaimsRequest(http.MethodGet, "/subscribe/search/x",
		map[string]string{"subscribe_id": strconv.FormatInt(showID, 10)}, aliceClaims))
	if body := decodeResponseBody(t, recorder.Body.Bytes()); body.Message != "未搜索到任何资源" {
		t.Fatalf("订阅搜索命中 = %+v", body)
	}

	// 无存储时全量接口返回空数组。
	if items := decodeSubscriptionList(t, func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		Dependencies{}.listAllSubscriptions(recorder, httptest.NewRequest(http.MethodGet, "/subscribe/list", nil))
		return recorder
	}()); len(items) != 0 {
		t.Fatalf("无存储全量列表 = %d", len(items))
	}
}
