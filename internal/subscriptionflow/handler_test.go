package subscriptionflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const subscriptionFlowDownloaderID = "11111111-1111-4111-8111-111111111111"

func TestHandlerSubmitsHighestRankedCandidate(t *testing.T) {
	t.Parallel()

	target := subscriptionFlowTarget()
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Config["media_category"] = "欧美"
	store := &subscriptionFlowStore{
		list: func(_ context.Context, _ time.Time, _ time.Time, _ int64, limit int) ([]database.Subscription, error) {
			if limit != searchPageSize {
				t.Fatalf("list limit = %d", limit)
			}
			return []database.Subscription{target}, nil
		},
		downloaders: []downloadconfig.Downloader{{
			ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
		}},
	}
	adapter := subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}
	searcher := &subscriptionFlowSearcher{search: func(_ context.Context, adapters []indexer.Adapter, query indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
		if len(adapters) != 1 || query.Keyword != "Example Movie 2026" || query.Limit != 100 {
			t.Fatalf("search = adapters:%d query:%+v", len(adapters), query)
		}
		resources := []indexer.Resource{
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.720p.WEB-DL", DownloadID: "41", Seeders: 100, PriOrder: 80},
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42", Seeders: 10, PriOrder: 90},
		}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	submission := &subscriptionFlowSubmission{}
	handler := mustSubscriptionFlowHandler(t, store, searcher, subscriptionFlowAdapters{adapter}, submission)
	mediaDetails := &subscriptionFlowMediaDetails{item: metadata.Media{
		Source: "themoviedb", ID: "42", VoteAverage: 8.7,
		PosterURL: "https://image.example/poster.jpg", BackdropURL: "https://image.example/backdrop.jpg",
	}}
	handler.SetMediaDetailResolver(mediaDetails)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 1 || submission.subscriptionID != target.ID || submission.username != target.Username {
		t.Fatalf("submission = %+v", submission)
	}
	if submission.plan.SiteID != 7 || submission.plan.DownloaderID != subscriptionFlowDownloaderID ||
		submission.plan.ResolverKind != "fixture" || submission.plan.ResolverVersion != 1 ||
		submission.plan.ResourceID != "42" || submission.plan.ResourceSecret != "" ||
		submission.plan.MoviePriority == nil || *submission.plan.MoviePriority != 90 {
		t.Fatalf("plan = %+v", submission.plan)
	}
	if submission.intent.ID == uuid.Nil || submission.intent.CandidateKey == "" || submission.intent.Label == "" {
		t.Fatalf("intent = %+v", submission.intent)
	}
	if submission.successInput.IntentID != submission.intent.ID ||
		submission.successInput.Username != target.Username ||
		submission.successInput.Manual || !submission.successInput.Background ||
		submission.successInput.Media.Source != target.MediaSource ||
		submission.successInput.Media.MediaID != target.MediaID ||
		submission.successInput.Media.TitleYear != "Example Movie (2026)" ||
		submission.successInput.Media.Category != "欧美" ||
		submission.successInput.Media.VoteAverage != 8.7 ||
		submission.successInput.Media.PosterURL != "https://image.example/poster.jpg" ||
		submission.successInput.Media.BackdropURL != "https://image.example/backdrop.jpg" ||
		submission.successInput.ResourceTerm != "WEB-DL 1080p" {
		t.Fatalf("success input = %#v", submission.successInput)
	}
	if mediaDetails.calls != 1 || mediaDetails.source != "themoviedb" || mediaDetails.id != "movie:42" {
		t.Fatalf("metadata detail = calls:%d source:%q id:%q", mediaDetails.calls, mediaDetails.source, mediaDetails.id)
	}
	if store.casCalls != 0 {
		t.Fatal("选中候选后状态应由提交事务推进")
	}
}

func TestHandlerUsesPersistedV2CandidateSortInputs(t *testing.T) {
	t.Parallel()

	target := subscriptionFlowTarget()
	target.Config["sites"] = []any{}
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
		downloaders: []downloadconfig.Downloader{{
			ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
		}},
		sortRules:   []string{"upload"},
		siteUploads: map[string]float64{"low-upload": 1.25, "high-upload": 1.5},
		siteOrders:  map[int]int{7: 9, 8: 1},
	}
	searcher := &subscriptionFlowSearcher{search: func(
		_ context.Context,
		_ []indexer.Adapter,
		_ indexer.Query,
		onEvent func(indexer.Event),
	) ([]indexer.Resource, error) {
		resources := []indexer.Resource{
			{SiteID: 7, Site: "low-upload", Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "low", Seeders: 100},
			{SiteID: 8, Site: "high-upload", Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "high", Seeders: 1},
		}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	submission := &subscriptionFlowSubmission{}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		searcher,
		subscriptionFlowAdapters{
			subscriptionFlowAdapter{id: 7, name: "low-upload", kind: "fixture", version: 1},
			subscriptionFlowAdapter{id: 8, name: "high-upload", kind: "fixture", version: 1},
		},
		submission,
	)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if store.sortCalls != 1 {
		t.Fatalf("LoadV2CandidateSortInputs() calls = %d, want 1", store.sortCalls)
	}
	if submission.calls != 1 || submission.plan.SiteID != 7 || submission.plan.ResourceSecret != "low" {
		t.Fatalf("submission = calls:%d plan:%+v, want V2 padded upload key to choose site 7 resource low", submission.calls, submission.plan)
	}
}

func TestHandlerPreservesV2UpstreamOrderBeforeStableCandidateSort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		eventResources    []indexer.Resource
		returnedResources []indexer.Resource
		wantSecret        string
	}{
		{
			name: "ignores coordinator return order when seeder is not configured",
			eventResources: []indexer.Resource{
				{
					SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL",
					DownloadID: "upstream-first", Seeders: 1,
				},
				{
					SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL",
					DownloadID: "coordinator-first", Seeders: 100,
				},
			},
			returnedResources: []indexer.Resource{
				{
					SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL",
					DownloadID: "coordinator-first", Seeders: 100,
				},
				{
					SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL",
					DownloadID: "upstream-first", Seeders: 1,
				},
			},
			wantSecret: "upstream-first",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := subscriptionFlowTarget()
			store := &subscriptionFlowStore{
				list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
					return []database.Subscription{target}, nil
				},
				downloaders: []downloadconfig.Downloader{{
					ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
				}},
				sortRules: []string{"torrent"},
			}
			searcher := &subscriptionFlowSearcher{search: func(
				_ context.Context,
				_ []indexer.Adapter,
				_ indexer.Query,
				onEvent func(indexer.Event),
			) ([]indexer.Resource, error) {
				onEvent(indexer.Event{Site: "fixture", Resources: tt.eventResources})
				return tt.returnedResources, nil
			}}
			submission := &subscriptionFlowSubmission{}
			handler := mustSubscriptionFlowHandler(
				t,
				store,
				searcher,
				subscriptionFlowAdapters{
					subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1},
				},
				submission,
			)

			if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if submission.calls != 1 || submission.plan.ResourceSecret != tt.wantSecret {
				t.Fatalf(
					"submission = calls:%d plan:%+v, want stable V2 order to choose %s",
					submission.calls,
					submission.plan,
					tt.wantSecret,
				)
			}
		})
	}
}

