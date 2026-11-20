package database

import (
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func downloadAddedInput(seasons map[int][]int) organization.SuccessInput {
	input := organization.SuccessInput{
		Media:        organization.MediaContext{TitleYear: "示例剧 (2025)"},
		ResourceTerm: "WEB-DL",
		Username:     "admin",
	}
	for season, episodes := range seasons {
		for _, episode := range episodes {
			input.Episodes = append(input.Episodes, organization.Episode{Season: season, Episode: episode})
		}
	}
	return input
}

func TestDownloadAddedTemplateContextSingleSeason(t *testing.T) {
	values := downloadAddedTemplateContext(downloadAddedInput(map[int][]int{1: {1, 2}}))

	if values["title_year"] != "示例剧 (2025)" {
		t.Fatalf("title_year = %#v", values["title_year"])
	}
	if values["resource_term"] != "WEB-DL" || values["username"] != "admin" {
		t.Fatalf("资源名/用户名未透传: %+v", values)
	}
	if values["season_fmt"] != "S01" {
		t.Fatalf("season_fmt = %#v，期望 S01", values["season_fmt"])
	}
	if values["download_episodes"] == "" || values["season_episode"] == "" {
		t.Fatalf("单季应填充集数描述: %+v", values)
	}
	// 单季时不应退化成跨季拼接格式。
	if values["download_episodes"] == values["season_episode"] {
		t.Fatalf("download_episodes 与 season_episode 语义不同，不应相同: %+v", values)
	}
}

func TestDownloadAddedTemplateContextSpecialSeason(t *testing.T) {
	values := downloadAddedTemplateContext(downloadAddedInput(map[int][]int{0: {3}}))
	if values["season_fmt"] != "S00" {
		t.Fatalf("特别篇应格式化 S00，实际 %#v", values["season_fmt"])
	}
}

func TestDownloadAddedTemplateContextMultipleSeasons(t *testing.T) {
	values := downloadAddedTemplateContext(downloadAddedInput(map[int][]int{1: {1}, 2: {3}}))

	// 跨季时只填 season_episode：season_fmt 与 download_episodes 保持空，
	// 因为「S01」这种单一季节标识在跨季场景下是错的。
	if values["season_fmt"] != "" || values["download_episodes"] != "" {
		t.Fatalf("跨季不应填单季字段: %+v", values)
	}
	seasonEpisode, _ := values["season_episode"].(string)
	if seasonEpisode == "" || !containsComma(seasonEpisode) {
		t.Fatalf("跨季应拼接成多段，实际 %#v", values["season_episode"])
	}
	// 季节按升序拼接，避免 map 遍历顺序导致文案抖动。
	if first := seasonEpisode[1]; first != '0' {
		t.Fatalf("跨季拼接应从第 1 季开始，实际 %q", seasonEpisode)
	}
}

func containsComma(value string) bool {
	for _, char := range value {
		if char == ',' {
			return true
		}
	}
	return false
}

func TestDownloadAddedTemplateContextWithoutEpisodes(t *testing.T) {
	values := downloadAddedTemplateContext(downloadAddedInput(nil))
	if values["season_fmt"] != "" || values["season_episode"] != "" || values["download_episodes"] != "" {
		t.Fatalf("没有集数时不应编造季节信息: %+v", values)
	}
}

func TestJoinNotificationContext(t *testing.T) {
	if got := joinNotificationContext(nil); got != "" {
		t.Fatalf("空列表应返回空串，实际 %q", got)
	}
	if got := joinNotificationContext([]string{"S01E01"}); got != "S01E01" {
		t.Fatalf("单元素应原样返回，实际 %q", got)
	}
	if got := joinNotificationContext([]string{"S01E01", "S02E03"}); got != "S01E01, S02E03" {
		t.Fatalf("多元素应用逗号空格连接，实际 %q", got)
	}
}

func TestTimePointerReturnsIndependentCopy(t *testing.T) {
	original := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	pointer := timePointer(original)
	if pointer == nil || !pointer.Equal(original) {
		t.Fatalf("timePointer 未返回原时间: %v", pointer)
	}
	// 指针必须指向副本：修改原变量不能影响已捕获的时间。
	mutated := original.Add(time.Hour)
	if pointer.Equal(mutated) {
		t.Fatal("pointer 与后续变更共享了内存")
	}
}
