package scrapeflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

type imageScrapeFake struct {
	scrapeFake
	nfoCalls      int
	movieImages   int
	tvImages      int
	episodeImages int
	imageErr      error
}

func (f *imageScrapeFake) ScrapeMovieNFO(context.Context, int, string) error {
	f.nfoCalls++
	return f.err
}

func (f *imageScrapeFake) ScrapeMovieImages(context.Context, int, string) error {
	f.movieImages++
	return f.imageErr
}

func (f *imageScrapeFake) ScrapeTVShowImages(context.Context, int, string) error {
	f.tvImages++
	return f.imageErr
}

func (f *imageScrapeFake) ScrapeEpisodeImages(context.Context, int, int, int, string) error {
	f.episodeImages++
	return f.imageErr
}

func TestServiceExecute(t *testing.T) {
	newService := func(t *testing.T, scraper Scraper) *Service {
		t.Helper()
		service, err := NewService(&scrapeStoreFake{}, scraper)
		if err != nil {
			t.Fatalf("NewService(): %v", err)
		}
		return service
	}
	request := func(kind Kind) Request {
		return Request{
			ID: uuid.New(), Kind: kind, MediaPath: testAbsolutePath(t, "x.mkv"),
			TMDBID: 42, Season: 1, Episode: 2,
		}
	}

	t.Run("NFO 失败直接上抛", func(t *testing.T) {
		scraper := &imageScrapeFake{scrapeFake: scrapeFake{err: errors.New("tmdb down")}}
		err := newService(t, scraper).execute(t.Context(), request(KindMovie))
		if err == nil || err.Error() != "tmdb down" {
			t.Fatalf("execute() = %v", err)
		}
		if scraper.movieImages != 0 {
			t.Fatalf("NFO 失败后仍写图片: %d", scraper.movieImages)
		}
	})

	t.Run("三类型成功并调用对应图片刮削", func(t *testing.T) {
		cases := []struct {
			kind  Kind
			check func(*imageScrapeFake) int
		}{
			{KindMovie, func(f *imageScrapeFake) int { return f.movieImages }},
			{KindTVShow, func(f *imageScrapeFake) int { return f.tvImages }},
			{KindEpisode, func(f *imageScrapeFake) int { return f.episodeImages }},
		}
		for _, testCase := range cases {
			scraper := &imageScrapeFake{}
			if err := newService(t, scraper).execute(t.Context(), request(testCase.kind)); err != nil {
				t.Fatalf("%v execute() = %v", testCase.kind, err)
			}
			if got := testCase.check(scraper); got != 1 {
				t.Fatalf("%v 图片刮削调用 = %d, want 1", testCase.kind, got)
			}
		}
	})

	t.Run("无图片能力时只写 NFO", func(t *testing.T) {
		scraper := &scrapeFake{}
		service, err := NewService(&scrapeStoreFake{}, scraper)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []Kind{KindMovie, KindTVShow, KindEpisode} {
			if err := service.execute(t.Context(), request(kind)); err != nil {
				t.Fatalf("%v execute() = %v", kind, err)
			}
		}
	})

	t.Run("图片刮削的上下文取消上抛", func(t *testing.T) {
		scraper := &imageScrapeFake{imageErr: context.Canceled}
		for _, kind := range []Kind{KindMovie, KindTVShow, KindEpisode} {
			err := newService(t, scraper).execute(t.Context(), request(kind))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%v execute() = %v, want context.Canceled", kind, err)
			}
		}
	})

	t.Run("已取消的上下文在 NFO 后短路", func(t *testing.T) {
		scraper := &imageScrapeFake{}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := newService(t, scraper).execute(ctx, request(KindMovie))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("execute() = %v, want context.Canceled", err)
		}
		if scraper.movieImages != 0 {
			t.Fatalf("取消后仍写图片: %d", scraper.movieImages)
		}
	})

	t.Run("未知类型", func(t *testing.T) {
		err := newService(t, &imageScrapeFake{}).execute(t.Context(), request(Kind("unknown")))
		if !errors.Is(err, ErrExecution) {
			t.Fatalf("execute() = %v, want %v", err, ErrExecution)
		}
	})
}