func TestHandlerMapsV2CandidateSortInputFailureToStorage(t *testing.T) {
	t.Parallel()

	target := subscriptionFlowTarget()
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
		sortErr: errors.New("sort inputs unavailable"),
	}
	searcher := &subscriptionFlowSearcher{search: func(
		_ context.Context,
		_ []indexer.Adapter,
		_ indexer.Query,
		onEvent func(indexer.Event),
	) ([]indexer.Resource, error) {
		resources := []indexer.Resource{{
			SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42",
		}}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	submission := &subscriptionFlowSubmission{}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		submission,
	)

	err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC())
	if !errors.Is(err, ErrSubscriptionSearchStorage) {
		t.Fatalf("HandleSubscriptionSearch() error = %v, want ErrSubscriptionSearchStorage", err)
	}
	if submission.calls != 0 {
		t.Fatalf("submissions = %d, want 0", submission.calls)
	}
}

func TestHandlerUsesPersistedMissingEpisodesAndSubmitsCoverage(t *testing.T) {
	t.Parallel()
	season := 1
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(3)
	target.Config["start_episode"] = float64(1)
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
		episodes: []subscription.SubscriptionEpisode{{
			SubscriptionID: target.ID, Season: 1, Episode: 2, State: subscription.SubscriptionEpisodeMissing,
		}},
		downloaders: []downloadconfig.Downloader{{
			ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
		}},
	}
	searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
		resources := []indexer.Resource{
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.S01E01.2026.1080p.WEB-DL", DownloadID: "41"},
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42"},
		}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	submission := &subscriptionFlowSubmission{}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		submission,
	)
	setSubscriptionFlowTVMetadata(handler, target)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wantInitialized := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}}
	if !reflect.DeepEqual(store.upserted, wantInitialized) {
		t.Fatalf("initialized episodes = %#v, want %#v", store.upserted, wantInitialized)
	}
	if submission.plan.ResourceID != "42" || !reflect.DeepEqual(
		submission.episodes,
		[]subscription.Episode{{Season: 1, Episode: 2}},
	) {
		t.Fatalf("submission = plan:%+v episodes:%+v", submission.plan, submission.episodes)
	}
	if !reflect.DeepEqual(
		submission.successInput.Episodes,
		[]organization.Episode{{Season: 1, Episode: 2}},
	) {
		t.Fatalf("success input episodes = %#v", submission.successInput.Episodes)
	}
}

func TestHandlerExecutesV2TVCandidatesSynchronously(t *testing.T) {
	t.Parallel()

	season := 1
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(2)
	target.Config["start_episode"] = float64(1)
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
		episodes: []subscription.SubscriptionEpisode{
			{SubscriptionID: target.ID, Season: 1, Episode: 1, State: subscription.SubscriptionEpisodeMissing},
			{SubscriptionID: target.ID, Season: 1, Episode: 2, State: subscription.SubscriptionEpisodeMissing},
		},
		downloaders: []downloadconfig.Downloader{{
			ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
		}},
	}
	searcher := &subscriptionFlowSearcher{search: func(
		_ context.Context,
		_ []indexer.Adapter,
		_ indexer.Query,
		onEvent func(indexer.Event),
	) ([]indexer.Resource, error) {
		resources := []indexer.Resource{
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.S01E01.2026.1080p.WEB-DL", DownloadID: "41", Seeders: 20},
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42", Seeders: 10},
		}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	submission := &subscriptionFlowSubmission{}
	execution := &subscriptionFlowExecution{submission: submission, store: store}
	handler, err := NewHandler(
		store,
		searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{
			id: 7, name: "fixture", kind: "fixture", version: 1,
			body: v2TVTestMetainfo(t, "Example.Movie.S01E01.mkv", "Example.Movie.S01E02.mkv"),
		}},
		submission,
		execution,
	)
	if err != nil {
		t.Fatal(err)
	}
	setSubscriptionFlowTVMetadata(handler, target)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 0 {
		t.Fatalf("async submissions = %d, want 0", submission.calls)
	}
	if submission.stageCalls != 2 || submission.rssStageCalls != 0 {
		t.Fatalf("stages = precise:%d rss:%d, want precise:2 rss:0", submission.stageCalls, submission.rssStageCalls)
	}
	if !reflect.DeepEqual(submission.stageResourceIDs, []string{"41", "42"}) {
		t.Fatalf("staged resources = %v, want [41 42]", submission.stageResourceIDs)
	}
	if len(execution.calls) != 2 || len(store.episodes) != 0 {
		t.Fatalf("execution calls = %d, remaining episodes = %#v", len(execution.calls), store.episodes)
	}
}

