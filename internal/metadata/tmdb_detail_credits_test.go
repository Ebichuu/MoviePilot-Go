package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTMDBDetailCarriesCreditsAndGenres 锁住详情页依赖的富字段。
//
// 2026-11-20 用户反馈「TMDB 这些也没显示」：详情接口只回了标题/海报，演员、导演、
// 类型、片长全是空——前端就不渲染这些区块。这里确认 append_to_response=credits
// 的结果真的落到了 Media 上，且 crew 里只认 job == "Director"。
func TestTMDBDetailCarriesCreditsAndGenres(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "append_to_response=credits") {
			t.Errorf("详情请求缺少 append_to_response=credits: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{
			"id": 603,
			"title": "黑客帝国",
			"original_title": "The Matrix",
			"original_language": "en",
			"release_date": "1999-03-31",
			"overview": "概述",
			"poster_path": "/p.jpg",
			"backdrop_path": "/b.jpg",
			"vote_average": 8.2,
			"vote_count": 25000,
			"popularity": 80.5,
			"runtime": 136,
			"genres": [{"id": 28, "name": "动作"}, {"id": 878, "name": "科幻"}],
			"production_countries": [{"iso_3166_1": "US"}],
			"credits": {
				"cast": [
					{"id": 6384, "name": "基努·里维斯", "character": "Neo", "profile_path": "/k.jpg"}
				],
				"crew": [
					{"id": 9330, "name": "沃卓斯基姐妹", "job": "Director", "profile_path": "/w.jpg"},
					{"id": 9331, "name": "某制片", "job": "Producer", "profile_path": ""}
				]
			}
		}`))
	}))
	defer server.Close()

	tmdb := NewTMDB(newMetadataTestHTTPClient(t, 1<<20), server.URL, "test-token")
	item, err := tmdb.Detail(context.Background(), "movie:603")
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if item.Runtime != 136 || item.VoteCount != 25000 || item.Popularity != 80.5 {
		t.Fatalf("片长/票数/热度 = %d/%d/%v", item.Runtime, item.VoteCount, item.Popularity)
	}
	if item.ReleaseDate != "1999-03-31" {
		t.Fatalf("release_date = %q", item.ReleaseDate)
	}
	if len(item.Genres) != 2 || item.Genres[0] != "动作" || item.Genres[1] != "科幻" {
		t.Fatalf("类型 = %#v", item.Genres)
	}
	if len(item.Actors) != 1 {
		t.Fatalf("演员 = %#v", item.Actors)
	}
	actor := item.Actors[0]
	if actor.Name != "基努·里维斯" || actor.Character != "Neo" || actor.Source != "themoviedb" {
		t.Fatalf("演员字段 = %#v", actor)
	}
	// profile_path 保持 TMDB 的相对路径：前端按 TMDB_IMAGE_DOMAIN 拼完整 URL（V2 同样透传）。
	if actor.ProfilePath != "/k.jpg" {
		t.Fatalf("演员头像应为相对路径，实际 %q", actor.ProfilePath)
	}
	if len(item.Directors) != 1 || item.Directors[0].Name != "沃卓斯基姐妹" {
		t.Fatalf("导演应只包含 Director，实际 %#v", item.Directors)
	}
}

// TestTMDBDetailTVEpisodeRunTime 确认剧集的单集时长也带出来（详情页用它显示片长）。
func TestTMDBDetailTVEpisodeRunTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"id": 1399, "name": "权力的游戏", "original_name": "Game of Thrones",
			"first_air_date": "2011-04-17", "episode_run_time": [60],
			"number_of_seasons": 8, "number_of_episodes": 73,
			"genres": [{"id": 18, "name": "剧情"}], "credits": {"cast": [], "crew": []}
		}`))
	}))
	defer server.Close()

	tmdb := NewTMDB(newMetadataTestHTTPClient(t, 1<<20), server.URL, "test-token")
	item, err := tmdb.Detail(context.Background(), "tv:1399")
	if err != nil {
		t.Fatalf("Detail() error = %v", err)
	}
	if len(item.EpisodeRunTime) != 1 || item.EpisodeRunTime[0] != 60 {
		t.Fatalf("episode_run_time = %#v", item.EpisodeRunTime)
	}
	if item.Type != "tv" || item.Title != "权力的游戏" || item.Year != 2011 {
		t.Fatalf("剧集基础字段 = %+v", item)
	}
	if item.NumberOfSeasons != 8 || item.NumberOfEpisodes != 73 {
		t.Fatalf("季集数 = %d 季 %d 集", item.NumberOfSeasons, item.NumberOfEpisodes)
	}
}
