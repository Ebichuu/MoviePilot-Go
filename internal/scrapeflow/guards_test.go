package scrapeflow

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

func TestConstructorsRejectNilDependencies(t *testing.T) {
	if _, err := NewPlanner(nil, false); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("NewPlanner(nil) = %v", err)
	}
	if _, err := NewService(nil, &scrapeFake{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("NewService(nil store) = %v", err)
	}
	if _, err := NewService(&scrapeStoreFake{}, nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("NewService(nil scraper) = %v", err)
	}
	if _, err := NewSweepHandler(nil, sweepEnqueuerFake{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("NewSweepHandler(nil store) = %v", err)
	}
	if _, err := NewSweepHandler(&sweepStoreFake{}, nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("NewSweepHandler(nil enqueuer) = %v", err)
	}
}

func TestValidKind(t *testing.T) {
	for _, kind := range []Kind{KindMovie, KindTVShow, KindEpisode, KindUnknown} {
		if !validKind(kind) {
			t.Fatalf("validKind(%q) = false", kind)
		}
	}
	for _, kind := range []Kind{"", "MOVIE", "tv", "ep", "  movie"} {
		if validKind(kind) {
			t.Fatalf("validKind(%q) = true", kind)
		}
	}
}

func TestRequestPlanValid(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "movie.mkv")
	base := func() RequestPlan {
		return RequestPlan{
			ID: uuid.New(), Kind: KindMovie, MediaPath: absolute, TMDBID: 42,
		}
	}
	if !base().Valid() {
		t.Fatal("基准请求应有效")
	}

	cases := map[string]func(*RequestPlan){
		"缺 ID":       func(r *RequestPlan) { r.ID = uuid.Nil },
		"未知类型":       func(r *RequestPlan) { r.Kind = "unknown" },
		"路径为空":       func(r *RequestPlan) { r.MediaPath = "" },
		"路径有空格":      func(r *RequestPlan) { r.MediaPath = " " + absolute },
		"相对路径":       func(r *RequestPlan) { r.MediaPath = "movie.mkv" },
		"路径含 NUL":    func(r *RequestPlan) { r.MediaPath = absolute + "\x00" },
		"路径过长":       func(r *RequestPlan) { r.MediaPath = absolute + strings.Repeat("a", 4096) },
		"季号越界":       func(r *RequestPlan) { r.Season = 1000 },
		"集号越界":       func(r *RequestPlan) { r.Episode = 5001 },
		"缺 TMDB ID":  func(r *RequestPlan) { r.TMDBID = 0 },
		"跳过原因过长":     func(r *RequestPlan) { r.SkipReason = strings.Repeat("x", 129) },
		"跳过原因带 TMDB": func(r *RequestPlan) { r.SkipReason = "scraping_disabled"; r.TMDBID = 42 },
		"电影带季集":      func(r *RequestPlan) { r.Season, r.Episode = 1, 1 },
		"剧集缺集号":      func(r *RequestPlan) { r.Kind = KindEpisode; r.Episode = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := base()
			mutate(&request)
			if request.Valid() {
				t.Fatalf("%s 应无效: %#v", name, request)
			}
		})
	}

	skipped := base()
	skipped.SkipReason, skipped.TMDBID = "scraping_disabled", 0
	if !skipped.Valid() {
		t.Fatal("带跳过原因且无 TMDB ID 的请求应有效")
	}
	episode := base()
	episode.Kind, episode.Season, episode.Episode = KindEpisode, 2, 3
	if !episode.Valid() {
		t.Fatal("剧集请求应有效")
	}
	special := base()
	special.Kind, special.Season, special.Episode = KindEpisode, 0, 1
	if !special.Valid() {
		t.Fatal("特别季剧集请求应有效")
	}
}

func TestPlanValid(t *testing.T) {
	newPlan := func() Plan {
		return Plan{
			BatchID:  uuid.New(),
			IntentID: uuid.New(),
			Username: "alice",
			Requests: []RequestPlan{{
				ID: uuid.New(), Kind: KindMovie,
				MediaPath: filepath.Join(t.TempDir(), "movie.mkv"), TMDBID: 42,
			}},
		}
	}
	if !newPlan().Valid() {
		t.Fatal("基准计划应有效")
	}

	cases := map[string]func(*Plan){
		"缺批次":    func(p *Plan) { p.BatchID = uuid.Nil },
		"缺意图":    func(p *Plan) { p.IntentID = uuid.Nil },
		"空用户名":   func(p *Plan) { p.Username = "" },
		"用户名带空格": func(p *Plan) { p.Username = " alice " },
		"重复请求":   func(p *Plan) { p.Requests = append(p.Requests, p.Requests[0]) },
		"请求无效":   func(p *Plan) { p.Requests[0].TMDBID = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			plan := newPlan()
			mutate(&plan)
			if plan.Valid() {
				t.Fatalf("%s 应无效", name)
			}
		})
	}

	oversized := newPlan()
	oversized.Requests = make([]RequestPlan, 10_001)
	if oversized.Valid() {
		t.Fatal("超过 10000 条请求应无效")
	}

	empty := newPlan()
	empty.Requests = nil
	if !empty.Valid() {
		t.Fatal("空请求列表应有效")
	}
}