func TestTargetEpisodesFromMetadata(t *testing.T) {
	t.Parallel()
	seasonTwo, seasonThree, invalidSeason := 2, 3, 1000
	tests := []struct {
		name   string
		season *int
		start  int
		detail metadata.Media
		want   []subscription.Episode
	}{
		{
			name:   "missing season defaults to season one",
			detail: metadata.Media{Type: "tv", SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 1, EpisodeCount: 3}}},
			want: []subscription.Episode{
				{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3},
			},
		},
		{
			name:   "configured start episode trims metadata target",
			start:  2,
			detail: metadata.Media{Type: "tv", EpisodeCount: 3},
			want:   []subscription.Episode{{Season: 1, Episode: 2}, {Season: 1, Episode: 3}},
		},
		{
			name:   "structured detail selects requested season",
			season: &seasonTwo,
			detail: metadata.Media{Type: "tv", SeasonInfo: []metadata.SeasonInfo{
				{SeasonNumber: 1, EpisodeCount: 12}, {SeasonNumber: 2, EpisodeCount: 2},
			}},
			want: []subscription.Episode{{Season: 2, Episode: 1}, {Season: 2, Episode: 2}},
		},
		{
			name:   "real provider total uses requested season",
			season: &seasonThree,
			detail: metadata.Media{Type: "anime", EpisodeCount: 2},
			want:   []subscription.Episode{{Season: 3, Episode: 1}, {Season: 3, Episode: 2}},
		},
		{
			name:   "structured detail never guesses a missing season from total",
			season: &seasonTwo,
			detail: metadata.Media{
				Type: "tv", EpisodeCount: 24,
				SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 1, EpisodeCount: 12}},
			},
		},
		{
			name:   "episode count above per season limit is rejected",
			detail: metadata.Media{Type: "tv", EpisodeCount: 5001},
		},
		{
			name:   "non tv detail is rejected",
			detail: metadata.Media{Type: "movie", EpisodeCount: 2},
		},
		{
			name:   "season above persistence limit is rejected",
			season: &invalidSeason,
			detail: metadata.Media{Type: "tv", EpisodeCount: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := targetEpisodesFromMetadata(tt.season, tt.start, tt.detail)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("targetEpisodesFromMetadata() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHandlerBuildsEmptyTVTargetFromMetadata(t *testing.T) {
	t.Parallel()
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Config["total_episode"] = float64(0)
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
		reflectUpserts: true,
		downloaders: []downloadconfig.Downloader{{
			ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
		}},
	}
	searchCalls := 0
	searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
		searchCalls++
		resources := []indexer.Resource{{
			SiteID: 7, Site: "fixture", Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42",
		}}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	submission := &subscriptionFlowSubmission{}
	handler := mustSubscriptionFlowHandler(
		t, store, searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		submission,
	)
	mediaDetails := &subscriptionFlowMediaDetails{item: metadata.Media{
		Source: "themoviedb", ID: "42", Type: "tv",
		SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 1, EpisodeCount: 2}},
	}}
	handler.SetMediaDetailResolver(mediaDetails)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wantTargets := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	if !reflect.DeepEqual(store.upserted, wantTargets) {
		t.Fatalf("metadata targets = %#v, want %#v", store.upserted, wantTargets)
	}
	if searchCalls != 1 || submission.calls != 1 || !reflect.DeepEqual(
		submission.episodes, []subscription.Episode{{Season: 1, Episode: 2}},
	) {
		t.Fatalf("search/submission = %d/%d episodes %#v", searchCalls, submission.calls, submission.episodes)
	}
	if mediaDetails.calls < 1 || mediaDetails.source != "themoviedb" || mediaDetails.id != "tv:42" {
		t.Fatalf("metadata detail = calls:%d source:%q id:%q", mediaDetails.calls, mediaDetails.source, mediaDetails.id)
	}
}

