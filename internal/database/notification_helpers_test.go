package database

import (
	"strings"
	"testing"
)

func TestSubscribeNotifyTitleYear(t *testing.T) {
	cases := []struct {
		title, year, want string
	}{
		{"沙丘2", "2024", "沙丘2 (2024)"},
		{"  沙丘2  ", " 2024 ", "沙丘2 (2024)"},
		{"沙丘2 (2024)", "2024", "沙丘2 (2024)"}, // 标题里已有年份就不再追加
		{"沙丘2", "", "沙丘2"},
		{"", "2024", ""},
	}
	for _, tc := range cases {
		if got := subscribeTitleYear(tc.title, tc.year); got != tc.want {
			t.Fatalf("subscribeTitleYear(%q, %q) = %q，期望 %q", tc.title, tc.year, got, tc.want)
		}
	}
}

func TestSubscribeNotifySeasonFmt(t *testing.T) {
	season := func(value int) *int { return &value }
	cases := []struct {
		season *int
		want   string
	}{
		{nil, ""},
		{season(0), ""},
		{season(-2), ""},
		{season(1), "S01"},
		{season(12), "S12"},
		{season(123), "S123"},
	}
	for _, tc := range cases {
		if got := subscribeSeasonFmt(tc.season); got != tc.want {
			t.Fatalf("subscribeSeasonFmt(%v) = %q，期望 %q", tc.season, got, tc.want)
		}
	}
}

func TestSubscribeNotifyRoute(t *testing.T) {
	if got := subscribeNotificationRoute("movie"); got != "#/subscribe/movie?tab=mysub" {
		t.Fatalf("电影路由 = %q", got)
	}
	if got := subscribeNotificationRoute("  MOVIE  "); got != "#/subscribe/movie?tab=mysub" {
		t.Fatalf("电影路由应大小写/空白不敏感: %q", got)
	}
	for _, mediaType := range []string{"tv", "TV", ""} {
		if got := subscribeNotificationRoute(mediaType); got != "#/subscribe/tv?tab=mysub" {
			t.Fatalf("非电影应落到电视剧路由，%q -> %q", mediaType, got)
		}
	}
}

func TestJoinNotificationContextKeepsValues(t *testing.T) {
	if got := joinNotificationContext(nil); got != "" {
		t.Fatalf("空输入应返回空串，实际 %q", got)
	}
	values := []string{"沙丘2", "2024", "WEB-DL"}
	joined := joinNotificationContext(values)
	for _, value := range values {
		if !strings.Contains(joined, value) {
			t.Fatalf("拼接结果缺少 %q: %q", value, joined)
		}
	}
}

func TestAcceptanceMediaTypeClassification(t *testing.T) {
	if !isDownloadIntentAcceptanceMovie("movie") {
		t.Fatal("movie 应被识别为电影验收")
	}
	if isDownloadIntentAcceptanceMovie("tv") {
		t.Fatal("tv 不应被识别为电影验收")
	}
	if !isDownloadIntentAcceptanceTV("tv") {
		t.Fatal("tv 应被识别为剧集验收")
	}
	if isDownloadIntentAcceptanceTV("movie") {
		t.Fatal("movie 不应被识别为剧集验收")
	}
}
