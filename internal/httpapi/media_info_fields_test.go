package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

// TestV2MediaInfoKeepsFrontendRequiredFields 锁住详情页模板依赖的字段形状。
//
// 前端 `media-AFq9rljV.js` 里写的是 `media.runtime || media.episode_run_time[0]`
// 以及 `media.genres.map(...)`：字段缺省或为 null 时前者抛 TypeError、后者抛 TypeError，
// Vue 渲染中断、整页空白（2026-11-20 用户反馈「点电影进去后面又是空白」）。
// 所以这些键必须存在且是空数组/0，而不是省略。
func TestV2MediaInfoKeepsFrontendRequiredFields(t *testing.T) {
	item := metadata.Media{
		Source: "douban", ID: "36915988", Type: "movie",
		Title: "饥饿游戏：收获日出", Year: 2026,
	}
	assertMediaInfoFrontendFields(t, "v2MediaInfo", v2MediaInfo(item, 0))
	assertMediaInfoFrontendFields(t, "emptyMediaInfo", emptyMediaInfo())
}

// TestV2MediaInfoFillsKnownMetadata 确认数据源给到的字段真的用上了，而不是一律留空。
func TestV2MediaInfoFillsKnownMetadata(t *testing.T) {
	item := metadata.Media{
		Source: "themoviedb", ID: "movie:693134", Type: "movie", Title: "沙丘2",
		OriginalTitle: "Dune: Part Two", OriginalLanguage: "en", Year: 2024,
		BackdropURL: "https://image.example/backdrop.jpg", VoteAverage: 8.2,
		OriginCountries: []string{"US"}, ProductionCountries: []string{"US"},
		GenreIDs: []int{878, 12},
	}
	info := v2MediaInfo(item, 0)
	if info["backdrop_path"] != item.BackdropURL {
		t.Fatalf("backdrop_path = %v，应为 %q", info["backdrop_path"], item.BackdropURL)
	}
	if info["vote_average"] != 8.2 {
		t.Fatalf("vote_average = %v，应为 8.2", info["vote_average"])
	}
	if info["original_language"] != "en" {
		t.Fatalf("original_language = %v", info["original_language"])
	}
	if info["tmdb_id"] != 693134 {
		t.Fatalf("tmdb_id = %v，应为 693134", info["tmdb_id"])
	}
	ids, ok := info["genre_ids"].([]int)
	if !ok || len(ids) != 2 {
		t.Fatalf("genre_ids = %v", info["genre_ids"])
	}
}

func assertMediaInfoFrontendFields(t *testing.T, name string, info map[string]any) {
	t.Helper()
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("%s 序列化失败: %v", name, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s 反序列化失败: %v", name, err)
	}
	for _, key := range []string{
		"episode_run_time", "genres", "names", "season_info", "genre_ids",
		"origin_country", "production_countries", "production_companies",
		"spoken_languages", "networks", "actors", "directors", "created_by",
		"languages", "release_dates", "episode_groups",
	} {
		value, ok := decoded[key]
		if !ok {
			t.Fatalf("%s 缺字段 %q：前端会直接遍历，缺省即空白页", name, key)
		}
		if _, isArray := value.([]any); !isArray {
			t.Fatalf("%s 的 %q 必须是数组，实际 %T(%v)", name, key, value, value)
		}
	}
	for _, key := range []string{"runtime", "vote_average", "number_of_seasons", "number_of_episodes"} {
		value, ok := decoded[key]
		if !ok {
			t.Fatalf("%s 缺字段 %q", name, key)
		}
		if _, isNumber := value.(float64); !isNumber {
			t.Fatalf("%s 的 %q 必须是数值，实际 %T(%v)", name, key, value, value)
		}
	}
	// V2 `schemas.MediaInfo` 一共 58 个字段，少一个前端就可能读到 undefined。
	if len(decoded) != 58 {
		t.Fatalf("%s 字段数 = %d，应为 V2 的 58 个", name, len(decoded))
	}
}