func TestMetadataEpisodeFallbackOnlyForUnknownTotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config map[string]any
		want   bool
	}{
		{name: "missing total is unknown", config: map[string]any{}, want: true},
		{name: "zero total is unknown", config: map[string]any{"total_episode": float64(0)}, want: true},
		{name: "positive total is explicit", config: map[string]any{"total_episode": float64(12)}, want: false},
		{name: "negative total is invalid", config: map[string]any{"total_episode": float64(-1)}, want: false},
		{name: "oversized total is invalid", config: map[string]any{"total_episode": float64(5001)}, want: false},
		{name: "non integer total is invalid", config: map[string]any{"total_episode": 2.5}, want: false},
		{name: "wrong total type is invalid", config: map[string]any{"total_episode": "0"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := metadataEpisodeFallbackAllowed(tt.config); got != tt.want {
				t.Fatalf("metadataEpisodeFallbackAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandlerHandlesUnresolvedAndEmptyTVTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		resolver      *subscriptionFlowMediaDetails
		config        map[string]any
		wantSearch    int
		wantComplete  int
		wantCAS       int
		wantReadCalls int
	}{
		{name: "resolver unavailable", wantCAS: 1},
		{
			name: "detail failure", resolver: &subscriptionFlowMediaDetails{err: errors.New("metadata unavailable")},
			wantCAS: 1, wantReadCalls: 1,
		},
		{
			name: "detail has no episodes", resolver: &subscriptionFlowMediaDetails{item: metadata.Media{Type: "tv"}},
			wantSearch: 1, wantComplete: 1, wantReadCalls: 2,
		},
		{
			name: "default season absent from structured detail",
			resolver: &subscriptionFlowMediaDetails{item: metadata.Media{
				Type: "tv", EpisodeCount: 24,
				SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 2, EpisodeCount: 12}},
			}},
			wantSearch: 1, wantComplete: 1, wantReadCalls: 1,
		},
		{
			name: "manually locked range is not expanded by metadata",
			config: map[string]any{
				"total_episode": float64(2), "start_episode": float64(3), "manual_total_episode": float64(1),
			},
			resolver: &subscriptionFlowMediaDetails{item: metadata.Media{
				Type: "tv", EpisodeCount: 24,
			}},
			wantComplete: 1, wantReadCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := subscriptionFlowTarget()
			target.MediaType = string(media.TypeTV)
			target.MediaSource = "themoviedb"
			target.MediaID = "42"
			if tt.config != nil {
				target.Config = tt.config
			}
			store := &subscriptionFlowStore{list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
				return []database.Subscription{target}, nil
			}}
			searchCalls := 0
			searcher := &subscriptionFlowSearcher{search: func(
				_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event),
			) ([]indexer.Resource, error) {
				searchCalls++
				resources := []indexer.Resource{{
					SiteID: 7, Site: "fixture", Title: "Example.Movie.S01E01.2026.1080p.WEB-DL", DownloadID: "42",
				}}
				onEvent(indexer.Event{Site: "fixture", Resources: resources})
				return resources, nil
			}}
			submission := &subscriptionFlowSubmission{}
			handler := mustSubscriptionFlowHandler(
				t, store, searcher,
				subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
				submission,
			)
			if tt.resolver != nil {
				handler.SetMediaDetailResolver(tt.resolver)
			}

			if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if searchCalls != tt.wantSearch || submission.calls != 0 || len(store.upserted) != 0 {
				t.Fatalf("empty target side effects = search:%d submit:%d upsert:%#v", searchCalls, submission.calls, store.upserted)
			}
			if store.casCalls != tt.wantCAS || (tt.wantCAS > 0 && store.casNext != subscription.Active) ||
				store.completeCalls != tt.wantComplete {
				t.Fatalf("state writes = CAS:%d next:%s complete:%d", store.casCalls, store.casNext, store.completeCalls)
			}
			if tt.wantComplete > 0 && (store.completeState != subscription.PendingSearch ||
				store.completeOwner != target.Username || store.completeID != target.ID ||
				!store.completeTime.Equal(target.UpdatedAt)) {
				t.Fatalf("completion = owner:%q id:%d state:%s time:%s", store.completeOwner, store.completeID, store.completeState, store.completeTime)
			}
			if tt.resolver != nil && tt.resolver.calls != tt.wantReadCalls {
				t.Fatalf("metadata reads = %d, want %d", tt.resolver.calls, tt.wantReadCalls)
			}
		})
	}
}

func TestHandlerMarksActiveWhenAnotherWorkerAcquiredAllMissingEpisodes(t *testing.T) {
	t.Parallel()
	season := 1
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(2)
	target.Config["start_episode"] = float64(1)
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
		episodes: []subscription.SubscriptionEpisode{},
	}
	searchCalls := 0
	searcher := &subscriptionFlowSearcher{search: func(context.Context, []indexer.Adapter, indexer.Query, func(indexer.Event)) ([]indexer.Resource, error) {
		searchCalls++
		return nil, errors.New("search must not run after missing episodes were acquired")
	}}
	submission := &subscriptionFlowSubmission{}
	handler := mustSubscriptionFlowHandler(
		t, store, searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		submission,
	)
	setSubscriptionFlowTVMetadata(handler, target)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wantInitialized := []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	if !reflect.DeepEqual(store.upserted, wantInitialized) {
		t.Fatalf("initialized episodes = %#v, want %#v", store.upserted, wantInitialized)
	}
	if searchCalls != 0 || submission.calls != 0 {
		t.Fatalf("search/submission calls = %d/%d, want 0/0", searchCalls, submission.calls)
	}
	if store.casCalls != 1 || store.casID != target.ID || store.casNext != subscription.Active {
		t.Fatalf("CAS = calls:%d id:%d next:%s", store.casCalls, store.casID, store.casNext)
	}
}

func TestHandlerMarksNoMatchActive(t *testing.T) {
	t.Parallel()

	target := subscriptionFlowTarget()
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
	}
	searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
		onEvent(indexer.Event{Site: "fixture"})
		return []indexer.Resource{}, nil
	}}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		&subscriptionFlowSubmission{},
	)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if store.casCalls != 1 || store.casID != target.ID || store.casNext != subscription.Active {
		t.Fatalf("CAS = calls:%d id:%d next:%s", store.casCalls, store.casID, store.casNext)
	}
}

