package metadata_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/scraper"
)

// TestNFOScrapingIntegration 演示完整的 NFO 刮削集成流程。
//
// 这个测试展示了如何将 TMDB 元数据适配器与 NFO 刮削服务集成：
// 1. 创建 TMDB 客户端
// 2. 创建 TMDB 元数据适配器
// 3. 创建 NFO 刮削服务
// 4. 为电影/剧集/单集生成 NFO 文件
//
// 测试使用本地 HTTP fixture，不访问真实 TMDB，也不读取生产凭据。
func TestNFOScrapingIntegration(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/3/movie/27205":
			if r.URL.Query().Get("append_to_response") != "credits" {
				t.Errorf("append_to_response = %q，期望 credits", r.URL.Query().Get("append_to_response"))
			}
			if err := json.NewEncoder(w).Encode(map[string]any{
				"id":             27205,
				"title":          "盗梦空间",
				"original_title": "Inception",
				"release_date":   "2010-07-16",
				"overview":       "一个小偷通过梦境分享技术窃取公司机密。",
				"runtime":        148,
				"vote_average":   8.8,
				"imdb_id":        "tt1375666",
				"genres":         []map[string]any{{"id": 878, "name": "科幻"}},
				"credits":        map[string]any{"cast": []any{}, "crew": []any{}},
			}); err != nil {
				t.Errorf("写入 fixture 响应失败: %v", err)
			}
		case "/3/movie/27205/images":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"posters": []any{}, "backdrops": []any{},
			}); err != nil {
				t.Errorf("写入图片 fixture 响应失败: %v", err)
			}
		default:
			t.Errorf("请求路径 = %q，期望 /3/movie/27205 或 /3/movie/27205/images", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	// 1. 创建共享 HTTP 客户端
	httpClient, err := metadata.NewHTTPClient(
		10,             // 并发数
		30*time.Second, // 超时
		10*1024*1024,   // 最大响应体 10MB
	)
	if err != nil {
		t.Fatalf("创建 HTTP 客户端失败: %v", err)
	}
	defer httpClient.CloseIdleConnections()

	// 2. 创建指向本地 fixture 的 TMDB 客户端
	tmdb := metadata.NewTMDB(
		httpClient,
		server.URL,
		"test-token",
	)

	// 3. 创建 TMDB 元数据适配器（实现 scraper.MetadataProvider 接口）
	metadataProvider := metadata.NewTMDBScraperAdapter(tmdb)

	// 4. 创建 NFO 写入器
	nfoWriter := scraper.NewNFOWriter()

	// 5. 创建 NFO 刮削服务
	scraperService := scraper.NewScraperService(
		metadataProvider,
		nfoWriter,
		slog.Default(),
	)

	// 6. 创建临时目录用于测试
	tempDir := t.TempDir()
	movieDir := filepath.Join(tempDir, "Movies", "Inception (2010)")
	if err := os.MkdirAll(movieDir, 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}

	// 7. 为电影生成 NFO
	moviePath := filepath.Join(movieDir, "Inception (2010).mkv")
	ctx := t.Context()

	t.Log("开始刮削电影 NFO...")
	if err := scraperService.ScrapeMovieNFO(ctx, 27205, moviePath); err != nil {
		t.Fatalf("刮削电影 NFO 失败: %v", err)
	}

	// 8. 验证 NFO 文件已生成
	nfoPath := filepath.Join(movieDir, "Inception (2010).nfo")
	nfoContent, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("读取 NFO 文件失败: %v", err)
	}
	for _, want := range []string{
		"<title>盗梦空间</title>",
		"<originaltitle>Inception</originaltitle>",
		`<uniqueid type="tmdb" default="true">27205</uniqueid>`,
	} {
		if !strings.Contains(string(nfoContent), want) {
			t.Errorf("NFO 内容缺少 %q", want)
		}
	}
	if got := requestCount.Load(); got != 2 {
		t.Errorf("TMDB fixture 调用次数 = %d，期望 2（详情 + 图片补全）", got)
	}
}

// Example 展示在生产环境中如何使用 NFO 刮削。
func Example() {
	// 1. 初始化依赖
	httpClient, err := metadata.NewHTTPClient(10, 30*time.Second, 10*1024*1024)
	if err != nil {
		slog.Error("初始化元数据 HTTP 客户端失败", "error", err)
		return
	}
	defer httpClient.CloseIdleConnections()

	tmdb := metadata.NewTMDB(
		httpClient,
		"https://api.themoviedb.org",
		os.Getenv("TMDB_API_TOKEN"),
	)

	metadataProvider := metadata.NewTMDBScraperAdapter(tmdb)
	nfoWriter := scraper.NewNFOWriter()
	scraperService := scraper.NewScraperService(metadataProvider, nfoWriter, slog.Default())

	// 2. 在整理流程完成后调用
	ctx := context.Background()

	// 电影刮削
	moviePath := "/media/Movies/Inception (2010)/Inception (2010).mkv"
	if err := scraperService.ScrapeMovieNFO(ctx, 27205, moviePath); err != nil {
		slog.Warn("电影 NFO 刮削失败", "error", err, "path", moviePath)
	}

	// 剧集刮削
	tvShowPath := "/media/TV Shows/Game of Thrones/tvshow.nfo"
	if err := scraperService.ScrapeTVShowNFO(ctx, 1399, tvShowPath); err != nil {
		slog.Warn("剧集 NFO 刮削失败", "error", err, "path", tvShowPath)
	}

	// 单集刮削
	episodePath := "/media/TV Shows/Game of Thrones/Season 01/S01E01.mkv"
	if err := scraperService.ScrapeEpisodeNFO(ctx, 1399, 1, 1, episodePath); err != nil {
		slog.Warn("单集 NFO 刮削失败", "error", err, "path", episodePath)
	}
}

// TestAdapterImplementsInterface 验证 TMDBScraperAdapter 实现了正确的接口。
func TestAdapterImplementsInterface(t *testing.T) {
	httpClient, err := metadata.NewHTTPClient(2, 10*time.Second, 1024*1024)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}
	t.Cleanup(httpClient.CloseIdleConnections)
	tmdb := metadata.NewTMDB(httpClient, "https://api.themoviedb.org", "token")
	adapter := metadata.NewTMDBScraperAdapter(tmdb)

	// 编译时检查：adapter 必须实现 scraper.MetadataProvider 接口
	var _ scraper.MetadataProvider = adapter

	if adapter == nil {
		t.Fatal("NewTMDBScraperAdapter() returned nil")
	}
}
