package subscriptionflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
)

func TestSubscriptionMediaFactsPreservesPrimaryIdentityAndEpisodeFacts(t *testing.T) {
	t.Parallel()
	season := 2
	primary := metadata.Media{
		Source: "douban", ID: "d1", Type: "tv", Title: "主源标题", Year: 2025,
		EpisodeCount: 12,
		SeasonInfo:   []metadata.SeasonInfo{{SeasonNumber: 2, EpisodeCount: 12, AirDate: "2026-04-01"}},
	}
	supplementer := &subscriptionFlowMediaSupplementer{item: metadata.Media{
		Source: "douban", ID: "d1", TMDBID: "42", Type: "tv", Title: "主源标题", Year: 2025,
		EpisodeCount: 24,
		SeasonInfo:   []metadata.SeasonInfo{{SeasonNumber: 2, EpisodeCount: 8, AirDate: "2026-05-01"}},
		GenreIDs:     []int{16}, OriginCountries: []string{"CN"},
	}, found: true}
	handler := &Handler{
		mediaDetails:      &subscriptionFlowMediaDetails{item: primary},
		mediaSupplementer: supplementer,
	}
	target := database.Subscription{
		MediaType: string(media.TypeTV), MediaSource: "douban", MediaID: "d1", Season: &season, Year: "2025",
	}

	got, available, err := handler.subscriptionMediaFacts(t.Context(), target)
	if err != nil || !available {
		t.Fatalf("subscriptionMediaFacts() available=%v err=%v", available, err)
	}
	if got.Source != primary.Source || got.ID != primary.ID || got.Title != primary.Title || got.TMDBID != "42" {
		t.Fatalf("主源身份或 TMDB ID 错误: %#v", got)
	}
	if got.EpisodeCount != primary.EpisodeCount || !reflect.DeepEqual(got.SeasonInfo, primary.SeasonInfo) {
		t.Fatalf("主源季集事实被辅助结果覆盖: %#v", got)
	}
	if supplementer.input.Season == nil || *supplementer.input.Season != season || supplementer.input.SeasonYear != 2026 {
		t.Fatalf("辅助入参 = %#v", supplementer.input)
	}
}

func TestV2FilterMediaFactsUsesSupplementAndFailsOpen(t *testing.T) {
	t.Parallel()
	target := database.Subscription{
		MediaType: string(media.TypeTV), MediaSource: "bangumi", MediaID: "7", Year: "2025",
	}
	primary := metadata.Media{Source: "bangumi", ID: "7", Type: "tv", Title: "动画", OriginalLanguage: "ja", Year: 2025}

	handler := &Handler{
		mediaDetails: &subscriptionFlowMediaDetails{item: primary},
		mediaSupplementer: &subscriptionFlowMediaSupplementer{item: metadata.Media{
			Source: "bangumi", ID: "7", TMDBID: "42", Type: "tv", Title: "动画", OriginalLanguage: "ja",
			GenreIDs: []int{16}, OriginCountries: []string{"CN"},
		}, found: true},
	}
	facts, err := handler.v2FilterMediaFacts(t.Context(), target)
	if err != nil || facts.OriginalLanguage != "ja" || facts.Category != "国漫" {
		t.Fatalf("补充后的过滤事实 = %#v err=%v", facts, err)
	}

	providerErr := errors.New("tmdb unavailable")
	handler.mediaSupplementer = &subscriptionFlowMediaSupplementer{err: providerErr}
	facts, err = handler.v2FilterMediaFacts(t.Context(), target)
	if err != nil || facts.OriginalLanguage != "ja" || facts.Category != "未分类" {
		t.Fatalf("普通辅助失败未使用主源事实: %#v err=%v", facts, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	handler.mediaSupplementer = &subscriptionFlowMediaSupplementer{err: context.Canceled}
	if _, err = handler.v2FilterMediaFacts(canceled, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误 = %v, want context.Canceled", err)
	}
}

func TestMetadataTargetEpisodesNeverUsesSupplementSeasonFacts(t *testing.T) {
	t.Parallel()
	season := 2
	target := database.Subscription{
		MediaType: string(media.TypeTV), MediaSource: "anilist", MediaID: "9", Season: &season,
		Config: map[string]any{"start_episode": 1},
	}
	supplementer := &subscriptionFlowMediaSupplementer{item: metadata.Media{
		Source: "anilist", ID: "9", TMDBID: "99", Type: "tv", Title: "Anime",
		SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 2, EpisodeCount: 8}},
	}, found: true}
	handler := &Handler{
		mediaDetails:      &subscriptionFlowMediaDetails{item: metadata.Media{Source: "anilist", ID: "9", Type: "tv", Title: "Anime"}},
		mediaSupplementer: supplementer,
	}
	episodes, err := handler.metadataTargetEpisodes(t.Context(), target)
	if err != nil || len(episodes) != 0 {
		t.Fatalf("辅助来源不得填充主源季集: %#v err=%v", episodes, err)
	}

	handler.mediaDetails = &subscriptionFlowMediaDetails{item: metadata.Media{
		Source: "anilist", ID: "9", Type: "tv", Title: "Anime", EpisodeCount: 12,
	}}
	episodes, err = handler.metadataTargetEpisodes(t.Context(), target)
	if err != nil || len(episodes) != 12 || episodes[11].Episode != 12 {
		t.Fatalf("主源总集数应优先: %#v", episodes)
	}
}

func TestRefreshTVTargetPropagatesSupplementCancellationWithoutReconcile(t *testing.T) {
	t.Parallel()
	season := 1
	store := &subscriptionFlowStore{}
	handler := &Handler{
		store: store,
		mediaDetails: &subscriptionFlowMediaDetails{item: metadata.Media{
			Source: "douban", ID: "7", Type: "tv", Title: "Show", EpisodeCount: 12,
		}},
		mediaSupplementer: &subscriptionFlowMediaSupplementer{err: context.Canceled},
	}
	target := database.Subscription{
		ID: 42, Username: "alice", Name: "Show", MediaType: string(media.TypeTV),
		MediaSource: "douban", MediaID: "7", Season: &season,
		UpdatedAt: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
		Config:    map[string]any{"total_episode": 2, "lack_episode": 2},
	}

	if _, _, _, err := handler.refreshTVTarget(t.Context(), target); !errors.Is(err, context.Canceled) {
		t.Fatalf("refreshTVTarget() error = %v, want context.Canceled", err)
	}
	if len(store.reconciled) != 0 {
		t.Fatalf("取消后 reconcile 次数 = %d, want 0", len(store.reconciled))
	}
}

type subscriptionFlowMediaSupplementer struct {
	item  metadata.Media
	found bool
	err   error
	calls int
	input metadata.TMDBSupplementInput
}

func (s *subscriptionFlowMediaSupplementer) Supplement(
	_ context.Context,
	input metadata.TMDBSupplementInput,
) (metadata.Media, bool, error) {
	s.calls++
	s.input = input
	if s.err != nil {
		return input.Media, false, s.err
	}
	return s.item, s.found, nil
}