func TestHandlerPausesPermanentSubscriptionConfigurationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configure  func(*database.Subscription, *subscriptionFlowStore)
		adapters   subscriptionFlowAdapters
		wantSearch bool
	}{
		{
			name: "invalid release regex",
			configure: func(target *database.Subscription, _ *subscriptionFlowStore) {
				target.Config["quality"] = "["
			},
			adapters:   subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
			wantSearch: true,
		},
		{
			name: "configured site no longer exists",
			configure: func(target *database.Subscription, _ *subscriptionFlowStore) {
				target.Config["sites"] = []any{float64(99)}
			},
			adapters: subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		},
		{
			name: "configured downloader no longer exists",
			configure: func(target *database.Subscription, store *subscriptionFlowStore) {
				target.Config["downloader"] = "removed"
				store.downloaders = []downloadconfig.Downloader{{
					ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
				}}
			},
			adapters:   subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
			wantSearch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := subscriptionFlowTarget()
			store := &subscriptionFlowStore{list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
				return []database.Subscription{target}, nil
			}}
			tt.configure(&target, store)
			store.list = func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
				return []database.Subscription{target}, nil
			}
			searchCalls := 0
			searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
				searchCalls++
				resources := []indexer.Resource{{
					SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42",
				}}
				onEvent(indexer.Event{Site: "fixture", Resources: resources})
				return resources, nil
			}}
			handler := mustSubscriptionFlowHandler(t, store, searcher, tt.adapters, &subscriptionFlowSubmission{})

			if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if store.casCalls != 1 || store.casNext != subscription.Paused {
				t.Fatalf("CAS = calls:%d next:%s", store.casCalls, store.casNext)
			}
			if (searchCalls > 0) != tt.wantSearch {
				t.Fatalf("search calls = %d, wantSearch=%v", searchCalls, tt.wantSearch)
			}
		})
	}
}

func TestHandlerRetriesWhenEverySiteFails(t *testing.T) {
	t.Parallel()

	const sensitiveFailure = "site error: cookie=must-not-leak"
	target := subscriptionFlowTarget()
	store := &subscriptionFlowStore{list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
		return []database.Subscription{target}, nil
	}}
	searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
		onEvent(indexer.Event{Site: "fixture", Error: sensitiveFailure})
		return nil, nil
	}}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		&subscriptionFlowSubmission{},
	)

	err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC())
	if !errors.Is(err, ErrSubscriptionIndexer) || strings.Contains(err.Error(), sensitiveFailure) {
		t.Fatalf("HandleSubscriptionSearch() error = %v", err)
	}
	if store.casCalls != 0 {
		t.Fatal("全站失败不能把订阅误标为无候选")
	}
}

func TestHandlerRetriesWhenSearcherProducesNoSiteEvents(t *testing.T) {
	t.Parallel()
	target := subscriptionFlowTarget()
	store := &subscriptionFlowStore{list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
		return []database.Subscription{target}, nil
	}}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		&subscriptionFlowSearcher{search: func(context.Context, []indexer.Adapter, indexer.Query, func(indexer.Event)) ([]indexer.Resource, error) {
			return []indexer.Resource{}, nil
		}},
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		&subscriptionFlowSubmission{},
	)

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); !errors.Is(err, ErrSubscriptionIndexer) {
		t.Fatalf("HandleSubscriptionSearch() error = %v", err)
	}
	if store.casCalls != 0 {
		t.Fatal("零站点事件不能被当作正常的无候选结果")
	}
}

func TestHandlerUsesStableKeysetCursor(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	firstPage := make([]database.Subscription, searchPageSize)
	for index := range firstPage {
		firstPage[index] = subscriptionFlowTarget()
		firstPage[index].ID = int64(index + 1)
		firstPage[index].UpdatedAt = base.Add(time.Duration(index) * time.Second)
		firstPage[index].MediaType = "invalid"
	}
	listCalls := 0
	store := &subscriptionFlowStore{list: func(_ context.Context, _ time.Time, after time.Time, afterID int64, _ int) ([]database.Subscription, error) {
		listCalls++
		if listCalls == 1 {
			if !after.IsZero() || afterID != 0 {
				t.Fatalf("first cursor = (%v,%d)", after, afterID)
			}
			return firstPage, nil
		}
		last := firstPage[len(firstPage)-1]
		if !after.Equal(last.UpdatedAt) || afterID != last.ID {
			t.Fatalf("next cursor = (%v,%d), want (%v,%d)", after, afterID, last.UpdatedAt, last.ID)
		}
		return []database.Subscription{}, nil
	}}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		&subscriptionFlowSearcher{},
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		&subscriptionFlowSubmission{},
	)
	if err := handler.HandleSubscriptionSearch(t.Context(), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if listCalls != 2 || store.casCalls != searchPageSize {
		t.Fatalf("calls = list:%d cas:%d", listCalls, store.casCalls)
	}
}

func TestCandidateQueryBuildsInitialMissingEpisodes(t *testing.T) {
	t.Parallel()
	season := 2
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeTV)
	target.Season = &season
	target.Config = map[string]any{
		"total_episode": float64(4), "start_episode": float64(2),
		"sites": []any{float64(9), float64(7)}, "include": "WEB-DL, HEVC",
		"exclude": "CAM\nTS", "custom_words": "foo=>bar\nbaz=>qux",
		"quality": "WEB-?DL", "resolution": "1080[pi]", "effect": "HDR10|DOVI",
	}
	query, keyword, err := candidateQuery(target)
	if err != nil {
		t.Fatal(err)
	}
	if keyword != "Example Movie 2026" || !reflect.DeepEqual(query.SiteIDs, []int{9, 7}) ||
		!reflect.DeepEqual(query.Include, []string{"WEB-DL, HEVC"}) ||
		!reflect.DeepEqual(query.Exclude, []string{"CAM\nTS"}) || query.Quality != "WEB-?DL" ||
		query.Resolution != "1080[pi]" || query.Effect != "HDR10|DOVI" {
		t.Fatalf("query = %+v keyword=%q", query, keyword)
	}
	wantEpisodes := []subscription.Episode{{Season: 2, Episode: 2}, {Season: 2, Episode: 3}, {Season: 2, Episode: 4}}
	if !reflect.DeepEqual(query.MissingEpisodes, wantEpisodes) {
		t.Fatalf("MissingEpisodes = %+v", query.MissingEpisodes)
	}
}

