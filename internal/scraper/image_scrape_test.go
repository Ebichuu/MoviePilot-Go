package scraper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imageScrapeProvider 比 mockMetadataProvider 多一层：每种请求可独立失败，
// 并可充当 SeasonPosterProvider。
type imageScrapeProvider struct {
	movie       *MovieDetails
	tvShow      *TVShowDetails
	episode     *EpisodeDetails
	movieErr    error
	tvErr       error
	episodeErr  error
	seasonURL   string
	seasonErr   error
	episodeCall int
}

func (p *imageScrapeProvider) GetMovieDetails(context.Context, int) (*MovieDetails, error) {
	if p.movieErr != nil {
		return nil, p.movieErr
	}
	return p.movie, nil
}

func (p *imageScrapeProvider) GetTVShowDetails(context.Context, int) (*TVShowDetails, error) {
	if p.tvErr != nil {
		return nil, p.tvErr
	}
	return p.tvShow, nil
}

func (p *imageScrapeProvider) GetEpisodeDetails(context.Context, int, int, int) (*EpisodeDetails, error) {
	p.episodeCall++
	if p.episodeErr != nil {
		return nil, p.episodeErr
	}
	return p.episode, nil
}

func (p *imageScrapeProvider) GetSeasonPosterURL(context.Context, int, int) (string, error) {
	return p.seasonURL, p.seasonErr
}

// noSeasonProvider 只实现基础接口，用于验证季海报能力是可选扩口。
type noSeasonProvider struct{ episode *EpisodeDetails }

func (p *noSeasonProvider) GetMovieDetails(context.Context, int) (*MovieDetails, error) {
	return &MovieDetails{}, nil
}

func (p *noSeasonProvider) GetTVShowDetails(context.Context, int) (*TVShowDetails, error) {
	return &TVShowDetails{}, nil
}

func (p *noSeasonProvider) GetEpisodeDetails(context.Context, int, int, int) (*EpisodeDetails, error) {
	return p.episode, nil
}

const testImagePayload = "IMAGE"

func newScrapeImageServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "empty") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, testImagePayload)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestScrapeMovieImages(t *testing.T) {
	server := newScrapeImageServer(t)

	if err := NewScraperService(nil, NewNFOWriter(), nil).ScrapeMovieImages(t.Context(), 1, "/tmp/movie.mkv"); err == nil {
		t.Fatal("缺少 provider 应报错")
	}

	provider := &imageScrapeProvider{movieErr: errors.New("TMDB 不可用")}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeMovieImages(t.Context(), 1, "/tmp/movie.mkv"); err == nil ||
		!strings.Contains(err.Error(), "fetch movie details") {
		t.Fatalf("provider 错误 = %v", err)
	}

	directory := t.TempDir()
	mediaPath := filepath.Join(directory, "Movie.mkv")
	// 两个地址都为空时什么都不写。
	provider = &imageScrapeProvider{movie: &MovieDetails{}}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeMovieImages(t.Context(), 1, mediaPath); err != nil {
		t.Fatalf("空图片地址 = %v", err)
	}
	if entries, _ := os.ReadDir(directory); len(entries) != 0 {
		t.Fatalf("空图片地址不应产生文件: %v", entries)
	}

	// poster/backdrop 都写入，backdrop 额外生成 fanart 兼容别名。
	provider = &imageScrapeProvider{movie: &MovieDetails{
		PosterURL:   server.URL + "/poster.jpg",
		BackdropURL: server.URL + "/backdrop.jpg",
	}}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeMovieImages(t.Context(), 1, mediaPath); err != nil {
		t.Fatalf("图片刮削 = %v", err)
	}
	for _, name := range []string{"poster.jpg", "backdrop.jpg", "fanart.jpg"} {
		body, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil || string(body) != testImagePayload {
			t.Fatalf("%s = (%q, %v)", name, body, err)
		}
	}

	// 单张图片失败只记日志，不影响其它图片，也不返回错误。
	provider = &imageScrapeProvider{movie: &MovieDetails{
		PosterURL:   server.URL + "/missing.jpg",
		BackdropURL: server.URL + "/backdrop.png",
	}}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeMovieImages(t.Context(), 1, mediaPath); err != nil {
		t.Fatalf("部分图片失败 = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "backdrop.png")); err != nil {
		t.Fatalf("可用图片未写入: %v", err)
	}

	// 上下文取消时把 context 错误上抛，而不是当成图片失败。
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	provider = &imageScrapeProvider{movie: &MovieDetails{PosterURL: server.URL + "/missing.jpg"}}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeMovieImages(canceled, 1, mediaPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
}

func TestScrapeTVShowImages(t *testing.T) {
	server := newScrapeImageServer(t)
	if err := NewScraperService(nil, NewNFOWriter(), nil).ScrapeTVShowImages(t.Context(), 1, "/tmp/show.mkv"); err == nil {
		t.Fatal("缺少 provider 应报错")
	}

	provider := &imageScrapeProvider{tvErr: errors.New("boom")}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeTVShowImages(t.Context(), 1, "/tmp/show.mkv"); err == nil ||
		!strings.Contains(err.Error(), "fetch TV show details") {
		t.Fatalf("provider 错误 = %v", err)
	}

	directory := t.TempDir()
	provider = &imageScrapeProvider{tvShow: &TVShowDetails{
		PosterURL:   server.URL + "/poster.jpg",
		BackdropURL: "",
	}}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeTVShowImages(t.Context(), 1, filepath.Join(directory, "Show.mkv")); err != nil {
		t.Fatalf("剧集图片 = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "poster.jpg")); err != nil {
		t.Fatalf("海报未写入: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "backdrop.jpg")); err == nil {
		t.Fatal("空 backdrop 不应写文件")
	}

	// images 为 nil 时静默跳过。
	service := NewScraperService(provider, NewNFOWriter(), nil)
	service.images = nil
	if err := service.ScrapeTVShowImages(t.Context(), 1, filepath.Join(directory, "Show.mkv")); err != nil {
		t.Fatalf("无图片写入器 = %v", err)
	}
}

func TestScrapeEpisodeImages(t *testing.T) {
	server := newScrapeImageServer(t)

	if err := NewScraperService(nil, NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 1, 1, "/tmp/ep.mkv"); err == nil {
		t.Fatal("缺少 provider 应报错")
	}

	// images 为 nil 时连元数据都不查。
	provider := &imageScrapeProvider{episode: &EpisodeDetails{PosterURL: server.URL + "/episode.jpg"}}
	service := NewScraperService(provider, NewNFOWriter(), nil)
	service.images = nil
	if err := service.ScrapeEpisodeImages(t.Context(), 1, 1, 1, "/tmp/ep.mkv"); err != nil || provider.episodeCall != 0 {
		t.Fatalf("无图片写入器 = (%v, calls=%d)", err, provider.episodeCall)
	}

	// 单集剧照 + 季海报都写入。
	directory := t.TempDir()
	episodePath := filepath.Join(directory, "Season 01", "Show.S01E02.mkv")
	provider = &imageScrapeProvider{
		episode:   &EpisodeDetails{PosterURL: server.URL + "/episode.jpg"},
		seasonURL: server.URL + "/season.jpg",
	}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 1, 2, episodePath); err != nil {
		t.Fatalf("单集图片 = %v", err)
	}
	// 单集剧照与季目录 poster 落在季目录，seasonXX-poster 落在剧集根目录。
	for _, name := range []string{"Show.S01E02.jpg", "poster.jpg"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(episodePath), name)); err != nil {
			t.Fatalf("%s 未写入: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "season01-poster.jpg")); err != nil {
		t.Fatalf("season01-poster 未写入: %v", err)
	}

	// 单集元数据失败不影响季海报，且不返回错误。
	provider = &imageScrapeProvider{episodeErr: errors.New("TMDB 抖动"), seasonURL: server.URL + "/season.jpg"}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 1, 2, episodePath); err != nil {
		t.Fatalf("单集元数据失败 = %v", err)
	}

	// 单集元数据返回 nil 也继续查季海报。
	provider = &imageScrapeProvider{seasonURL: server.URL + "/season.jpg"}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 1, 2, episodePath); err != nil {
		t.Fatalf("空单集详情 = %v", err)
	}

	// 未实现季海报能力时直接返回。
	if err := NewScraperService(&noSeasonProvider{episode: &EpisodeDetails{PosterURL: server.URL + "/episode.jpg"}},
		NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 1, 2, episodePath); err != nil {
		t.Fatalf("无季海报能力 = %v", err)
	}

	// 季海报地址为空 / 查询失败（非 context）/ 写入失败都只记日志。
	for name, provider := range map[string]*imageScrapeProvider{
		"季海报为空":   {seasonURL: "  "},
		"季海报查询失败": {seasonURL: server.URL + "/season.jpg", seasonErr: errors.New("boom")},
	} {
		if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 1, 2, episodePath); err != nil {
			t.Fatalf("%s = %v", name, err)
		}
	}
	// 季目录与请求季不一致时 WriteSeasonImage 失败，同样只记日志。
	if err := NewScraperService(&imageScrapeProvider{seasonURL: server.URL + "/season.jpg"},
		NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 9, 2, episodePath); err != nil {
		t.Fatalf("季目录不匹配 = %v", err)
	}

	// context 错误必须上抛：元数据查询、单集图片下载、季海报查询三处。
	for name, provider := range map[string]*imageScrapeProvider{
		"元数据取消": {episodeErr: context.Canceled},
		"季海报超时": {seasonURL: server.URL + "/season.jpg", seasonErr: context.DeadlineExceeded},
	} {
		err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeEpisodeImages(t.Context(), 1, 1, 2, episodePath)
		if err == nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	// 单集图片下载失败时，错误里带 context 也会上抛。
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	provider = &imageScrapeProvider{episode: &EpisodeDetails{PosterURL: server.URL + "/missing.jpg"}}
	if err := NewScraperService(provider, NewNFOWriter(), nil).ScrapeEpisodeImages(canceled, 1, 1, 2, episodePath); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
}

func TestImageContextError(t *testing.T) {
	if err := imageContextError(nil, errors.New("boom")); err != nil {
		t.Fatalf("普通错误 = %v", err)
	}
	if err := imageContextError(context.Background(), nil); err != nil {
		t.Fatalf("nil 错误 = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := imageContextError(canceled, errors.New("boom")); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
	if err := imageContextError(context.Background(), context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Canceled 错误 = %v", err)
	}
	if err := imageContextError(context.Background(), context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeadlineExceeded 错误 = %v", err)
	}
}

func TestImageWriterDownloadBranches(t *testing.T) {
	server := newScrapeImageServer(t)
	writer := NewImageWriter()

	if _, err := writer.download(t.Context(), "http://[::1"); err == nil || !strings.Contains(err.Error(), "image request invalid") {
		t.Fatalf("非法地址 = %v", err)
	}
	if _, err := writer.download(t.Context(), server.URL+"/missing.jpg"); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("404 = %v", err)
	}
	if _, err := writer.download(t.Context(), server.URL+"/empty.jpg"); err == nil || !strings.Contains(err.Error(), "payload invalid") {
		t.Fatalf("空响应 = %v", err)
	}
	body, err := writer.download(t.Context(), server.URL+"/poster.jpg")
	if err != nil || string(body) != testImagePayload {
		t.Fatalf("正常下载 = (%q, %v)", body, err)
	}

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	if _, err := writer.download(t.Context(), closedURL); err == nil || !strings.Contains(err.Error(), "image request failed") {
		t.Fatalf("连接失败 = %v", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := writer.download(canceled, server.URL+"/poster.jpg"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
}

func TestWriteTargetsBranches(t *testing.T) {
	server := newScrapeImageServer(t)
	directory := t.TempDir()
	writer := NewImageWriter()
	sourceURL := server.URL + "/poster.jpg"
	mediaPath := filepath.Join(directory, "Movie.mkv")
	target := filepath.Join(directory, "poster.jpg")

	if err := writer.writeTargets(nil, mediaPath, sourceURL, []string{target}); err == nil {
		t.Fatal("nil ctx 应报错")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := writer.writeTargets(canceled, mediaPath, sourceURL, []string{target}); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", err)
	}
	var nilWriter *ImageWriter
	if err := nilWriter.writeTargets(t.Context(), mediaPath, sourceURL, []string{target}); err == nil {
		t.Fatal("nil 写入器应报错")
	}
	if err := (&ImageWriter{}).writeTargets(t.Context(), mediaPath, sourceURL, []string{target}); err == nil {
		t.Fatal("缺少 HTTP 客户端应报错")
	}
	if err := writer.writeTargets(t.Context(), "  ", sourceURL, []string{target}); err == nil {
		t.Fatal("空媒体路径应报错")
	}
	if err := writer.writeTargets(t.Context(), mediaPath, sourceURL, nil); err == nil {
		t.Fatal("空目标应报错")
	}

	// 路径校验失败时不下载。
	rejecting := NewImageWriter()
	rejecting.validatePath = func(string) error { return errors.New("拒绝") }
	if err := rejecting.writeTargets(t.Context(), mediaPath, sourceURL, []string{target}); err == nil ||
		!strings.Contains(err.Error(), "invalid image path") {
		t.Fatalf("路径校验 = %v", err)
	}

	// 目标已是目录时立即失败。
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("准备目录: %v", err)
	}
	if err := writer.writeTargets(t.Context(), mediaPath, sourceURL, []string{target}); err == nil ||
		!strings.Contains(err.Error(), "image target is a directory") {
		t.Fatalf("目标是目录 = %v", err)
	}
	_ = os.RemoveAll(target)

	// 已存在且不覆盖时跳过下载（源地址不可用也不报错）。
	if err := os.WriteFile(target, []byte("OLD"), 0644); err != nil {
		t.Fatalf("准备旧图片: %v", err)
	}
	if err := writer.writeTargets(t.Context(), mediaPath, server.URL+"/missing.jpg", []string{target}); err != nil {
		t.Fatalf("跳过已存在图片 = %v", err)
	}
	if body, _ := os.ReadFile(target); string(body) != "OLD" {
		t.Fatalf("旧图片被改写: %q", body)
	}

	// 覆盖模式会替换旧文件。
	overwriting := NewImageWriter()
	overwriting.Overwrite = true
	if err := overwriting.writeTargets(t.Context(), mediaPath, sourceURL, []string{target}); err != nil {
		t.Fatalf("覆盖写 = %v", err)
	}
	if body, _ := os.ReadFile(target); string(body) != testImagePayload {
		t.Fatalf("覆盖结果 = %q", body)
	}
}

func TestWriteImageAtomicBranches(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "poster.jpg")
	if err := writeImageAtomic(target, []byte("NEW")); err != nil {
		t.Fatalf("writeImageAtomic: %v", err)
	}
	if body, _ := os.ReadFile(target); string(body) != "NEW" {
		t.Fatalf("内容 = %q", body)
	}
	if err := writeImageAtomic(filepath.Join(directory, "missing-dir", "poster.jpg"), []byte("x")); err == nil {
		t.Fatal("目录不存在应报错")
	}

	noReplace := filepath.Join(directory, "backdrop.jpg")
	if err := writeImageAtomicNoReplace(noReplace, []byte("FIRST")); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	// 目标已存在：保留旧内容且不报错。
	if err := writeImageAtomicNoReplace(noReplace, []byte("SECOND")); err != nil {
		t.Fatalf("已存在时不覆盖: %v", err)
	}
	if body, _ := os.ReadFile(noReplace); string(body) != "FIRST" {
		t.Fatalf("旧内容被覆盖: %q", body)
	}
	// 目标是目录：明确的错误。
	asDirectory := filepath.Join(directory, "logo.jpg")
	if err := os.MkdirAll(asDirectory, 0755); err != nil {
		t.Fatalf("准备目录: %v", err)
	}
	if err := writeImageAtomicNoReplace(asDirectory, []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "image target is a directory") {
		t.Fatalf("目标是目录 = %v", err)
	}
}
