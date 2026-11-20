package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// runRawHandler 直接返回原始响应体，供「返回数组而非 Response 包装」的兼容端点使用。
func runRawHandler(t *testing.T, handler http.HandlerFunc, method, target string, user *auth.Claims) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	if user != nil {
		request = request.WithContext(context.WithValue(request.Context(), claimsContextKey, *user))
	}
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

func TestSplitTitleYear(t *testing.T) {
	cases := []struct {
		value string
		title string
		year  string
	}{
		{"沙丘 (2021)", "沙丘", "2021"},
		{"沙丘(2021)", "沙丘", "2021"},
		{"  沙丘  ( 2021 )  ", "沙丘", "2021"},
		{"沙丘", "沙丘", ""},
		{"沙丘 (21)", "沙丘 (21)", ""},
		{"沙丘 (abcd)", "沙丘 (abcd)", ""},
		{"沙丘 (2021", "沙丘 (2021", ""},
		{"", "", ""},
		// 只有年份没有标题时标题为空、年份仍然解析出来。
		{"(2021)", "", "2021"},
		// 标题里本身就带括号时按最后一个括号判断，不能误切成两段。
		{"沙丘 (导演剪辑版) (2021)", "沙丘 (导演剪辑版)", "2021"},
	}
	for _, testCase := range cases {
		title, year := splitTitleYear(testCase.value)
		if title != testCase.title || year != testCase.year {
			t.Fatalf("splitTitleYear(%q) = (%q, %q)，期望 (%q, %q)", testCase.value, title, year, testCase.title, testCase.year)
		}
	}
}

func TestPad2(t *testing.T) {
	cases := map[int]string{0: "00", 1: "01", 9: "09", 10: "10", 12: "12", 100: "100"}
	for value, want := range cases {
		if got := pad2(value); got != want {
			t.Fatalf("pad2(%d) = %q，期望 %q", value, got, want)
		}
	}
}

func TestTransferHistorySeasonsAndEpisodes(t *testing.T) {
	entry := database.TransferHistoryEntry{FileFacts: []organization.FileFact{
		{Season: 2, Episodes: []int{3, 1}},
		{Season: 1, Episodes: []int{2, 1}},
		{Season: 10, Episodes: []int{0, -1, 4}},
		// 没有季号的文件不应产生 S00。
		{Season: 0, Episodes: []int{5}},
	}}
	// 去重 + 升序，并补零到两位。
	if got := transferHistorySeasons(entry); got != "S01,S02,S10" {
		t.Fatalf("季 = %q", got)
	}
	// 集号跨文件合并去重，非法值（0/负数）忽略。
	if got := transferHistoryEpisodes(entry); got != "E01,E02,E03,E04,E05" {
		t.Fatalf("集 = %q", got)
	}

	empty := database.TransferHistoryEntry{}
	if got := transferHistorySeasons(empty); got != "" {
		t.Fatalf("空季 = %q", got)
	}
	if got := transferHistoryEpisodes(empty); got != "" {
		t.Fatalf("空集 = %q", got)
	}
}

func TestNewTransferHistoryItem(t *testing.T) {
	created := time.Date(2026, 11, 19, 8, 30, 5, 0, time.UTC)
	entry := database.TransferHistoryEntry{
		TitleYear: "沙丘 (2021)", MediaType: "movie", Category: "电影",
		MediaSource: "themoviedb", MediaID: " 438631 ", PosterURL: "https://img.example/p.jpg",
		Username: "alice", CreatedAt: created,
		FileFacts: []organization.FileFact{{Season: 1, Episodes: []int{1}}},
	}
	item := newTransferHistoryItem(entry, 7)
	if item.ID != 7 || item.Title != "沙丘" || item.Year != "2021" {
		t.Fatalf("条目 = %+v", item)
	}
	if item.Seasons != "S01" || item.Episodes != "E01" {
		t.Fatalf("季集 = %q / %q", item.Seasons, item.Episodes)
	}
	if item.Date != "2026-11-19 08:30:05" {
		t.Fatalf("时间 = %q", item.Date)
	}
	// 来自 TMDB 的记录要带上数字 ID（前端据此跳详情页）。
	if item.TMDBID == nil || *item.TMDBID != 438631 {
		t.Fatalf("TMDB ID = %v", item.TMDBID)
	}
	if !item.Status {
		t.Fatal("整理历史只包含成功记录")
	}

	// 其它来源或非法 ID 不应带 TMDB ID。
	other := newTransferHistoryItem(database.TransferHistoryEntry{MediaSource: "douban", MediaID: "123"}, 1)
	if other.TMDBID != nil {
		t.Fatalf("非 TMDB 记录带了 ID: %v", *other.TMDBID)
	}
	bad := newTransferHistoryItem(database.TransferHistoryEntry{MediaSource: "themoviedb", MediaID: "abc"}, 1)
	if bad.TMDBID != nil {
		t.Fatalf("非法 ID 不应解析: %v", *bad.TMDBID)
	}
}