func TestSelectAdaptersUsesConfiguredOrderAndDeterministicLimit(t *testing.T) {
	t.Parallel()
	all := make([]indexer.Adapter, 0, maxSearchAdapters+2)
	for siteID := maxSearchAdapters + 2; siteID >= 1; siteID-- {
		all = append(all, subscriptionFlowAdapter{id: siteID, name: fmt.Sprintf("site-%d", siteID)})
	}
	configured := selectAdapters(all, []int{9, 2, 9, 4})
	if got := adapterIDs(configured); !reflect.DeepEqual(got, []int{9, 2, 4}) {
		t.Fatalf("configured adapters = %v", got)
	}
	defaults := selectAdapters(all, nil)
	if len(defaults) != maxSearchAdapters || defaults[0].(indexer.SiteIDAdapter).SiteID() != 1 ||
		defaults[len(defaults)-1].(indexer.SiteIDAdapter).SiteID() != maxSearchAdapters {
		t.Fatalf("default adapters are not deterministic: first=%d last=%d len=%d",
			defaults[0].(indexer.SiteIDAdapter).SiteID(),
			defaults[len(defaults)-1].(indexer.SiteIDAdapter).SiteID(),
			len(defaults))
	}
}

func adapterIDs(adapters []indexer.Adapter) []int {
	result := make([]int, 0, len(adapters))
	for _, adapter := range adapters {
		result = append(result, adapter.(indexer.SiteIDAdapter).SiteID())
	}
	return result
}

func subscriptionFlowTarget() database.Subscription {
	return database.Subscription{
		ID: 21, Username: "alice", Name: "Example Movie", MediaType: string(media.TypeMovie), Year: "2026",
		InternalState: subscription.PendingSearch, UpdatedAt: time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC),
		Config: map[string]any{"sites": []any{float64(7)}, "resolution": "1080p"},
	}
}

func mustSubscriptionFlowHandler(
	t *testing.T,
	store TargetStore,
	searcher Searcher,
	adapters AdapterProvider,
	submission DownloadSubmission,
) *Handler {
	t.Helper()
	handler, err := NewHandler(store, searcher, adapters, submission)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func setSubscriptionFlowTVMetadata(handler *Handler, target database.Subscription) {
	season := 1
	if target.Season != nil {
		season = *target.Season
	}
	total, _ := integerValue(target.Config["total_episode"])
	handler.SetMediaDetailResolver(&subscriptionFlowMediaDetails{item: metadata.Media{
		Source: target.MediaSource, ID: target.MediaID, Type: "tv",
		SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: season, EpisodeCount: total}},
	}})
}

type subscriptionFlowStore struct {
	list            func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error)
	downloaders     []downloadconfig.Downloader
	found           bool
	downloadErr     error
	settingErr      error
	casErr          error
	casCalls        int
	casID           int64
	casNext         subscription.State
	completeErr     error
	completeCalls   int
	completeOwner   string
	completeID      int64
	completeState   subscription.State
	completeTime    time.Time
	completeType    media.MediaType
	progressErr     error
	progressCalls   int
	progressOwner   string
	progressID      int64
	progressState   subscription.State
	progressTime    time.Time
	episodes        []subscription.SubscriptionEpisode
	episodeErr      error
	upserted        []subscription.Episode
	reflectUpserts  bool
	sortRules       []string
	siteUploads     map[string]float64
	siteOrders      map[int]int
	sortErr         error
	sortCalls       int
	reconciled      []subscription.TVTargetResolution
	activeTarget    *database.Subscription
	activeTargetErr error
}

func (s *subscriptionFlowStore) GetActiveSubscription(
	_ context.Context, _ string, _ int64,
) (database.Subscription, error) {
	if s.activeTargetErr != nil {
		return database.Subscription{}, s.activeTargetErr
	}
	if s.activeTarget == nil {
		return database.Subscription{}, database.ErrSubscriptionNotFound
	}
	return *s.activeTarget, nil
}

type subscriptionFlowMediaDetails struct {
	item   metadata.Media
	err    error
	calls  int
	source string
	id     string
}

func (r *subscriptionFlowMediaDetails) Detail(_ context.Context, source, id string) (metadata.Media, error) {
	r.calls++
	r.source = source
	r.id = id
	return r.item, r.err
}

func (s *subscriptionFlowStore) ListSubscriptionsForPreciseSearch(
	ctx context.Context, cutoff, after time.Time, afterID int64, limit int,
) ([]database.Subscription, error) {
	if s.list == nil {
		return []database.Subscription{}, nil
	}
	return s.list(ctx, cutoff, after, afterID, limit)
}

func (s *subscriptionFlowStore) CompareAndSwapSubscriptionPreciseSearchState(
	_ context.Context, _ string, id int64, next subscription.State,
) error {
	s.casCalls++
	s.casID = id
	s.casNext = next
	return s.casErr
}

