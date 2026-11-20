package metadata

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDoubanSearchAndDetail(t *testing.T) {
	signingMaterial := strings.Repeat("fixture", 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != doubanUserAgent {
			t.Errorf("豆瓣 User-Agent 未覆盖: %q", r.Header.Values("User-Agent"))
		}
		timestamp := r.URL.Query().Get("_ts")
		mac := hmac.New(sha1.New, []byte(signingMaterial))
		_, _ = mac.Write([]byte("GET&" + url.QueryEscape(r.URL.Path) + "&" + timestamp))
		if r.URL.Query().Get("_sig") != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
			t.Errorf("豆瓣签名错误: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/weixin":
			if r.URL.Query().Get("q") != "示例" || r.URL.Query().Get("apiKey") != "test-key" {
				t.Errorf("豆瓣搜索参数错误: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[
  {"type_name":"电影","target":{"id":1,"type":"movie","title":"示例电影","original_title":"Example Movie","year":"2026","cover_url":"https://img.test/movie.jpg","rating":{"value":7.4}}},
  {"type_name":"电视剧","target":{"id":"2","type":"tv","title":"示例剧集","year":2025}},
  {"type_name":"图书","target":{"id":"3","title":"忽略"}}
]}`))
		case "/movie/1":
			_, _ = w.Write([]byte(`{"id":"1","title":"示例电影","original_title":"Example Movie","year":"2026-08-10","intro":"简介","aka":["别名"],"pic":{"large":"https://img.test/large.jpg"},"rating":{"value":8.2}}`))
		case "/tv/2":
			_, _ = w.Write([]byte(`{"id":"2","type":"tv","title":"示例剧集","year":"2025","episodes_count":8}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	httpClient := newMetadataTestHTTPClient(t, 1<<20)
	client := NewDouban(httpClient, server.URL, "test-key", signingMaterial)
	client.now = func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }
	var _ Provider = client

	items, err := client.Search(context.Background(), "示例")
	if err != nil || len(items) != 2 {
		t.Fatalf("豆瓣搜索失败: items=%#v err=%v", items, err)
	}
	if items[0].ID != "1" || items[0].Type != "movie" || items[0].VoteAverage != 7.4 ||
		items[1].Type != "tv" || items[1].VoteAverage != 0 {
		t.Fatalf("豆瓣搜索映射错误: %#v", items)
	}
	detail, err := client.Detail(context.Background(), "movie:1")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Year != 2026 || detail.PosterURL != "https://img.test/large.jpg" || detail.VoteAverage != 8.2 ||
		detail.BackdropURL != "" || strings.Join(detail.Aliases, ",") != "别名,Example Movie" {
		t.Fatalf("豆瓣详情映射错误: %#v", detail)
	}
	tvDetail, err := client.Detail(context.Background(), "tv:2")
	if err != nil || tvDetail.EpisodeCount != 8 {
		t.Fatalf("豆瓣剧集总数映射错误: %#v err=%v", tvDetail, err)
	}
}

func TestBangumiSearchAndDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Bangumi token 缺失")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/search/subjects":
			if r.Method != http.MethodPost || r.URL.Query().Get("limit") != "20" {
				t.Errorf("Bangumi 搜索请求错误: %s %s", r.Method, r.URL.String())
			}
			var body struct {
				Keyword string `json:"keyword"`
				Filter  struct {
					Type []int `json:"type"`
				} `json:"filter"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Keyword != "动画" || len(body.Filter.Type) != 1 || body.Filter.Type[0] != 2 {
				t.Errorf("Bangumi 搜索体错误: %#v err=%v", body, err)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":42,"type":2,"name":"Example Anime","name_cn":"示例动画","date":"2026-01-02","summary":"简介","images":{"large":"https://img.test/bgm.jpg"},"rating":{"score":6.1}},{"id":9,"type":1,"name":"book"}]}`))
		case "/v0/subjects/42":
			_, _ = w.Write([]byte(`{"id":42,"type":2,"name":"Example Anime","name_cn":"示例动画","date":"2026-01-02","summary":"详情","total_episodes":12,"rating":{"score":7.3}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewBangumi(newMetadataTestHTTPClient(t, 1<<20), server.URL, "test-token")
	var _ Provider = client
	items, err := client.Search(context.Background(), "动画")
	if err != nil || len(items) != 1 || items[0].Title != "示例动画" ||
		items[0].OriginalTitle != "Example Anime" || items[0].VoteAverage != 6.1 {
		t.Fatalf("Bangumi 搜索映射错误: %#v err=%v", items, err)
	}
	detail, err := client.Detail(context.Background(), "42")
	if err != nil || detail.Year != 2026 || detail.ID != "42" ||
		detail.VoteAverage != 7.3 || detail.BackdropURL != "" || detail.EpisodeCount != 12 {
		t.Fatalf("Bangumi 详情错误: %#v err=%v", detail, err)
	}
}

func TestAniListSearchDetailAndGraphQLError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("解析 AniList 请求失败: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request.Variables["search"] == "error" {
			_, _ = w.Write([]byte(`{"errors":[{"message":"rate limited"}]}`))
			return
		}
		if _, searching := request.Variables["search"]; searching {
			if !strings.Contains(request.Query, "SEARCH_MATCH") {
				t.Errorf("AniList 搜索查询缺少 SEARCH_MATCH")
			}
			_, _ = w.Write([]byte(`{"data":{"Page":{"media":[{"id":100,"title":{"romaji":"Example Anime","english":"Example","native":"例"},"seasonYear":2026,"description":"简介","coverImage":{"extraLarge":"https://img.test/al.jpg"},"bannerImage":"https://img.test/al-backdrop.jpg","averageScore":87,"synonyms":["Alias"]}]}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"Media":{"id":100,"title":{"romaji":"Example Anime","native":"例"},"startDate":{"year":2025},"bannerImage":"https://img.test/al-detail-backdrop.jpg","averageScore":74,"episodes":24,"synonyms":[]}}}`))
	}))
	defer server.Close()

	client := NewAniList(newMetadataTestHTTPClient(t, 1<<20), server.URL)
	var _ Provider = client
	items, err := client.Search(context.Background(), "Example")
	if err != nil || len(items) != 1 {
		t.Fatalf("AniList 搜索失败: %#v err=%v", items, err)
	}
	if items[0].Title != "Example" || items[0].Year != 2026 || items[0].PosterURL == "" ||
		items[0].VoteAverage != 8.7 || items[0].BackdropURL != "https://img.test/al-backdrop.jpg" ||
		strings.Join(items[0].Aliases, ",") != "例,Example Anime,Alias" {
		t.Fatalf("AniList 搜索映射错误: %#v", items[0])
	}
	detail, err := client.Detail(context.Background(), "100")
	if err != nil || detail.Year != 2025 || detail.Title != "Example Anime" ||
		detail.VoteAverage != 7.4 || detail.BackdropURL != "https://img.test/al-detail-backdrop.jpg" ||
		detail.EpisodeCount != 24 {
		t.Fatalf("AniList 详情错误: %#v err=%v", detail, err)
	}
	if _, err := client.Search(context.Background(), "error"); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("AniList GraphQL 错误未传播: %v", err)
	}
}

func TestHTTPClientRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"` + strings.Repeat("x", 128) + `"}`))
	}))
	defer server.Close()

	client := newMetadataTestHTTPClient(t, 64)
	var target map[string]any
	err := client.DoJSON(context.Background(), http.MethodGet, server.URL, nil, nil, &target)
	if err == nil || !strings.Contains(err.Error(), "超过 64 字节") {
		t.Fatalf("超大响应未被拒绝: %v", err)
	}
}

func newMetadataTestHTTPClient(t *testing.T, maxBody int64) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(4, 2*time.Second, maxBody)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