func TestStableContextError(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if got := stableContextError(canceled, nil); !errors.Is(got, context.Canceled) {
		t.Fatalf("已取消上下文 = %v", got)
	}
	if got := stableContextError(context.Background(), context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("Canceled 错误 = %v", got)
	}
	if got := stableContextError(context.Background(), context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("DeadlineExceeded 错误 = %v", got)
	}
	if got := stableContextError(nil, context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("nil 上下文 + Canceled = %v", got)
	}
	if got := stableContextError(context.Background(), errors.New("boom")); got != nil {
		t.Fatalf("普通错误 = %v, want nil", got)
	}
	if got := stableContextError(nil, nil); got != nil {
		t.Fatalf("全空 = %v, want nil", got)
	}
}

type sweepStoreFake struct {
	ids        []uuid.UUID
	recoverErr error
	listErr    error
}

func (f *sweepStoreFake) RecoverExpiredScrapes(context.Context, time.Time, int) (int64, error) {
	return 0, f.recoverErr
}

func (f *sweepStoreFake) ListDueScrapes(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return f.ids, f.listErr
}

type sweepEnqueuerFake struct{ err error }

func (f sweepEnqueuerFake) EnqueueScrape(context.Context, uuid.UUID) error { return f.err }

func TestSweepHandlerHandle(t *testing.T) {
	now := time.Date(2026, 8, 12, 19, 0, 0, 0, time.UTC)
	newHandler := func(t *testing.T, store SweepStore, enqueuer Enqueuer) *SweepHandler {
		t.Helper()
		handler, err := NewSweepHandler(store, enqueuer)
		if err != nil {
			t.Fatalf("NewSweepHandler(): %v", err)
		}
		handler.now = func() time.Time { return now }
		return handler
	}

	if err := (*SweepHandler)(nil).Handle(t.Context()); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil handler = %v", err)
	}
	handler := newHandler(t, &sweepStoreFake{}, sweepEnqueuerFake{})
	if err := handler.Handle(nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil ctx = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := handler.Handle(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 ctx = %v", err)
	}

	recoverFailure := newHandler(t, &sweepStoreFake{recoverErr: errors.New("boom")}, sweepEnqueuerFake{})
	if err := recoverFailure.Handle(t.Context()); !errors.Is(err, ErrPersistence) {
		t.Fatalf("回收失败 = %v", err)
	}
	listFailure := newHandler(t, &sweepStoreFake{listErr: errors.New("boom")}, sweepEnqueuerFake{})
	if err := listFailure.Handle(t.Context()); !errors.Is(err, ErrPersistence) {
		t.Fatalf("列表失败 = %v", err)
	}
	oversized := make([]uuid.UUID, SweepPageSize+1)
	for index := range oversized {
		oversized[index] = uuid.New()
	}
	if err := newHandler(t, &sweepStoreFake{ids: oversized}, sweepEnqueuerFake{}).Handle(t.Context()); !errors.Is(err, ErrPersistence) {
		t.Fatalf("超页 = %v", err)
	}
	if err := newHandler(t, &sweepStoreFake{ids: []uuid.UUID{uuid.Nil}}, sweepEnqueuerFake{}).Handle(t.Context()); !errors.Is(err, ErrPersistence) {
		t.Fatalf("零 UUID = %v", err)
	}
	if err := newHandler(t, &sweepStoreFake{ids: []uuid.UUID{uuid.New()}}, sweepEnqueuerFake{err: errors.New("boom")}).Handle(t.Context()); !errors.Is(err, ErrPersistence) {
		t.Fatalf("入队失败 = %v", err)
	}
	if err := newHandler(t, &sweepStoreFake{ids: []uuid.UUID{uuid.New()}}, sweepEnqueuerFake{err: context.Canceled}).Handle(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("入队取消 = %v", err)
	}
	if err := newHandler(t, &sweepStoreFake{ids: []uuid.UUID{uuid.New(), uuid.New()}}, sweepEnqueuerFake{}).Handle(t.Context()); err != nil {
		t.Fatalf("正常回收 = %v", err)
	}
}