func TestTransferHistoryHandlerEmptyWithoutStore(t *testing.T) {
	deps := Dependencies{}
	// 没有数据库时如实返回空列表，而不是报错或编造数据。
	status, body := runRawHandler(t, deps.transferHistory, http.MethodGet, "/api/v1/history/transfer", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	var payload struct {
		Success bool             `json:"success"`
		Data    map[string]any   `json:"data"`
		List    []map[string]any `json:"-"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if !payload.Success {
		t.Fatalf("应返回成功: %s", body)
	}
	if payload.Data["total"] != float64(0) {
		t.Fatalf("总数 = %v", payload.Data["total"])
	}
	list, ok := payload.Data["list"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("列表 = %v", payload.Data["list"])
	}

	// 「失败」是整理历史里不存在的状态：直接返回空，且不查库。
	status, body = runRawHandler(t, deps.transferHistory, http.MethodGet, "/api/v1/history/transfer?title=失败", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if payload.Data["total"] != float64(0) {
		t.Fatalf("失败状态应为空: %s", body)
	}

	// status=false 同样为空（整理历史只记录成功）。
	status, body = runRawHandler(t, deps.transferHistory, http.MethodGet, "/api/v1/history/transfer?status=false", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if payload.Data["total"] != float64(0) {
		t.Fatalf("status=false 应为空: %s", body)
	}
}

func TestHistoryDeletionRefusals(t *testing.T) {
	// 这些记录来自幂等的事实快照（整理成功记录 / 下载器任务），删掉会导致重复整理，
	// 因此接口明确拒绝，而不是照搬 V2 的删除语义。
	cases := []struct {
		name    string
		handler http.HandlerFunc
		message string
	}{
		{"清空整理历史", emptyTransferHistory, "整理记录来自组织成功快照（幂等事实），不支持清空"},
		{"删除整理历史", deleteTransferHistory, "整理记录来自组织成功快照（幂等事实），不支持删除单条记录"},
		{"删除下载历史", deleteDownloadHistory, "下载历史来自下载器，请在下载器中删除任务"},
	}
	for _, testCase := range cases {
		status, payload := runCompatHandler(t, testCase.handler, http.MethodDelete, "/api/v1/history")
		if status != http.StatusOK {
			t.Fatalf("%s 状态码 = %d", testCase.name, status)
		}
		if payload.Success {
			t.Fatalf("%s 不应返回成功", testCase.name)
		}
		if payload.Message != testCase.message {
			t.Fatalf("%s 消息 = %q", testCase.name, payload.Message)
		}
	}
}

func TestDashboardNetworkAndTransferDefaults(t *testing.T) {
	deps := Dependencies{}
	// 没有下载器数据源时返回 [0, 0]，前端据此显示空闲。
	status, body := runRawHandler(t, deps.dashboardNetwork, http.MethodGet, "/api/v1/dashboard/network", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	var network []int64
	if err := json.Unmarshal(body, &network); err != nil {
		t.Fatalf("响应不是数组: %v (%s)", err, body)
	}
	if len(network) != 2 || network[0] != 0 || network[1] != 0 {
		t.Fatalf("网络 = %v", network)
	}

	// 默认 7 天、最多 31 天、最少 1 天。
	daysCases := []struct {
		query string
		want  int
	}{
		{"", 7},
		{"?days=3", 3},
		{"?days=0", 1},
		{"?days=999", 31},
		{"?days=abc", 7},
	}
	for _, testCase := range daysCases {
		status, body := runRawHandler(t, deps.dashboardTransfer, http.MethodGet, "/api/v1/dashboard/transfer"+testCase.query, nil)
		if status != http.StatusOK {
			t.Fatalf("%q 状态码 = %d", testCase.query, status)
		}
		var counts []int
		if err := json.Unmarshal(body, &counts); err != nil {
			t.Fatalf("响应不是数组: %v (%s)", err, body)
		}
		if len(counts) != testCase.want {
			t.Fatalf("%q 天数 = %d，期望 %d", testCase.query, len(counts), testCase.want)
		}
		for _, count := range counts {
			if count != 0 {
				t.Fatalf("%q 计数 = %v", testCase.query, counts)
			}
		}
	}
}

func TestDashboardScheduleProgressRefusal(t *testing.T) {
	status, payload := runCompatHandler(t, dashboardScheduleProgress, http.MethodGet, "/api/v1/dashboard/schedule/1/progress")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	// River 周期任务没有进度总线：明确说「后台服务不存在」，而不是返回假进度。
	if payload.Success || payload.Message != "后台服务不存在" {
		t.Fatalf("响应 = %+v", payload)
	}
}

func TestSystemGlobalUserFlagsFollowClaims(t *testing.T) {
	deps := Dependencies{}
	admin := auth.Claims{Username: "admin", SuperUser: true}
	status, body := runRawHandler(t, deps.systemGlobalUser, http.MethodGet, "/api/v1/system/global/user", &admin)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	data := payload.Data
	if data["USER_UNIQUE_ID"] != "admin" {
		t.Fatalf("用户 = %v", data["USER_UNIQUE_ID"])
	}
	// 超管才有分享管理权限。
	if data["SUBSCRIBE_SHARE_MANAGE"] != true || data["WORKFLOW_SHARE_MANAGE"] != true {
		t.Fatalf("超管权限 = %+v", data)
	}
	// 本仓库没有 AI 助手，相关开关必须固定关闭（不能假装可用）。
	if data["AI_AGENT_ENABLE"] != false || data["AI_AGENT_HIDE_ENTRY"] != true {
		t.Fatalf("AI 开关 = %+v", data)
	}
	if data["AI_RECOMMEND_ENABLED"] != false || data["LLM_SUPPORT_AUDIO_INPUT"] != false {
		t.Fatalf("AI 开关 = %+v", data)
	}

	plain := auth.Claims{Username: "bob"}
	_, body = runRawHandler(t, deps.systemGlobalUser, http.MethodGet, "/api/v1/system/global/user", &plain)
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if payload.Data["SUBSCRIBE_SHARE_MANAGE"] != false || payload.Data["USER_UNIQUE_ID"] != "bob" {
		t.Fatalf("普通用户 = %+v", payload.Data)
	}
}
