//go:build integration

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// TestUpdateSubscriptionIntegration 覆盖订阅更新的参数校验、归属校验与两条更新路径。
func TestUpdateSubscriptionIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	seedHTTPAPIUser(t, deps, "alice", false)
	seedHTTPAPIUser(t, deps, "bob", false)
	aliceClaims := auth.Claims{Username: "alice"}
	adminClaims := auth.Claims{Username: "admin", SuperUser: true}

	seasonOne := 1
	movieID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "alice", Name: "某电影", MediaType: "电影", Year: "2023",
		MediaSource: "themoviedb", MediaID: "278",
	})
	showID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "alice", Name: "某剧", MediaType: "电视剧", Year: "2024", Season: &seasonOne,
		MediaSource: "themoviedb", MediaID: "550",
	})
	bobID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "bob", Name: "别人的电影", MediaType: "电影",
		MediaSource: "themoviedb", MediaID: "680",
	})

	post := func(claims auth.Claims, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		deps.updateSubscription(recorder, httpapiJSONRequest(http.MethodPut, "/subscribe/", nil, claims, body))
		return recorder
	}

	if got := post(aliceClaims, `{bad`).Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("坏 JSON 状态码 = %d", got)
	}
	if got := post(aliceClaims, `{"id":1}{}`).Code; got != http.StatusUnprocessableEntity {
		t.Fatalf("多余 JSON 状态码 = %d", got)
	}
	if body := decodeResponseBody(t, post(aliceClaims, `{"id":0}`).Body.Bytes()); body.Success ||
		body.Message != database.ErrSubscriptionNotFound.Error() {
		t.Fatalf("零值 ID = %+v", body)
	}
	if body := decodeResponseBody(t, post(aliceClaims,
		`{"id":`+strconv.FormatInt(movieID, 10)+`,"total_episode":-1}`).Body.Bytes()); body.Success ||
		body.Message != "订阅参数无效" {
		t.Fatalf("非法集数 = %+v", body)
	}
	if body := decodeResponseBody(t, post(aliceClaims, `{"id":999999}`).Body.Bytes()); body.Success ||
		body.Message != database.ErrSubscriptionNotFound.Error() {
		t.Fatalf("未知订阅 = %+v", body)
	}
	if body := decodeResponseBody(t, post(aliceClaims,
		`{"id":`+strconv.FormatInt(bobID, 10)+`,"name":"改名"}`).Body.Bytes()); body.Success ||
		body.Message != database.ErrSubscriptionNotFound.Error() {
		t.Fatalf("越权更新 = %+v", body)
	}

	// 电影局部更新：只改名，title 以外的列保持原值。
	if body := decodeResponseBody(t, post(aliceClaims,
		`{"id":`+strconv.FormatInt(movieID, 10)+
			`,"name":"新电影名","type":"电影","year":"2023","media_source":"themoviedb","media_id":"278"}`).Body.Bytes()); !body.Success {
		t.Fatalf("电影更新 = %+v", body)
	}
	updated, err := deps.Store.GetAccessibleActiveSubscription(t.Context(), "alice", movieID, false)
	if err != nil || updated.Name != "新电影名" || updated.MediaType != "电影" {
		t.Fatalf("电影更新结果 = (%+v, %v)", updated, err)
	}

	// 剧集更新：季 + 起止集走 TV 目标解析分支。
	if body := decodeResponseBody(t, post(aliceClaims, `{"id":`+strconv.FormatInt(showID, 10)+
		`,"type":"电视剧","season":1,"start_episode":2,"total_episode":12}`).Body.Bytes()); !body.Success {
		t.Fatalf("剧集更新 = %+v", body)
	}
	show, err := deps.Store.GetAccessibleActiveSubscription(t.Context(), "alice", showID, false)
	if err != nil || show.Name != "某剧" {
		t.Fatalf("剧集更新结果 = (%+v, %v)", show, err)
	}

	// 管理员可以更新他人订阅。
	if body := decodeResponseBody(t, post(adminClaims,
		`{"id":`+strconv.FormatInt(bobID, 10)+`,"name":"管理员改名"}`).Body.Bytes()); !body.Success {
		t.Fatalf("管理员更新 = %+v", body)
	}
}