func TestTVShowNFOPath(t *testing.T) {
	library := filepath.Join(t.TempDir(), "library")
	show := filepath.Join(library, "示例剧 (2026)")
	videos := func(parts ...string) []string {
		result := make([]string, 0, len(parts))
		result = append(result, parts...)
		return result
	}

	cases := []struct {
		name   string
		videos []string
		want   string
	}{
		{
			name:   "单季目录回退到剧集根",
			videos: videos(filepath.Join(show, "Season 1", "ep1.mkv")),
			want:   filepath.Join(show, "tvshow.nfo"),
		},
		{
			name: "多季共用剧集根",
			videos: videos(
				filepath.Join(show, "Season 1", "ep1.mkv"),
				filepath.Join(show, "Season 2", "ep1.mkv"),
			),
			want: filepath.Join(show, "tvshow.nfo"),
		},
		{
			name:   "中文季目录",
			videos: videos(filepath.Join(show, "第2季", "ep1.mkv")),
			want:   filepath.Join(show, "tvshow.nfo"),
		},
		{
			name:   "特别季目录",
			videos: videos(filepath.Join(show, "Specials", "ep1.mkv")),
			want:   filepath.Join(show, "tvshow.nfo"),
		},
		{
			name:   "根目录即视频所在目录",
			videos: videos(filepath.Join(show, "ep1.mkv")),
			want:   filepath.Join(show, "tvshow.nfo"),
		},
		{
			name: "两季不同剧集回退到公共父目录",
			videos: videos(
				filepath.Join(library, "剧集A", "Season 1", "ep1.mkv"),
				filepath.Join(library, "剧集B", "Season 1", "ep1.mkv"),
			),
			want: filepath.Join(library, "tvshow.nfo"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := tvShowNFOPath(testCase.videos); got != testCase.want {
				t.Fatalf("tvShowNFOPath() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestIsSeasonDirectory(t *testing.T) {
	valid := []string{"Season 1", "season.02", "S01", "s 3", "第1季", "Special", "specials"}
	for _, value := range valid {
		if !isSeasonDirectory(value) {
			t.Fatalf("isSeasonDirectory(%q) = false", value)
		}
	}
	invalid := []string{"", "Specials 2", "第季", "Season", "S", "示例剧 (2026)"}
	for _, value := range invalid {
		if isSeasonDirectory(value) {
			t.Fatalf("isSeasonDirectory(%q) = true", value)
		}
	}
}

func TestMapEpisode(t *testing.T) {
	bound := episodeSet([]Episode{{Season: 1, Episode: 5}, {Season: 2, Episode: 3}})

	season, episode, found := mapEpisode("Show.S01E05.mkv", 0, nil, 1, bound)
	if season != 1 || episode != 5 || !found {
		t.Fatalf("解析季集 = (%d, %d, %v)", season, episode, found)
	}
	if _, _, found := mapEpisode("Show.S01E06.mkv", 0, nil, 1, bound); found {
		t.Fatal("未绑定的季集不应命中")
	}
	if season, episode, found := mapEpisode("Show.S02E03.mkv", 1, nil, 1, bound); season != 2 || episode != 3 || !found {
		t.Fatalf("跨季解析 = (%d, %d, %v)", season, episode, found)
	}
	// 文件名只有集号（第 N 集）时使用来源季，缺省季回落到 1。
	if season, episode, found := mapEpisode("Show 第5集.mkv", 1, nil, 1, bound); season != 1 || episode != 5 || !found {
		t.Fatalf("来源季回填 = (%d, %d, %v)", season, episode, found)
	}
	firstSeason := episodeSet([]Episode{{Season: 1, Episode: 4}})
	if season, episode, found := mapEpisode("Show 第4集.mkv", 0, nil, 1, firstSeason); season != 1 || episode != 4 || !found {
		t.Fatalf("缺省季 = (%d, %d, %v)", season, episode, found)
	}
	// 多集范围不按单集绑定。
	if _, _, found := mapEpisode("Show.S01E01-E02.mkv", 0, nil, 1, bound); found {
		t.Fatal("多集范围不应直接命中")
	}
	// 单视频 + 单集时按订阅集回填。
	episodes := []Episode{{Season: 2, Episode: 3}}
	if season, episode, found := mapEpisode("Show.mkv", 0, episodes, 1, bound); season != 2 || episode != 3 || !found {
		t.Fatalf("单集回填 = (%d, %d, %v)", season, episode, found)
	}
	if _, _, found := mapEpisode("Show.mkv", 0, []Episode{{Season: -1, Episode: 3}}, 1, bound); found {
		t.Fatal("负季号的单集不应命中")
	}
	// 季号 0（特别季）是合法取值。
	zeroSeason := map[Episode]struct{}{{Season: 0, Episode: 3}: {}}
	if season, episode, found := mapEpisode("Show.mkv", 0, []Episode{{Season: 0, Episode: 3}}, 1, zeroSeason); season != 0 || episode != 3 || !found {
		t.Fatalf("特别季回填 = (%d, %d, %v)", season, episode, found)
	}
	if _, _, found := mapEpisode("Show.mkv", 0, episodes, 2, bound); found {
		t.Fatal("多视频不应回填单集")
	}
}
