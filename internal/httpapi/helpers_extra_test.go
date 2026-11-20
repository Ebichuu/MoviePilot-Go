package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func intPtr(value int) *int          { return &value }
func int64Ptr(value int64) *int64    { return &value }
func stringPtr(value string) *string { return &value }

func TestValidSubscriptionUpdateRequestBounds(t *testing.T) {
	tests := []struct {
		name  string
		input subscriptionUpdateRequest
		want  bool
	}{
		{name: "全空", input: subscriptionUpdateRequest{}, want: true},
		{
			name:  "边界内",
			input: subscriptionUpdateRequest{TotalEpisode: intPtr(0), StartEpisode: intPtr(5000), Season: intPtr(999)},
			want:  true,
		},
		{name: "总集数为负", input: subscriptionUpdateRequest{TotalEpisode: intPtr(-1)}, want: false},
		{name: "总集数超上限", input: subscriptionUpdateRequest{TotalEpisode: intPtr(5001)}, want: false},
		{name: "起始集数为负", input: subscriptionUpdateRequest{StartEpisode: intPtr(-1)}, want: false},
		{name: "起始集数超上限", input: subscriptionUpdateRequest{StartEpisode: intPtr(5001)}, want: false},
		{name: "季为负", input: subscriptionUpdateRequest{Season: intPtr(-1)}, want: false},
		{name: "季超上限", input: subscriptionUpdateRequest{Season: intPtr(1000)}, want: false},
		{name: "TMDB ID 为负", input: subscriptionUpdateRequest{TMDBID: int64Ptr(-1)}, want: false},
		{name: "Bangumi ID 为负", input: subscriptionUpdateRequest{BangumiID: int64Ptr(-1)}, want: false},
		{name: "AniList ID 为负", input: subscriptionUpdateRequest{AniListID: int64Ptr(-1)}, want: false},
		{
			name:  "sites 超长",
			input: subscriptionUpdateRequest{Sites: make([]int, 1025)},
			want:  false,
		},
		{
			name:  "filter_groups 超长",
			input: subscriptionUpdateRequest{FilterGroups: make([]string, 1025)},
			want:  false,
		},
		{
			name:  "sites 恰好 1024",
			input: subscriptionUpdateRequest{Sites: make([]int, 1024)},
			want:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validSubscriptionUpdateRequest(test.input); got != test.want {
				t.Fatalf("validSubscriptionUpdateRequest() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSubscriptionUpdateConfigProjectsAllFields(t *testing.T) {
	config := subscriptionUpdateConfig(subscriptionUpdateRequest{
		Keyword: stringPtr("某剧"), TMDBID: int64Ptr(603), MediaKey: stringPtr("tmdb:603"),
		TotalEpisode: intPtr(12), StartEpisode: intPtr(1), Season: intPtr(2),
		Sites: []int{1, 2}, BestVersion: intPtr(3), SearchIMDBID: intPtr(1),
		CustomWords: stringPtr("自定义"), EpisodeGroup: stringPtr("group"),
	})

	keyword, _ := config["keyword"].(*string)
	mediaKey, _ := config["mediaid"].(*string)
	episodeGroup, _ := config["episode_group"].(*string)
	if keyword == nil || *keyword != "某剧" || mediaKey == nil || *mediaKey != "tmdb:603" ||
		episodeGroup == nil || *episodeGroup != "group" {
		t.Fatalf("字符串字段 = %#v", config)
	}
	tmdb, _ := config["tmdbid"].(*int64)
	if tmdb == nil || *tmdb != 603 {
		t.Fatalf("tmdbid = %#v", config["tmdbid"])
	}
	if total, ok := config["total_episode"].(*int); !ok || *total != 12 {
		t.Fatalf("total_episode = %#v", config["total_episode"])
	}
	sites, ok := config["sites"].([]int)
	if !ok || len(sites) != 2 || sites[1] != 2 {
		t.Fatalf("sites = %#v", config["sites"])
	}

	// 未提供的字段仍然出现在 map 里（V2 的 config 是固定形状）。
	empty := subscriptionUpdateConfig(subscriptionUpdateRequest{})
	for _, key := range []string{"keyword", "tmdbid", "doubanid", "bangumiid", "anilistid", "mediaid",
		"total_episode", "start_episode", "sites", "best_version", "custom_words", "filter_groups"} {
		if _, ok := empty[key]; !ok {
			t.Fatalf("缺少字段 %q: %#v", key, empty)
		}
	}
	if empty["total_episode"] != (*int)(nil) || empty["keyword"] != (*string)(nil) {
		t.Fatalf("空输入应为 nil 值: %#v", empty)
	}

	// 编码成 JSON 时 nil 指针序列化为 null，与 V2 的 Optional 一致。
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"total_episode":null`) {
		t.Fatalf("JSON = %s", encoded)
	}
}

func TestSubscriptionUpdateIntegerAndTVType(t *testing.T) {
	if got := subscriptionUpdateInteger(nil); got != 0 {
		t.Fatalf("nil = %d", got)
	}
	if got := subscriptionUpdateInteger(intPtr(7)); got != 7 {
		t.Fatalf("7 = %d", got)
	}
	if got := subscriptionUpdateInteger(intPtr(0)); got != 0 {
		t.Fatalf("0 = %d", got)
	}

	tv := []string{"tv", "TV", " Tv ", "电视剧", " 电视剧 "}
	for _, value := range tv {
		if !isTVSubscriptionType(value) {
			t.Fatalf("%q 应识别为电视剧", value)
		}
	}
	for _, value := range []string{"", "movie", "电影", "anime", "电视剧集"} {
		if isTVSubscriptionType(value) {
			t.Fatalf("%q 不应识别为电视剧", value)
		}
	}
}

func TestValidUserPasswordRules(t *testing.T) {
	valid := []string{"abcd1234", "abcd!@#$", "1234!@#$", "Pässw0rd!", strings.Repeat("a1", 25)}
	for _, password := range valid {
		if !validUserPassword(password) {
			t.Fatalf("%q 应通过", password)
		}
	}

	invalid := []string{
		"", "a1!4567", // 太短（7）
		strings.Repeat("a1", 25) + "x", // 太长（51）
		"abcdefgh",                     // 只有字母
		"12345678",                     // 只有数字
		"!@#$%^&*",                     // 只有特殊字符
		"        ",                     // 全空格
	}
	for _, password := range invalid {
		if validUserPassword(password) {
			t.Fatalf("%q 应被拒绝", password)
		}
	}
}

func TestDecodeJSONBodyGuards(t *testing.T) {
	if err := decodeJSONBody(nil, &map[string]any{}); err == nil {
		t.Fatal("nil 请求应报错")
	}
	if err := decodeJSONBody(httptest.NewRequest(http.MethodPost, "/", nil), &map[string]any{}); err == nil {
		t.Fatal("nil body 应报错")
	}

	var payload struct {
		Name string `json:"name"`
	}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"alice"}`))
	if err := decodeJSONBody(request, &payload); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if payload.Name != "alice" {
		t.Fatalf("payload = %+v", payload)
	}

	bad := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":`))
	if err := decodeJSONBody(bad, &payload); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestEmptySubscriptionShape(t *testing.T) {
	subscription := emptySubscription()
	wantKeys := []string{
		"id", "username", "name", "type", "year", "season", "media_source", "media_id", "state",
		"date", "last_update", "total_episode", "lack_episode", "completed_episode", "sites",
		"filter_groups", "note",
	}
	if len(subscription) != len(wantKeys) {
		t.Fatalf("字段数 = %d, want %d", len(subscription), len(wantKeys))
	}
	for _, key := range wantKeys {
		if _, ok := subscription[key]; !ok {
			t.Fatalf("缺少字段 %q", key)
		}
	}
	if subscription["total_episode"] != 0 || subscription["lack_episode"] != 0 {
		t.Fatalf("计数应为 0: %#v", subscription)
	}
	if sites, ok := subscription["sites"].([]int); !ok || len(sites) != 0 {
		t.Fatalf("sites = %#v", subscription["sites"])
	}
	if groups, ok := subscription["filter_groups"].([]string); !ok || len(groups) != 0 {
		t.Fatalf("filter_groups = %#v", subscription["filter_groups"])
	}
	if notes, ok := subscription["note"].([]any); !ok || len(notes) != 0 {
		t.Fatalf("note = %#v", subscription["note"])
	}
	if subscription["id"] != nil || subscription["state"] != nil {
		t.Fatalf("空值字段 = %#v", subscription)
	}
}
