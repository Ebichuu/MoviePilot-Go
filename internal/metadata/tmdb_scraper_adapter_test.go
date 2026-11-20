package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/scraper"
)

func TestTMDBScraperAdapter_GetMovieDetails(t *testing.T) {
	// Mock TMDB API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/3/movie/27205") {
			t.Errorf("Unexpected path: %s", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "append_to_response=credits") {
			t.Errorf("Missing append_to_response parameter")
		}

		response := map[string]interface{}{
			"id":             27205,
			"title":          "盗梦空间",
			"original_title": "Inception",
			"release_date":   "2010-07-16",
			"overview":       "一个小偷通过梦境分享技术窃取公司机密。",
			"tagline":        "你的思维是犯罪现场。",
			"runtime":        148,
			"vote_average":   8.8,
			"poster_path":    "/poster.jpg",
			"backdrop_path":  "/backdrop.jpg",
			"imdb_id":        "tt1375666",
			"genres": []map[string]interface{}{
				{"id": 28, "name": "动作"},
				{"id": 878, "name": "科幻"},
			},
			"credits": map[string]interface{}{
				"cast": []map[string]interface{}{
					{
						"id":           6193,
						"name":         "莱昂纳多·迪卡普里奥",
						"character":    "Cobb",
						"profile_path": "/leo.jpg",
						"order":        0,
					},
					{
						"id":           24045,
						"name":         "约瑟夫·高登-莱维特",
						"character":    "Arthur",
						"profile_path": "/joseph.jpg",
						"order":        1,
					},
				},
				"crew": []map[string]interface{}{
					{
						"id":         525,
						"name":       "克里斯托弗·诺兰",
						"job":        "Director",
						"department": "Directing",
					},
					{
						"id":         525,
						"name":       "克里斯托弗·诺兰",
						"job":        "Screenplay",
						"department": "Writing",
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	httpClient, _ := NewHTTPClient(2, 10*time.Second, 1024*1024)
	tmdb := NewTMDB(httpClient, server.URL, "test-token")
	adapter := NewTMDBScraperAdapter(tmdb)

	ctx := context.Background()
	details, err := adapter.GetMovieDetails(ctx, 27205)
	if err != nil {
		t.Fatalf("GetMovieDetails failed: %v", err)
	}

	// 验证基本信息
	if details.Title != "盗梦空间" {
		t.Errorf("Title = %q, want %q", details.Title, "盗梦空间")
	}
	if details.OriginalTitle != "Inception" {
		t.Errorf("OriginalTitle = %q, want %q", details.OriginalTitle, "Inception")
	}
	if details.Year != 2010 {
		t.Errorf("Year = %d, want 2010", details.Year)
	}
	if details.Rating != 8.8 {
		t.Errorf("Rating = %f, want 8.8", details.Rating)
	}
	if details.Runtime != 148 {
		t.Errorf("Runtime = %d, want 148", details.Runtime)
	}
	if details.TMDBID != 27205 {
		t.Errorf("TMDBID = %d, want 27205", details.TMDBID)
	}
	if details.IMDBID != "tt1375666" {
		t.Errorf("IMDBID = %q, want %q", details.IMDBID, "tt1375666")
	}

	// 验证日期
	expectedDate := time.Date(2010, 7, 16, 0, 0, 0, 0, time.UTC)
	if !details.Premiered.Equal(expectedDate) {
		t.Errorf("Premiered = %v, want %v", details.Premiered, expectedDate)
	}

	// 验证类型
	if len(details.Genres) != 2 {
		t.Errorf("Genres count = %d, want 2", len(details.Genres))
	}
	if len(details.Genres) > 0 && details.Genres[0] != "动作" {
		t.Errorf("Genres[0] = %q, want %q", details.Genres[0], "动作")
	}

	// 验证导演
	if len(details.Directors) != 1 || details.Directors[0] != "克里斯托弗·诺兰" {
		t.Errorf("Directors = %v, want [克里斯托弗·诺兰]", details.Directors)
	}

	// 验证编剧
	if len(details.Writers) != 1 || details.Writers[0] != "克里斯托弗·诺兰" {
		t.Errorf("Writers = %v, want [克里斯托弗·诺兰]", details.Writers)
	}

	// 验证演员
	if len(details.Cast) != 2 {
		t.Errorf("Cast count = %d, want 2", len(details.Cast))
	}
	if len(details.Cast) > 0 {
		actor := details.Cast[0]
		if actor.Name != "莱昂纳多·迪卡普里奥" {
			t.Errorf("Cast[0].Name = %q, want %q", actor.Name, "莱昂纳多·迪卡普里奥")
		}
		if actor.Role != "Cobb" {
			t.Errorf("Cast[0].Role = %q, want %q", actor.Role, "Cobb")
		}
		if !strings.Contains(actor.ImageURL, "/leo.jpg") {
			t.Errorf("Cast[0].ImageURL = %q, want to contain %q", actor.ImageURL, "/leo.jpg")
		}
	}

	// 验证图片 URL
	if !strings.Contains(details.PosterURL, "/poster.jpg") {
		t.Errorf("PosterURL = %q, want to contain %q", details.PosterURL, "/poster.jpg")
	}
	if !strings.Contains(details.BackdropURL, "/backdrop.jpg") {
		t.Errorf("BackdropURL = %q, want to contain %q", details.BackdropURL, "/backdrop.jpg")
	}
}

func TestTMDBScraperAdapter_GetTVShowDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"id":             1399,
			"name":           "权力的游戏",
			"original_name":  "Game of Thrones",
			"first_air_date": "2011-04-17",
			"overview":       "九大家族争夺维斯特洛大陆的控制权。",
			"tagline":        "凛冬将至",
			"vote_average":   9.3,
			"poster_path":    "/poster.jpg",
			"backdrop_path":  "/backdrop.jpg",
			"genres": []map[string]interface{}{
				{"id": 18, "name": "剧情"},
				{"id": 10765, "name": "奇幻"},
			},
			"credits": map[string]interface{}{
				"cast": []map[string]interface{}{
					{
						"id":           1223786,
						"name":         "艾米莉亚·克拉克",
						"character":    "Daenerys Targaryen",
						"profile_path": "/emilia.jpg",
						"order":        0,
					},
				},
			},
			"external_ids": map[string]interface{}{
				"imdb_id": "tt0944947",
				"tvdb_id": 121361,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	httpClient, _ := NewHTTPClient(2, 10*time.Second, 1024*1024)
	tmdb := NewTMDB(httpClient, server.URL, "test-token")
	adapter := NewTMDBScraperAdapter(tmdb)

	ctx := context.Background()
	details, err := adapter.GetTVShowDetails(ctx, 1399)
	if err != nil {
		t.Fatalf("GetTVShowDetails failed: %v", err)
	}

	if details.Title != "权力的游戏" {
		t.Errorf("Title = %q, want %q", details.Title, "权力的游戏")
	}
	if details.TMDBID != 1399 {
		t.Errorf("TMDBID = %d, want 1399", details.TMDBID)
	}
	if details.TVDBID != 121361 {
		t.Errorf("TVDBID = %d, want 121361", details.TVDBID)
	}
	if details.IMDBID != "tt0944947" {
		t.Errorf("IMDBID = %q, want %q", details.IMDBID, "tt0944947")
	}
	if len(details.Cast) != 1 {
		t.Errorf("Cast count = %d, want 1", len(details.Cast))
	}
}

func TestTMDBScraperAdapter_GetEpisodeDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/season/1/episode/1") {
			t.Errorf("Unexpected path: %s", r.URL.Path)
		}

		response := map[string]interface{}{
			"id":           63056,
			"name":         "凛冬将至",
			"overview":     "艾德·史塔克在家庭和老朋友之间左右为难。",
			"air_date":     "2011-04-17",
			"runtime":      62,
			"vote_average": 8.9,
			"still_path":   "/still.jpg",
			"credits": map[string]interface{}{
				"cast": []map[string]interface{}{
					{
						"id":           3967,
						"name":         "肖恩·宾",
						"character":    "Eddard Stark",
						"profile_path": "/sean.jpg",
						"order":        0,
					},
				},
				"crew": []map[string]interface{}{
					{
						"id":         1223202,
						"name":       "蒂姆·范·帕顿",
						"job":        "Director",
						"department": "Directing",
					},
					{
						"id":         1222585,
						"name":       "大卫·贝尼奥夫",
						"job":        "Writer",
						"department": "Writing",
					},
				},
			},
			"external_ids": map[string]interface{}{
				"tvdb_id": 3254641,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	httpClient, _ := NewHTTPClient(2, 10*time.Second, 1024*1024)
	tmdb := NewTMDB(httpClient, server.URL, "test-token")
	adapter := NewTMDBScraperAdapter(tmdb)

	ctx := context.Background()
	details, err := adapter.GetEpisodeDetails(ctx, 1399, 1, 1)
	if err != nil {
		t.Fatalf("GetEpisodeDetails failed: %v", err)
	}

	if details.Title != "凛冬将至" {
		t.Errorf("Title = %q, want %q", details.Title, "凛冬将至")
	}
	if details.Season != 1 {
		t.Errorf("Season = %d, want 1", details.Season)
	}
	if details.Episode != 1 {
		t.Errorf("Episode = %d, want 1", details.Episode)
	}
	if details.Runtime != 62 {
		t.Errorf("Runtime = %d, want 62", details.Runtime)
	}
	if details.TMDBID != 63056 {
		t.Errorf("TMDBID = %d, want 63056", details.TMDBID)
	}
	if details.TVDBID != 3254641 {
		t.Errorf("TVDBID = %d, want 3254641", details.TVDBID)
	}

	if len(details.Directors) != 1 || details.Directors[0] != "蒂姆·范·帕顿" {
		t.Errorf("Directors = %v, want [蒂姆·范·帕顿]", details.Directors)
	}
	if len(details.Writers) != 1 || details.Writers[0] != "大卫·贝尼奥夫" {
		t.Errorf("Writers = %v, want [大卫·贝尼奥夫]", details.Writers)
	}
	if len(details.Cast) != 1 {
		t.Errorf("Cast count = %d, want 1", len(details.Cast))
	}
}

func TestTMDBScraperAdapter_NilClient(t *testing.T) {
	adapter := NewTMDBScraperAdapter(nil)
	ctx := context.Background()

	_, err := adapter.GetMovieDetails(ctx, 123)
	if err == nil {
		t.Error("Expected error for nil client, got nil")
	}

	_, err = adapter.GetTVShowDetails(ctx, 123)
	if err == nil {
		t.Error("Expected error for nil client, got nil")
	}

	_, err = adapter.GetEpisodeDetails(ctx, 123, 1, 1)
	if err == nil {
		t.Error("Expected error for nil client, got nil")
	}
}

func TestTMDBScraperAdapter_Interface(t *testing.T) {
	// 验证 TMDBScraperAdapter 实现了 scraper.MetadataProvider 接口
	var _ scraper.MetadataProvider = (*TMDBScraperAdapter)(nil)
}