func (s *subscriptionFlowStore) CompareAndSwapSubscriptionPreciseSearchStateAt(
	ctx context.Context, username string, id int64, next subscription.State, _ time.Time,
) error {
	return s.CompareAndSwapSubscriptionPreciseSearchState(ctx, username, id, next)
}

func (s *subscriptionFlowStore) CompleteSatisfiedTVSubscription(
	_ context.Context,
	username string,
	id int64,
	expected subscription.State,
	expectedUpdatedAt time.Time,
) error {
	s.completeCalls++
	s.completeOwner = username
	s.completeID = id
	s.completeState = expected
	s.completeTime = expectedUpdatedAt
	s.completeType = media.TypeTV
	return s.completeErr
}

func (s *subscriptionFlowStore) RecordEmptyTVSubscriptionProgress(
	_ context.Context,
	username string,
	id int64,
	expected subscription.State,
	expectedUpdatedAt time.Time,
) error {
	s.progressCalls++
	s.progressOwner = username
	s.progressID = id
	s.progressState = expected
	s.progressTime = expectedUpdatedAt
	return s.progressErr
}

func (s *subscriptionFlowStore) CompleteSatisfiedMovieSubscription(
	_ context.Context,
	username string,
	id int64,
	expected subscription.State,
	expectedUpdatedAt time.Time,
) error {
	s.completeCalls++
	s.completeOwner = username
	s.completeID = id
	s.completeState = expected
	s.completeTime = expectedUpdatedAt
	s.completeType = media.TypeMovie
	return s.completeErr
}

func (s *subscriptionFlowStore) UpsertSubscriptionEpisodes(
	_ context.Context,
	_ string,
	_ int64,
	episodes []subscription.Episode,
) error {
	s.upserted = append([]subscription.Episode(nil), episodes...)
	if s.reflectUpserts {
		for _, episode := range episodes {
			s.episodes = append(s.episodes, subscription.SubscriptionEpisode{
				Season: episode.Season, Episode: episode.Episode, State: subscription.SubscriptionEpisodeMissing,
			})
		}
	}
	return s.episodeErr
}

func (s *subscriptionFlowStore) ListSubscriptionEpisodes(
	_ context.Context,
	_ string,
	_ int64,
	_ []subscription.SubscriptionEpisodeState,
	_ int,
) ([]subscription.SubscriptionEpisode, error) {
	return append([]subscription.SubscriptionEpisode(nil), s.episodes...), s.episodeErr
}

func (s *subscriptionFlowStore) ReconcileSubscriptionTarget(
	_ context.Context,
	input database.SubscriptionTargetReconcile,
) (time.Time, error) {
	if s.episodeErr != nil {
		return time.Time{}, s.episodeErr
	}
	s.reconciled = append(s.reconciled, input.Target)
	s.upserted = append([]subscription.Episode(nil), input.Target.TargetEpisodes...)
	if !s.reflectUpserts {
		return input.ExpectedUpdatedAt, nil
	}
	byCoordinate := make(map[subscription.Episode]subscription.SubscriptionEpisode, len(s.episodes))
	for _, item := range s.episodes {
		byCoordinate[subscription.Episode{Season: item.Season, Episode: item.Episode}] = item
	}
	for _, episode := range input.Target.TargetEpisodes {
		if _, exists := byCoordinate[episode]; !exists {
			byCoordinate[episode] = subscription.SubscriptionEpisode{
				SubscriptionID: input.SubscriptionID, Season: episode.Season, Episode: episode.Episode,
				State: subscription.SubscriptionEpisodeMissing,
			}
		}
	}
	s.episodes = s.episodes[:0]
	for _, episode := range input.Target.TargetEpisodes {
		s.episodes = append(s.episodes, byCoordinate[episode])
	}
	return input.ExpectedUpdatedAt, nil
}