// TestUpdateSubscriptionStatusIntegration 覆盖订阅状态控制：ID/状态校验、越权与状态切换。
func TestUpdateSubscriptionStatusIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	seedHTTPAPIUser(t, deps, "alice", false)
	seedHTTPAPIUser(t, deps, "bob", false)
	aliceClaims := auth.Claims{Username: "alice"}

	id := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "alice", Name: "某剧", MediaType: "电视剧",
		MediaSource: "themoviedb", MediaID: "550",
	})
	bobID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "bob", Name: "别人的剧", MediaType: "电视剧",
		MediaSource: "themoviedb", MediaID: "551",
	})
	subID := strconv.FormatInt(id, 10)

	call := func(claims auth.Claims, subID, state string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		deps.updateSubscriptionStatus(recorder, httpapiClaimsRequest(
			http.MethodPut, "/subscribe/"+subID+"/status?state="+state,
			map[string]string{"subid": subID}, claims,
		))
		return recorder
	}

	if got := call(aliceClaims, "abc", "R").Code; got != http.StatusBadRequest {
		t.Fatalf("非法 ID 状态码 = %d", got)
	}
	if body := decodeResponseBody(t, call(aliceClaims, subID, "X").Body.Bytes()); body.Success ||
		body.Message != database.ErrInvalidSubscriptionState.Error() {
		t.Fatalf("非法状态 = %+v", body)
	}
	if body := decodeResponseBody(t, call(aliceClaims, "999999", "R").Body.Bytes()); body.Success ||
		body.Message != database.ErrSubscriptionNotFound.Error() {
		t.Fatalf("未知订阅 = %+v", body)
	}
	if body := decodeResponseBody(t, call(aliceClaims, strconv.FormatInt(bobID, 10), "R").Body.Bytes()); body.Success ||
		body.Message != database.ErrSubscriptionNotFound.Error() {
		t.Fatalf("越权控制 = %+v", body)
	}

	if body := decodeResponseBody(t, call(aliceClaims, subID, "S").Body.Bytes()); !body.Success {
		t.Fatalf("暂停订阅 = %+v", body)
	}
	if state := subscriptionStateIntegration(t, deps, "alice", id); state != subscription.Paused {
		t.Fatalf("暂停后状态 = %q", state)
	}
	if body := decodeResponseBody(t, call(aliceClaims, subID, "R").Body.Bytes()); !body.Success {
		t.Fatalf("恢复订阅 = %+v", body)
	}
	if state := subscriptionStateIntegration(t, deps, "alice", id); state != subscription.Active {
		t.Fatalf("恢复后状态 = %q", state)
	}
}

// TestResetSubscriptionIntegration 覆盖订阅重置：ID 校验、存储缺失、越权与成功路径。
func TestResetSubscriptionIntegration(t *testing.T) {
	deps := newHTTPAPIIntegrationDeps(t)
	seedHTTPAPIUser(t, deps, "alice", false)
	seedHTTPAPIUser(t, deps, "bob", false)
	aliceClaims := auth.Claims{Username: "alice"}

	id := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "alice", Name: "某电影", MediaType: "电影",
		MediaSource: "themoviedb", MediaID: "278",
	})
	bobID := mustCreateHTTPAPISubscription(t, deps, database.NewSubscription{
		Username: "bob", Name: "别人的电影", MediaType: "电影",
		MediaSource: "themoviedb", MediaID: "279",
	})

	call := func(dependencies Dependencies, claims auth.Claims, subID string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		dependencies.resetSubscription(recorder, httpapiClaimsRequest(
			http.MethodPut, "/subscribe/reset/"+subID, map[string]string{"subid": subID}, claims,
		))
		return recorder
	}

	if got := call(deps, aliceClaims, "abc").Code; got != http.StatusBadRequest {
		t.Fatalf("非法 ID 状态码 = %d", got)
	}
	if got := call(Dependencies{}, aliceClaims, "1").Code; got != http.StatusInternalServerError {
		t.Fatalf("存储缺失状态码 = %d", got)
	}
	if body := decodeResponseBody(t, call(deps, aliceClaims, "999999").Body.Bytes()); body.Success ||
		body.Message != database.ErrSubscriptionNotFound.Error() {
		t.Fatalf("未知订阅 = %+v", body)
	}
	if body := decodeResponseBody(t, call(deps, aliceClaims, strconv.FormatInt(bobID, 10)).Body.Bytes()); body.Success ||
		body.Message != database.ErrSubscriptionNotFound.Error() {
		t.Fatalf("越权重置 = %+v", body)
	}
	if body := decodeResponseBody(t, call(deps, aliceClaims, strconv.FormatInt(id, 10)).Body.Bytes()); !body.Success {
		t.Fatalf("重置订阅 = %+v", body)
	}
}

func subscriptionStateIntegration(t *testing.T, deps Dependencies, username string, id int64) subscription.State {
	t.Helper()
	items, err := deps.Store.ListSubscriptions(t.Context(), username, false)
	if err != nil {
		t.Fatalf("查询订阅列表: %v", err)
	}
	for _, item := range items {
		if item.ID == id {
			return item.InternalState
		}
	}
	t.Fatalf("订阅 %d 不存在", id)
	return ""
}