func TestPlannerSkipReason(t *testing.T) {
	disabled, err := NewPlanner(&sourceStoreFake{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := disabled.skipReason(Source{}); got != "scraping_disabled" {
		t.Fatalf("停用跳过原因 = %q", got)
	}
	enabled, err := NewPlanner(&sourceStoreFake{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := enabled.skipReason(Source{}); got != "tmdb_identity_unavailable" {
		t.Fatalf("缺身份跳过原因 = %q", got)
	}
}

func TestNormalizeMediaType(t *testing.T) {
	cases := map[string]media.MediaType{
		"电影":     media.TypeMovie,
		"movie":  media.TypeMovie,
		" 电影 ":   media.TypeMovie,
		"电视剧":    media.TypeTV,
		"tv":     media.TypeTV,
		"Series": media.TypeTV,
		"anime":  media.TypeUnknown,
		"":       media.TypeUnknown,
	}
	for value, want := range cases {
		if got := normalizeMediaType(value); got != want {
			t.Fatalf("normalizeMediaType(%q) = %v, want %v", value, got, want)
		}
	}
}

// TestServiceHandleGuards 覆盖 Handle 的入参守卫与租约校验分支。
func TestServiceHandleGuards(t *testing.T) {
	service, err := NewService(&scrapeStoreFake{}, &scrapeFake{})
	if err != nil {
		t.Fatal(err)
	}
	if err := (*Service)(nil).Handle(t.Context(), uuid.New()); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil service = %v", err)
	}
	if err := service.Handle(nil, uuid.New()); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("nil ctx = %v", err)
	}
	if err := service.Handle(t.Context(), uuid.Nil); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("零 UUID = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := service.Handle(canceled, uuid.New()); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 ctx = %v", err)
	}

	// 未抢到租约时直接返回，不落任何状态。
	unclaimed, err := NewService(&scrapeStoreFake{}, &scrapeFake{})
	if err != nil {
		t.Fatal(err)
	}
	unclaimed.now = func() time.Time { return time.Now() }
	if err := unclaimed.Handle(t.Context(), uuid.New()); err != nil {
		t.Fatalf("未抢到租约 = %v", err)
	}

	// 租约与请求 ID 不一致时按持久化失败处理，不执行刮削。
	id := uuid.New()
	mismatched := &guardedStoreFake{lease: Lease{Token: uuid.New()}, claimed: true}
	service2, err := NewService(mismatched, &scrapeFake{})
	if err != nil {
		t.Fatal(err)
	}
	service2.now = func() time.Time { return time.Now() }
	if err := service2.Handle(t.Context(), id); !errors.Is(err, ErrPersistence) {
		t.Fatalf("租约不匹配 = %v", err)
	}

	// 完成写回失败。
	broken := &guardedStoreFake{
		lease:   Lease{Token: uuid.New(), Request: Request{ID: id, Kind: KindMovie, MediaPath: "/lib/movie.mkv", TMDBID: 1}},
		claimed: true,
	}
	service3, err := NewService(broken, &scrapeFake{})
	if err != nil {
		t.Fatal(err)
	}
	service3.now = func() time.Time { return time.Now() }
	broken.completeErr = io.ErrClosedPipe
	if err := service3.Handle(t.Context(), id); !errors.Is(err, ErrPersistence) {
		t.Fatalf("完成失败 = %v", err)
	}
}

type guardedStoreFake struct {
	lease         Lease
	claimed       bool
	claimErr      error
	completeErr   error
	rescheduleErr error
}

func (s *guardedStoreFake) ClaimScrape(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error) {
	return s.lease, s.claimed, s.claimErr
}

func (s *guardedStoreFake) CompleteScrape(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	return s.completeErr
}

func (s *guardedStoreFake) RescheduleScrape(context.Context, uuid.UUID, uuid.UUID, time.Time, string) error {
	return s.rescheduleErr
}