func (s *subscriptionFlowStore) ListSubscriptionEpisodesForTarget(
	_ context.Context,
	_ string,
	_ int64,
	_ time.Time,
	targetEpisodes []subscription.Episode,
	states []subscription.SubscriptionEpisodeState,
	_ int,
) ([]subscription.SubscriptionEpisode, error) {
	if s.episodeErr != nil {
		return nil, s.episodeErr
	}
	target := make(map[subscription.Episode]struct{}, len(targetEpisodes))
	for _, episode := range targetEpisodes {
		target[episode] = struct{}{}
	}
	allowed := make(map[subscription.SubscriptionEpisodeState]struct{}, len(states))
	for _, state := range states {
		allowed[state] = struct{}{}
	}
	result := make([]subscription.SubscriptionEpisode, 0)
	for _, item := range s.episodes {
		if _, ok := target[subscription.Episode{Season: item.Season, Episode: item.Episode}]; !ok {
			continue
		}
		if _, ok := allowed[item.State]; ok {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *subscriptionFlowStore) LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error) {
	found := s.found
	if len(s.downloaders) > 0 {
		found = true
	}
	return s.downloaders, found, s.downloadErr
}

func (s *subscriptionFlowStore) GetSetting(_ context.Context, _ string, target any) (bool, error) {
	if s.settingErr != nil {
		return false, s.settingErr
	}
	directories, ok := target.(*[]downloadconfig.Directory)
	if ok {
		*directories = []downloadconfig.Directory{}
	}
	return false, nil
}

func (s *subscriptionFlowStore) LoadV2CandidateSortInputs(
	context.Context,
) ([]string, map[string]float64, map[int]int, error) {
	s.sortCalls++
	return s.sortRules, s.siteUploads, s.siteOrders, s.sortErr
}

type subscriptionFlowSearcher struct {
	search func(context.Context, []indexer.Adapter, indexer.Query, func(indexer.Event)) ([]indexer.Resource, error)
}

func (s *subscriptionFlowSearcher) Search(
	ctx context.Context, adapters []indexer.Adapter, query indexer.Query, onEvent func(indexer.Event),
) ([]indexer.Resource, error) {
	if s.search == nil {
		return []indexer.Resource{}, nil
	}
	return s.search(ctx, adapters, query, onEvent)
}

type subscriptionFlowAdapters []indexer.Adapter

func (a subscriptionFlowAdapters) Snapshot() []indexer.Adapter {
	return append([]indexer.Adapter(nil), a...)
}

type subscriptionFlowAdapter struct {
	id      int
	name    string
	kind    string
	version int16
	body    []byte
}

func (a subscriptionFlowAdapter) Name() string { return a.name }
func (a subscriptionFlowAdapter) SiteID() int  { return a.id }
func (a subscriptionFlowAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}
func (a subscriptionFlowAdapter) ResolveDownload(context.Context, string) (indexer.ResolvedDownload, error) {
	return indexer.ResolvedDownload{Body: append([]byte(nil), a.body...)}, nil
}
func (a subscriptionFlowAdapter) ResolverKind() string   { return a.kind }
func (a subscriptionFlowAdapter) ResolverVersion() int16 { return a.version }

type subscriptionFlowSubmission struct {
	calls            int
	stageCalls       int
	rssStageCalls    int
	username         string
	subscriptionID   int64
	rssState         subscription.State
	rssUsernames     []string
	rssIDs           []int64
	rssStates        []subscription.State
	intent           downloader.Intent
	plan             database.DownloadIntentPlan
	successInput     organization.SuccessInput
	episodes         []subscription.Episode
	stageResourceIDs []string
	err              error
}

func (s *subscriptionFlowSubmission) SubmitSubscriptionEpisodes(
	_ context.Context,
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	s.calls++
	s.username = username
	s.subscriptionID = subscriptionID
	s.intent = intent
	s.plan = plan
	s.successInput = successInput.Clone()
	s.episodes = append([]subscription.Episode(nil), episodes...)
	return intent.ID, s.err
}

func (s *subscriptionFlowSubmission) SubmitSubscriptionTargetEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	_ time.Time,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.SubmitSubscriptionEpisodes(ctx, username, subscriptionID, intent, plan, successInput, episodes)
}

func (s *subscriptionFlowSubmission) StageSubscriptionEpisodes(
	_ context.Context,
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	s.stageCalls++
	s.captureStage(username, subscriptionID, intent, plan, successInput, episodes)
	return intent.ID, s.err
}

func (s *subscriptionFlowSubmission) StageSubscriptionTargetEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	_ time.Time,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.StageSubscriptionEpisodes(ctx, username, subscriptionID, intent, plan, successInput, episodes)
}

func (s *subscriptionFlowSubmission) StageRSSSubscriptionEpisodes(
	_ context.Context,
	username string,
	subscriptionID int64,
	expected subscription.State,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	s.rssStageCalls++
	s.rssState = expected
	s.rssStates = append(s.rssStates, expected)
	s.captureStage(username, subscriptionID, intent, plan, successInput, episodes)
	return intent.ID, s.err
}

func (s *subscriptionFlowSubmission) StageRSSTargetEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expected subscription.State,
	_ time.Time,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.StageRSSSubscriptionEpisodes(ctx, username, subscriptionID, expected, intent, plan, successInput, episodes)
}

func (s *subscriptionFlowSubmission) captureStage(
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) {
	s.username = username
	s.subscriptionID = subscriptionID
	s.intent = intent
	s.plan = plan
	s.successInput = successInput.Clone()
	s.episodes = append([]subscription.Episode(nil), episodes...)
	s.stageResourceIDs = append(s.stageResourceIDs, plan.ResourceID)
}

type subscriptionFlowExecution struct {
	submission *subscriptionFlowSubmission
	store      *subscriptionFlowStore
	calls      []uuid.UUID
	outcomes   []downloadflow.IntentExecutionResult
}

func (e *subscriptionFlowExecution) ExecuteIntent(
	_ context.Context,
	_ string,
	intentID uuid.UUID,
) (downloadflow.IntentExecutionResult, error) {
	e.calls = append(e.calls, intentID)
	result := downloadflow.IntentExecutionResult{Outcome: downloadflow.IntentExecutionAccepted}
	if len(e.outcomes) > 0 {
		result = e.outcomes[0]
		e.outcomes = e.outcomes[1:]
	}
	if result.Outcome == downloadflow.IntentExecutionAccepted {
		e.consumeStagedEpisodes()
	}
	return result, nil
}

func (e *subscriptionFlowExecution) ExecuteIntentWithMetainfo(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	_ []byte,
) (downloadflow.IntentExecutionResult, error) {
	return e.ExecuteIntent(ctx, username, intentID)
}

func (e *subscriptionFlowExecution) consumeStagedEpisodes() {
	selected := make(map[subscription.Episode]struct{}, len(e.submission.episodes))
	for _, episode := range e.submission.episodes {
		selected[episode] = struct{}{}
	}
	remaining := e.store.episodes[:0]
	for _, row := range e.store.episodes {
		if _, consumed := selected[subscription.Episode{Season: row.Season, Episode: row.Episode}]; !consumed {
			remaining = append(remaining, row)
		}
	}
	e.store.episodes = remaining
}

var _ indexer.VersionedDownloadResolver = subscriptionFlowAdapter{}
