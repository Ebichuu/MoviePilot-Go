package subscriptionflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestRSSHandlerPollUsesConfiguredInterval(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		intervals []time.Duration
		want      time.Duration
	}{
		{name: "default", want: 30 * time.Minute},
		{name: "V2 minimum", intervals: []time.Duration{5 * time.Minute}, want: 5 * time.Minute},
		{name: "configured", intervals: []time.Duration{45 * time.Minute}, want: 45 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRSSFlowStore(subscriptionFlowTarget())
			store.cursor.NextPollAt = now.Add(-time.Second)
			adapter := rssFlowAdapter{
				subscriptionFlowAdapter: subscriptionFlowAdapter{
					id: 7, name: "rss", kind: "declarative-rss", version: 1,
				},
				key: "sha256:configured-interval",
			}
			base := mustSubscriptionFlowHandler(
				t,
				store,
				&subscriptionFlowSearcher{},
				subscriptionFlowAdapters{adapter},
				&subscriptionFlowSubmission{},
			)
			handler, err := NewRSSHandler(
				store,
				base,
				subscriptionFlowAdapters{adapter},
				&rssFlowEnqueuer{},
				tt.intervals...,
			)
			if err != nil {
				t.Fatal(err)
			}
			handler.now = func() time.Time { return now }

			if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
				t.Fatal(err)
			}
			wantNextPollAt := now.Add(tt.want)
			if !store.nextPollAt.Equal(wantNextPollAt) {
				t.Fatalf("next poll at = %s, want %s", store.nextPollAt, wantNextPollAt)
			}
		})
	}
}

func TestRSSHandlerPollMarksNoCacheRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.cursor.NextPollAt = now.Add(-time.Second)
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "M-Team", kind: "declarative-rss", version: 1},
		key:                     "sha256:mteam-no-cache", noCache: true,
	}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
		t.Fatal(err)
	}
	if !store.commitReplaceCache {
		t.Fatal("m-team non-empty RSS refresh did not request cache replacement")
	}
}

func TestRSSHandlerPollFiltersHistoricalItemsBeforeRecognition(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.cursor.NextPollAt = now.Add(-time.Second)
	store.items = []database.RSSItem{{ID: 41, SiteID: 7, ItemKey: "sha256:old", Title: "Old", Resource: json.RawMessage(`{"description":"cached"}`)}}
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1}, key: "sha256:history-filter"}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{})
	recognizer := &rssMediaRecognizerFake{}
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.mediaRecognizer = recognizer
	handler.now = func() time.Time { return now }
	adapter.response = indexer.RSSResponse{Items: []indexer.RSSItem{
		{ItemKey: "sha256:old", Resource: indexer.Resource{SiteID: 7, Title: "Old", Description: "cached", DownloadID: "old"}},
		{ItemKey: "sha256:new", Resource: indexer.Resource{SiteID: 7, Title: "New", Description: "fresh", DownloadID: "new"}},
	}}
	// The adapter snapshot is immutable; construct a handler with the response-bearing adapter.
	base = mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{})
	handler, err = NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.mediaRecognizer = recognizer
	handler.now = func() time.Time { return now }
	if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
		t.Fatal(err)
	}
	if len(recognizer.queries) != 1 || len(store.committed.Items) != 1 || store.committed.Items[0].Title != "New" {
		t.Fatalf("recognition calls/items = %d/%#v, want 1/New", len(recognizer.queries), store.committed.Items)
	}
}

func TestFilterRSSItemsByHistoricalSignaturesMatchesV2StrictRules(t *testing.T) {
	t.Parallel()
	cached := map[string]struct{}{"OldDescription": {}}
	items := []indexer.RSSItem{
		{ItemKey: "sha256:old", Resource: indexer.Resource{Title: "Old", Description: "Description"}},
		{ItemKey: "sha256:same-batch", Resource: indexer.Resource{Title: "Movie", Description: "Description"}},
		{ItemKey: "sha256:case", Resource: indexer.Resource{Title: "movie", Description: "Description"}},
		{ItemKey: "sha256:space", Resource: indexer.Resource{Title: "Movie ", Description: "Description"}},
	}
	got := filterRSSItemsByHistoricalSignatures(cached, items)
	if len(got) != 3 || got[0].ItemKey != "sha256:same-batch" || got[1].ItemKey != "sha256:case" || got[2].ItemKey != "sha256:space" {
		t.Fatalf("filtered items = %#v, want same-batch/case/space", got)
	}
}

func TestNewRSSHandlerRejectsInvalidPollInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
	}{
		{name: "zero"},
		{name: "negative", interval: -time.Minute},
		{name: "below V2 minimum", interval: 4 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRSSFlowStore(subscriptionFlowTarget())
			adapter := rssFlowAdapter{
				subscriptionFlowAdapter: subscriptionFlowAdapter{
					id: 7, name: "rss", kind: "declarative-rss", version: 1,
				},
				key: "sha256:invalid-interval",
			}
			base := mustSubscriptionFlowHandler(
				t,
				store,
				&subscriptionFlowSearcher{},
				subscriptionFlowAdapters{adapter},
				&subscriptionFlowSubmission{},
			)
			if _, err := NewRSSHandler(
				store,
				base,
				subscriptionFlowAdapters{adapter},
				&rssFlowEnqueuer{},
				tt.interval,
			); !errors.Is(err, ErrInvalidRSSHandler) {
				t.Fatalf("NewRSSHandler(interval=%s) error = %v, want ErrInvalidRSSHandler", tt.interval, err)
			}
		})
	}
}

func TestRSSHandlerSetPollInterval(t *testing.T) {
	t.Parallel()

	handler := &RSSHandler{}
	handler.pollInterval.Store(int64(30 * time.Minute))

	if err := handler.SetPollInterval(5 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(handler.pollInterval.Load()); got != 5*time.Minute {
		t.Fatalf("poll interval = %s, want %s", got, 5*time.Minute)
	}
	if err := handler.SetPollInterval(4 * time.Minute); !errors.Is(err, ErrInvalidRSSHandler) {
		t.Fatalf("SetPollInterval() error = %v, want ErrInvalidRSSHandler", err)
	}
	if got := time.Duration(handler.pollInterval.Load()); got != 5*time.Minute {
		t.Fatalf("invalid update changed poll interval to %s", got)
	}
}

func TestRSSHandlerPollMatchesActiveSubscription(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Config["media_category"] = "欧美"
	store := newRSSFlowStore(target)
	store.cursor.NextPollAt = now.Add(-time.Second)
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1}, key: "sha256:cursor-1"}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission)
	mediaDetails := &subscriptionFlowMediaDetails{item: metadata.Media{
		Source: "themoviedb", ID: "42", VoteAverage: 8.4,
		PosterURL: "https://image.example/rss-poster.jpg", BackdropURL: "https://image.example/rss-backdrop.jpg",
	}}
	base.SetMediaDetailResolver(mediaDetails)
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 0 {
		t.Fatalf("poll submissions = %d, want 0 before global match", submission.calls)
	}
	if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: now, ExpectedSiteIDs: []int{7},
	}); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 1 || submission.subscriptionID != target.ID || submission.plan.ResourceID != "42" {
		t.Fatalf("submission = %+v", submission)
	}
	if submission.plan.MoviePriority == nil || *submission.plan.MoviePriority != 0 {
		t.Fatalf("RSS must freeze known zero movie quality: %+v", submission.plan)
	}
	if submission.successInput.IntentID != submission.intent.ID ||
		submission.successInput.Media.Source != target.MediaSource ||
		submission.successInput.Media.MediaID != target.MediaID ||
		submission.successInput.Media.Category != "欧美" ||
		submission.successInput.Media.VoteAverage != 8.4 ||
		submission.successInput.Media.PosterURL != "https://image.example/rss-poster.jpg" ||
		submission.successInput.Media.BackdropURL != "https://image.example/rss-backdrop.jpg" ||
		submission.successInput.ResourceTerm != "WEB-DL 1080p" ||
		submission.successInput.Manual || !submission.successInput.Background {
		t.Fatalf("success input = %#v", submission.successInput)
	}
	if mediaDetails.calls != 1 || mediaDetails.source != "themoviedb" || mediaDetails.id != "movie:42" {
		t.Fatalf("metadata detail = calls:%d source:%q id:%q", mediaDetails.calls, mediaDetails.source, mediaDetails.id)
	}
	if store.committed.NotModified || len(store.committed.Items) != 1 {
		t.Fatalf("committed poll = %+v", store.committed)
	}
}

func TestRSSHandlerExecutesV2TVCandidatesWithV2StateTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		state            subscription.State
		wantPreciseStage int
		wantRSSStages    []subscription.State
	}{
		{
			name:             "R uses RSS stage once then precise stages",
			state:            subscription.Active,
			wantPreciseStage: 1,
			wantRSSStages:    []subscription.State{subscription.Active},
		},
		{
			name:          "P remains pending for every candidate",
			state:         subscription.PendingIdentification,
			wantRSSStages: []subscription.State{subscription.PendingIdentification, subscription.PendingIdentification},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			season := 1
			target := subscriptionFlowTarget()
			target.InternalState = tt.state
			target.MediaType = string(media.TypeTV)
			target.MediaSource = "themoviedb"
			target.MediaID = "42"
			target.Season = &season
			target.Config["total_episode"] = float64(2)
			target.Config["start_episode"] = float64(1)
			store := newRSSFlowStore(target)
			store.episodes = []subscription.SubscriptionEpisode{
				{SubscriptionID: target.ID, Season: 1, Episode: 1, State: subscription.SubscriptionEpisodeMissing},
				{SubscriptionID: target.ID, Season: 1, Episode: 2, State: subscription.SubscriptionEpisodeMissing},
			}
			adapter := rssFlowAdapter{
				subscriptionFlowAdapter: subscriptionFlowAdapter{
					id: 7, name: "rss", kind: "declarative-rss", version: 1,
					body: v2TVTestMetainfo(t, "Example.Movie.S01E01.mkv", "Example.Movie.S01E02.mkv"),
				},
				key: "sha256:v2-tv-state",
			}
			submission := &subscriptionFlowSubmission{}
			execution := &subscriptionFlowExecution{submission: submission, store: store.subscriptionFlowStore}
			base, err := NewHandler(
				store,
				&subscriptionFlowSearcher{},
				subscriptionFlowAdapters{adapter},
				submission,
				execution,
			)
			if err != nil {
				t.Fatal(err)
			}
			setSubscriptionFlowTVMetadata(base, target)
			handler, err := NewRSSHandler(
				store,
				base,
				subscriptionFlowAdapters{adapter},
				&rssFlowEnqueuer{},
			)
			if err != nil {
				t.Fatal(err)
			}
			resources := map[int][]indexer.Resource{7: {
				{SiteID: 7, Site: "rss", Title: "Example.Movie.S01E01.2026.1080p.WEB-DL", DownloadID: "41", Seeders: 20},
				{SiteID: 7, Site: "rss", Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42", Seeders: 10},
			}}

			matched, err := handler.matchRSSTarget(t.Context(), rssSources([]indexer.Adapter{adapter}), resources, target)
			if err != nil {
				t.Fatal(err)
			}
			if !matched {
				t.Fatal("matchRSSTarget() = false, want true")
			}
			if submission.calls != 0 {
				t.Fatalf("async submissions = %d, want 0", submission.calls)
			}
			if submission.stageCalls != tt.wantPreciseStage ||
				!reflect.DeepEqual(submission.rssStates, tt.wantRSSStages) {
				t.Fatalf(
					"stages = precise:%d rss:%v, want precise:%d rss:%v",
					submission.stageCalls,
					submission.rssStates,
					tt.wantPreciseStage,
					tt.wantRSSStages,
				)
			}
			if len(execution.calls) != 2 || len(store.episodes) != 0 {
				t.Fatalf("execution calls = %d, remaining episodes = %#v", len(execution.calls), store.episodes)
			}
		})
	}
}

func TestRSSHandlerPollHandlesUnresolvedAndEmptyTVTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		resolver      *subscriptionFlowMediaDetails
		wantComplete  int
		wantReadCalls int
	}{
		{name: "metadata resolver unavailable"},
		{
			name: "metadata detail fails", resolver: &subscriptionFlowMediaDetails{err: errors.New("metadata unavailable")},
			wantReadCalls: 1,
		},
		{
			name: "metadata detail has no episodes", resolver: &subscriptionFlowMediaDetails{item: metadata.Media{Type: "tv"}},
			wantComplete: 1, wantReadCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
			target := subscriptionFlowTarget()
			target.InternalState = subscription.Active
			target.MediaType = string(media.TypeTV)
			target.MediaSource = "themoviedb"
			target.MediaID = "42"
			target.Config["total_episode"] = float64(0)
			store := newRSSFlowStore(target)
			store.cursor.NextPollAt = now.Add(-time.Second)
			adapter := rssFlowAdapter{
				subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
				key:                     "sha256:cursor-empty-tv",
				response: indexer.RSSResponse{Items: []indexer.RSSItem{{
					ItemKey: "sha256:item-empty-tv",
					Resource: indexer.Resource{
						SiteID: 7, Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42",
					},
				}}},
			}
			submission := &subscriptionFlowSubmission{}
			base := mustSubscriptionFlowHandler(
				t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
			)
			if tt.resolver != nil {
				base.SetMediaDetailResolver(tt.resolver)
			}
			handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
			if err != nil {
				t.Fatal(err)
			}
			handler.now = func() time.Time { return now }

			if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
				t.Fatal(err)
			}
			if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
				Key: "global", Cutoff: now, ExpectedSiteIDs: []int{7},
			}); err != nil {
				t.Fatal(err)
			}
			if submission.calls != 0 || len(submission.episodes) != 0 {
				t.Fatalf("unresolved TV target submission = calls:%d episodes:%#v", submission.calls, submission.episodes)
			}
			if len(store.upserted) != 0 {
				t.Fatalf("unresolved TV target persisted episodes = %#v", store.upserted)
			}
			if store.completeCalls != tt.wantComplete || store.casCalls != 0 {
				t.Fatalf("RSS state writes = completions:%d CAS:%d", store.completeCalls, store.casCalls)
			}
			if tt.wantComplete > 0 && (store.completeState != subscription.Active ||
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

func TestRSSHandlerPollSubmitsKnownTVEpisodes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	season := 1
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(2)
	target.Config["start_episode"] = float64(1)
	store := newRSSFlowStore(target)
	store.reflectUpserts = true
	store.cursor.NextPollAt = now.Add(-time.Second)
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
		key:                     "sha256:cursor-known-tv",
		response: indexer.RSSResponse{Items: []indexer.RSSItem{{
			ItemKey: "sha256:item-known-tv",
			Resource: indexer.Resource{
				SiteID: 7, Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42",
			},
		}}},
	}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	setSubscriptionFlowTVMetadata(base, target)
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }

	if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: now, ExpectedSiteIDs: []int{7},
	}); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 1 || len(submission.episodes) != 1 ||
		submission.episodes[0] != (subscription.Episode{Season: 1, Episode: 2}) {
		t.Fatalf("known TV submission = calls:%d episodes:%#v", submission.calls, submission.episodes)
	}
}

func TestRSSHandlerPollSubmitsPendingIdentificationWithoutCompleting(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC)
	season := 1
	target := subscriptionFlowTarget()
	target.InternalState = subscription.PendingIdentification
	target.V2State = "P"
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(2)
	target.Config["start_episode"] = float64(1)
	store := newRSSFlowStore(target)
	store.reflectUpserts = true
	store.cursor.NextPollAt = now.Add(-time.Second)
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{
			id: 7, name: "rss", kind: "declarative-rss", version: 1,
		},
		key: "sha256:cursor-pending-identification",
		response: indexer.RSSResponse{Items: []indexer.RSSItem{{
			ItemKey: "sha256:item-pending-identification",
			Resource: indexer.Resource{
				SiteID: 7, Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42",
			},
		}}},
	}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	setSubscriptionFlowTVMetadata(base, target)
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }

	if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: now, ExpectedSiteIDs: []int{7},
	}); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 1 || submission.rssState != subscription.PendingIdentification ||
		len(submission.episodes) != 1 ||
		submission.episodes[0] != (subscription.Episode{Season: 1, Episode: 2}) {
		t.Fatalf(
			"pending-identification submission = calls:%d state:%q episodes:%#v",
			submission.calls, submission.rssState, submission.episodes,
		)
	}
	if store.completeCalls != 0 {
		t.Fatalf(
			"pending-identification result = completions:%d",
			store.completeCalls,
		)
	}
}

func TestRSSHandlerMatchSubmitsForEveryMatchingOwner(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	first := subscriptionFlowTarget()
	first.ID = 31
	first.Username = "alice"
	first.InternalState = subscription.Active
	first.V2State = "R"
	first.UpdatedAt = cutoff.Add(-2 * time.Minute)
	second := first
	second.ID = 32
	second.Username = "bob"
	second.InternalState = subscription.PendingIdentification
	second.V2State = "P"
	second.UpdatedAt = cutoff.Add(-time.Minute)

	store := newRSSFlowStore(first)
	store.list = func(_ context.Context, _ time.Time, after time.Time, afterID int64, _ int) ([]database.Subscription, error) {
		if after.IsZero() && afterID == 0 {
			return []database.Subscription{first, second}, nil
		}
		return nil, nil
	}
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{
		id: 7, name: "rss", kind: "declarative-rss", version: 1,
	}, key: "sha256:multi-owner"}
	store.items = []database.RSSItem{{
		ID: 7, SiteID: 7, ItemKey: "sha256:multi-owner",
		Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42", Resource: []byte(`{}`),
	}}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}

	err = handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if submission.calls != 2 ||
		!reflect.DeepEqual(submission.rssUsernames, []string{"alice", "bob"}) ||
		!reflect.DeepEqual(submission.rssIDs, []int64{31, 32}) ||
		!reflect.DeepEqual(submission.rssStates, []subscription.State{
			subscription.Active, subscription.PendingIdentification,
		}) {
		t.Fatalf(
			"multi-owner match = calls:%d users:%v ids:%v states:%v",
			submission.calls, submission.rssUsernames, submission.rssIDs, submission.rssStates,
		)
	}
}

func TestRSSHandlerMatchScansAllSubscriptions(t *testing.T) {
	t.Parallel()
	const oldTargetBound int64 = 10_000
	cutoff := time.Date(2026, 8, 16, 12, 15, 0, 0, time.UTC)
	tests := []struct {
		name          string
		total         int64
		matchingID    int64
		wantSubmitted []int64
	}{
		{
			name:  "exactly old target bound completes without failure",
			total: oldTargetBound,
		},
		{
			name:          "target after old bound remains reachable",
			total:         oldTargetBound + 1,
			matchingID:    oldTargetBound + 1,
			wantSubmitted: []int64{oldTargetBound + 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRSSFlowStore(subscriptionFlowTarget())
			store.list = func(
				_ context.Context,
				_ time.Time,
				_ time.Time,
				afterID int64,
				limit int,
			) ([]database.Subscription, error) {
				if limit <= 0 {
					t.Fatalf("ListSubscriptionsForRSS() limit = %d, want positive", limit)
				}
				firstID := afterID + 1
				if firstID > tt.total {
					return nil, nil
				}
				lastID := min(firstID+int64(limit)-1, tt.total)
				targets := make([]database.Subscription, 0, lastID-firstID+1)
				for id := firstID; id <= lastID; id++ {
					target := database.Subscription{ID: id, UpdatedAt: cutoff.Add(-time.Minute)}
					if id == tt.matchingID {
						target = subscriptionFlowTarget()
						target.ID = id
						target.Username = "last-owner"
						target.InternalState = subscription.Active
						target.V2State = "R"
						target.UpdatedAt = cutoff.Add(-time.Minute)
					}
					targets = append(targets, target)
				}
				return targets, nil
			}
			adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{
				id: 7, name: "rss", kind: "declarative-rss", version: 1,
			}, key: "sha256:all-subscriptions"}
			store.items = []database.RSSItem{{
				ID: 9, SiteID: 7, ItemKey: "sha256:all-subscriptions",
				Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42", Resource: []byte(`{}`),
			}}
			submission := &subscriptionFlowSubmission{}
			base := mustSubscriptionFlowHandler(
				t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
			)
			handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
			if err != nil {
				t.Fatal(err)
			}

			err = handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
				Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7},
			})
			if err != nil || !reflect.DeepEqual(submission.rssIDs, tt.wantSubmitted) {
				t.Fatalf(
					"all-subscription scan = error:%v submitted:%v, want submitted:%v",
					err, submission.rssIDs, tt.wantSubmitted,
				)
			}
		})
	}
}

func TestRSSHandlerMatchRetriesOnlyRemainingOwnerAfterPartialFailure(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 12, 30, 0, 0, time.UTC)
	first := subscriptionFlowTarget()
	first.ID = 41
	first.Username = "alice"
	first.InternalState = subscription.Active
	first.V2State = "R"
	first.UpdatedAt = cutoff.Add(-2 * time.Minute)
	second := first
	second.ID = 42
	second.Username = "bob"
	second.UpdatedAt = cutoff.Add(-time.Minute)

	submission := &rssPartialFailureSubmission{successful: make(map[int64]bool)}
	store := newRSSFlowStore(first)
	store.list = func(_ context.Context, _ time.Time, after time.Time, afterID int64, _ int) ([]database.Subscription, error) {
		if !after.IsZero() || afterID != 0 {
			return nil, nil
		}
		result := make([]database.Subscription, 0, 2)
		for _, target := range []database.Subscription{first, second} {
			if !submission.successful[target.ID] {
				result = append(result, target)
			}
		}
		return result, nil
	}
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{
		id: 7, name: "rss", kind: "declarative-rss", version: 1,
	}, key: "sha256:partial-owner"}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	store.items = []database.RSSItem{{
		ID: 8, SiteID: 7, ItemKey: "sha256:partial-owner",
		Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42", Resource: []byte(`{}`),
	}}
	args := jobs.RSSMatchArgs{Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7}}

	err = handler.HandleRSSMatch(t.Context(), args)
	if !errors.Is(err, ErrRSSSubmission) ||
		!reflect.DeepEqual(submission.attempts, []int64{41, 42}) || !submission.successful[41] {
		t.Fatalf("first pass = error:%v attempts:%v successful:%v", err, submission.attempts, submission.successful)
	}
	err = handler.HandleRSSMatch(t.Context(), args)
	if err != nil ||
		!reflect.DeepEqual(submission.attempts, []int64{41, 42, 42}) || !submission.successful[42] {
		t.Fatalf("retry pass = error:%v attempts:%v successful:%v", err, submission.attempts, submission.successful)
	}
}

func TestRSSHandlerPollIgnoresEpisodeAlreadyInMediaServer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 13, 0, 0, 0, time.UTC)
	season := 1
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(2)
	target.Config["start_episode"] = float64(1)
	store := newRSSFlowStore(target)
	store.reflectUpserts = true
	store.cursor.NextPollAt = now.Add(-time.Second)
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
		key:                     "sha256:cursor-library-tv",
		response: indexer.RSSResponse{Items: []indexer.RSSItem{{
			ItemKey: "sha256:item-library-tv",
			Resource: indexer.Resource{
				SiteID: 7, Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42",
			},
		}}},
	}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	setSubscriptionFlowTVMetadata(base, target)
	base.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: []subscription.Episode{
		{Season: 1, Episode: 2},
	}})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }

	if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now}); err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: now, ExpectedSiteIDs: []int{7},
	}); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 0 {
		t.Fatalf("existing episode result = submission:%d", submission.calls)
	}
}

func TestRSSHandlerMatchCompletesLibrarySatisfiedSubscription(t *testing.T) {
	t.Parallel()
	season := 1
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(2)
	store := newRSSFlowStore(target)
	store.reflectUpserts = true
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{
			id: 7, name: "rss", kind: "declarative-rss", version: 1,
		},
		key: "sha256:satisfied-tv",
	}
	store.items = []database.RSSItem{{
		ID: 7, SiteID: 7, ItemKey: "sha256:satisfied-tv",
		Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42", Resource: []byte(`{}`),
	}}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	setSubscriptionFlowTVMetadata(base, target)
	base.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
	}})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}

	err = handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: target.UpdatedAt.Add(time.Minute), ExpectedSiteIDs: []int{7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if submission.calls != 0 || store.completeCalls != 1 ||
		store.completeOwner != target.Username || store.completeID != target.ID ||
		store.completeState != subscription.Active || !store.completeTime.Equal(target.UpdatedAt) {
		t.Fatalf(
			"satisfied RSS target = submission:%d complete:%d owner:%q id:%d state:%s time:%s",
			submission.calls, store.completeCalls, store.completeOwner, store.completeID,
			store.completeState, store.completeTime,
		)
	}
}

func TestRSSHandlerMatchPreservesSatisfiedPendingIdentification(t *testing.T) {
	t.Parallel()
	season := 1
	target := subscriptionFlowTarget()
	target.InternalState = subscription.PendingIdentification
	target.V2State = "P"
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(2)
	store := newRSSFlowStore(target)
	store.reflectUpserts = true
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{
		id: 7, name: "rss", kind: "declarative-rss", version: 1,
	}, key: "sha256:satisfied-pending"}
	store.items = []database.RSSItem{{
		ID: 7, SiteID: 7, ItemKey: "sha256:satisfied-pending",
		Title: "Example.Movie.S01E02.2026.1080p.WEB-DL", DownloadID: "42", Resource: []byte(`{}`),
	}}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	setSubscriptionFlowTVMetadata(base, target)
	base.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
	}})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}

	err = handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: target.UpdatedAt.Add(time.Minute), ExpectedSiteIDs: []int{7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if submission.calls != 0 || store.completeCalls != 0 {
		t.Fatalf(
			"satisfied pending-identification = submission:%d completion:%d",
			submission.calls, store.completeCalls,
		)
	}
	if store.progressCalls != 1 || store.progressOwner != target.Username || store.progressID != target.ID ||
		store.progressState != subscription.PendingIdentification || !store.progressTime.Equal(target.UpdatedAt) ||
		store.casCalls != 0 {
		t.Fatalf(
			"pending-identification progress = calls:%d owner:%q id:%d state:%s time:%s cas:%d",
			store.progressCalls, store.progressOwner, store.progressID, store.progressState,
			store.progressTime, store.casCalls,
		)
	}
}

func TestRSSHandlerMatchHandlesExistingMovieByV2State(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		state            subscription.State
		v2State          string
		useLibrary       bool
		config           map[string]any
		wantCompletions  int
		wantLibraryCalls int
	}{
		{
			name: "R library hit completes movie", state: subscription.Active, v2State: "R",
			useLibrary: true, config: map[string]any{}, wantCompletions: 1, wantLibraryCalls: 1,
		},
		{
			name: "P library hit remains pending identification", state: subscription.PendingIdentification, v2State: "P",
			useLibrary: true, config: map[string]any{}, wantLibraryCalls: 1,
		},
		{
			name: "R download note completes movie", state: subscription.Active, v2State: "R",
			config: map[string]any{"note": []any{float64(1)}}, wantCompletions: 1,
		},
		{
			name: "R best movie accepted 100 completes without library", state: subscription.Active, v2State: "R",
			config: map[string]any{"best_version": true, "current_priority": 100}, wantCompletions: 1,
		},
		{
			name: "P best movie accepted 100 remains pending", state: subscription.PendingIdentification, v2State: "P",
			config: map[string]any{"best_version": true, "current_priority": 100},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := subscriptionFlowTarget()
			target.InternalState = tt.state
			target.V2State = tt.v2State
			target.MediaType = string(media.TypeMovie)
			target.MediaSource = "themoviedb"
			target.MediaID = "42"
			for key, value := range tt.config {
				target.Config[key] = value
			}
			store := newRSSFlowStore(target)
			adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{
				id: 7, name: "rss", kind: "declarative-rss", version: 1,
			}, key: "sha256:existing-movie"}
			store.items = []database.RSSItem{{
				ID: 7, SiteID: 7, ItemKey: "sha256:existing-movie",
				Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42", Resource: []byte(`{}`),
			}}
			submission := &subscriptionFlowSubmission{}
			base := mustSubscriptionFlowHandler(
				t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
			)
			var source *existingMovieSourceFake
			if tt.useLibrary {
				source = &existingMovieSourceFake{found: true}
				base.SetExistingMovieSource(source)
			}
			handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
			if err != nil {
				t.Fatal(err)
			}

			err = handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
				Key: "global", Cutoff: target.UpdatedAt.Add(time.Minute), ExpectedSiteIDs: []int{7},
			})
			if err != nil || submission.calls != 0 || store.completeCalls != tt.wantCompletions {
				t.Fatalf(
					"existing movie = error:%v submission:%d completion:%d",
					err, submission.calls, store.completeCalls,
				)
			}
			if tt.wantCompletions > 0 && (store.completeState != tt.state || store.completeType != media.TypeMovie) {
				t.Fatalf("completion = state:%s type:%s", store.completeState, store.completeType)
			}
			if source != nil && source.calls != tt.wantLibraryCalls {
				t.Fatalf("library calls = %d, want %d", source.calls, tt.wantLibraryCalls)
			}
		})
	}
}

func TestRSSHandlerMatchRetriesStaleSatisfiedSnapshot(t *testing.T) {
	t.Parallel()
	season := 1
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.V2State = "R"
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	target.Season = &season
	target.Config["total_episode"] = float64(1)
	store := newRSSFlowStore(target)
	store.reflectUpserts = true
	store.completeErr = database.ErrSubscriptionPreciseSearchStateConflict
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{
		id: 7, name: "rss", kind: "declarative-rss", version: 1,
	}, key: "sha256:stale-satisfied"}
	store.items = []database.RSSItem{{
		ID: 7, SiteID: 7, ItemKey: "sha256:stale-satisfied",
		Title: "Example.Movie.S01E01.2026.1080p.WEB-DL", DownloadID: "42", Resource: []byte(`{}`),
	}}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{},
	)
	setSubscriptionFlowTVMetadata(base, target)
	base.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: []subscription.Episode{{Season: 1, Episode: 1}}})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}

	err = handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: target.UpdatedAt.Add(time.Minute), ExpectedSiteIDs: []int{7},
	})
	if !errors.Is(err, ErrRSSMatch) || store.completeCalls != 1 {
		t.Fatalf("stale satisfied snapshot = error:%v completion:%d", err, store.completeCalls)
	}
}

func TestRSSHandlerPollMapsLeaseContentionToSnoozeSentinel(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.leaseErr = database.ErrRSSLeaseConflict
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1}, key: "sha256:cursor-1"}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	err = handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now})
	if !errors.Is(err, jobs.ErrRSSLeaseBusy) {
		t.Fatalf("HandleRSSPoll() error = %v, want ErrRSSLeaseBusy", err)
	}
}

func TestRSSHandlerRejects304WithoutValidator(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.cursor.NextPollAt = now.Add(-time.Second)
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
		key:                     "sha256:cursor-1", response: indexer.RSSResponse{NotModified: true},
	}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	err = handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: now})
	if !errors.Is(err, ErrRSSFetch) || !errors.Is(err, jobs.ErrRSSPollFailureRecorded) || store.failedPolls != 1 {
		t.Fatalf("HandleRSSPoll() = %v, failed polls=%d", err, store.failedPolls)
	}
}

func TestRSSHandlerPollRetriesCacheTrimAfterCursorAdvanced(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 11, 30, 0, 0, time.UTC)
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.cursor.NextPollAt = now.Add(-time.Second)
	store.acquireResults = []bool{true, false}
	store.trimDeletes = []int{0, rssCacheTrimBatch, 0}
	store.trimErrors = []error{errors.New("database unavailable"), nil, nil}
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
		key:                     "sha256:trim-retry",
	}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{},
	)
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	args := jobs.RSSPollArgs{SiteID: 7, Cutoff: now}

	if err := handler.HandleRSSPoll(t.Context(), args); !errors.Is(err, ErrRSSStorage) {
		t.Fatalf("first HandleRSSPoll() error = %v, want ErrRSSStorage", err)
	}
	if err := handler.HandleRSSPoll(t.Context(), args); err != nil {
		t.Fatalf("retry HandleRSSPoll() error = %v", err)
	}
	if store.commitCalls != 1 || store.trimCalls != 3 {
		t.Fatalf("poll retry calls = commit:%d trim:%d, want 1/3", store.commitCalls, store.trimCalls)
	}
}

func TestRSSHandlerSweepEnqueuesDueSites(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.cursor.NextPollAt = now.Add(-time.Second)
	first := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1}, key: "sha256:cursor-1"}
	second := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 8, name: "rss-2", kind: "declarative-rss", version: 1}, key: "sha256:cursor-2"}
	enqueuer := &rssFlowEnqueuer{}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{first, second}, &subscriptionFlowSubmission{})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{first, second}, enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	if _, err := handler.HandleRSSSweep(t.Context(), now, jobs.RSSSweepState{}); err != nil {
		t.Fatal(err)
	}
	if len(enqueuer.args) != 1 || enqueuer.args[0].SiteID != 7 {
		t.Fatalf("enqueued RSS polls = %+v", enqueuer.args)
	}
	if store.ensureEndpoint != "sha256:cursor-2" && store.ensureEndpoint != "sha256:cursor-1" {
		t.Fatalf("cursor endpoint was not opaque: %q", store.ensureEndpoint)
	}
	if !store.recoverAt.Equal(now) {
		t.Fatalf("lease recovery time = %s, want current time %s", store.recoverAt, now)
	}
}

func TestRSSHandlerSweepUsesCurrentTimeForDelayedJob(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 30, 0, 0, time.UTC)
	cutoff := now.Add(-20 * time.Minute)
	store := newRSSFlowStore(subscriptionFlowTarget())
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1}, key: "sha256:cursor-1"}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	if _, err := handler.HandleRSSSweep(t.Context(), cutoff, jobs.RSSSweepState{}); err != nil {
		t.Fatal(err)
	}
	if !store.recoverAt.Equal(now) {
		t.Fatalf("lease recovery time = %s, want %s", store.recoverAt, now)
	}
}

func TestRSSHandlerMatchReturnsCacheTrimFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := newRSSFlowStore(subscriptionFlowTarget())
	store.cleanupErr = errors.New("database unavailable")
	adapter := rssFlowAdapter{subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1}, key: "sha256:cursor-1"}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, &subscriptionFlowSubmission{})
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	err = handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: now, ExpectedSiteIDs: []int{7},
	})
	if !errors.Is(err, ErrRSSStorage) {
		t.Fatalf("HandleRSSMatch() error = %v, want ErrRSSStorage", err)
	}
}

func TestBuildRSSPollResultSeparatesSecretLocatorFromPublicURLs(t *testing.T) {
	t.Parallel()
	locator := "https://tracker.example/download/42?passkey=fixture#fragment"
	response := indexer.RSSResponse{Items: []indexer.RSSItem{{
		ItemKey: "sha256:item-secret",
		Resource: indexer.Resource{
			SiteID:      7,
			Title:       "Fixture.Release.2026.1080p",
			DownloadID:  locator,
			DetailURL:   "https://reader:password@tracker.example/details/42?token=fixture#comments",
			DownloadURL: locator,
		},
	}}}
	result, err := buildRSSPollResult(database.RSSCursor{}, response)
	if err != nil {
		t.Fatalf("buildRSSPollResult() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("buildRSSPollResult() items = %d, want 1", len(result.Items))
	}
	item := result.Items[0]
	if !strings.HasPrefix(item.DownloadID, "sha256:") || item.ResourceSecret != locator {
		t.Fatalf("protected locator = (%q, %q)", item.DownloadID, item.ResourceSecret)
	}
	if item.DetailURL != "https://tracker.example/details/42" || item.DownloadURL != "https://tracker.example/download/42" {
		t.Fatalf("public URLs = (%q, %q)", item.DetailURL, item.DownloadURL)
	}
	stored := string(item.Resource)
	for _, secret := range []string{"fixture", "password", "passkey=", "token="} {
		if strings.Contains(stored, secret) {
			t.Fatalf("stored resource contains secret marker %q: %s", secret, stored)
		}
	}
}

func TestRSSResourceSnapshotPreservesV2MediaTitleWithoutPublicResourceField(t *testing.T) {
	t.Parallel()
	response := indexer.RSSResponse{Items: []indexer.RSSItem{{
		ItemKey: "sha256:media-title",
		Resource: indexer.Resource{
			SiteID:               7,
			Title:                "Release.S01E01",
			DownloadID:           "https://tracker.example/download/42",
			RecognizedMediaTitle: "Canonical Show",
		},
	}}}
	result, err := buildRSSPollResult(database.RSSCursor{}, response)
	if err != nil {
		t.Fatalf("buildRSSPollResult() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("buildRSSPollResult() items = %d, want 1", len(result.Items))
	}
	stored := string(result.Items[0].Resource)
	if !strings.Contains(stored, `"_v2_media_title":"Canonical Show"`) {
		t.Fatalf("stored RSS resource lost V2 media title: %s", stored)
	}
	var public indexer.Resource
	if err := json.Unmarshal(result.Items[0].Resource, &public); err != nil {
		t.Fatalf("json.Unmarshal(public resource) error = %v", err)
	}
	if public.RecognizedMediaTitle != "" {
		t.Fatalf("internal media title leaked through public resource model: %q", public.RecognizedMediaTitle)
	}
	restored, ok := restoreRSSResource(database.RSSItem{
		SiteID:     7,
		ItemKey:    "sha256:media-title",
		Title:      "Release.S01E01",
		DownloadID: "https://tracker.example/download/42",
		Resource:   result.Items[0].Resource,
	})
	if !ok || restored.RecognizedMediaTitle != "Canonical Show" {
		t.Fatalf("restoreRSSResource() title = (%q, %v), want Canonical Show", restored.RecognizedMediaTitle, ok)
	}
}

func TestBuildRSSPollResultSortsAndCapsV2RefreshWindow(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	items := make([]indexer.RSSItem, 0, rssMaxRefreshItems+2)
	for index := 0; index < rssMaxRefreshItems+2; index++ {
		value := strconv.Itoa(index)
		items = append(items, indexer.RSSItem{
			ItemKey: "sha256:refresh-" + value,
			Resource: indexer.Resource{
				Title:       "Fixture.Release." + value,
				DownloadID:  "https://tracker.example/download/" + value,
				PublishedAt: base.Add(time.Duration(index) * time.Minute),
			},
		})
	}

	result, err := buildRSSPollResult(database.RSSCursor{}, indexer.RSSResponse{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != rssMaxRefreshItems {
		t.Fatalf("refresh items = %d, want %d", len(result.Items), rssMaxRefreshItems)
	}
	if result.Items[0].Title != "Fixture.Release.51" || result.Items[len(result.Items)-1].Title != "Fixture.Release.2" {
		t.Fatalf(
			"refresh window = first:%q last:%q, want newest 50 items",
			result.Items[0].Title,
			result.Items[len(result.Items)-1].Title,
		)
	}
}

func TestRestoreRSSResourceUsesDecryptedLocator(t *testing.T) {
	t.Parallel()
	locator := "https://tracker.example/download/42?passkey=fixture"
	resource, ok := restoreRSSResource(database.RSSItem{
		SiteID:         7,
		ItemKey:        "sha256:item-secret",
		Title:          "Fixture.Release.2026.1080p",
		DownloadID:     "sha256:opaque",
		ResourceSecret: locator,
		DetailURL:      "https://tracker.example/details/42",
		DownloadURL:    "https://tracker.example/download/42",
		Resource:       []byte(`{"title":"Fixture.Release.2026.1080p"}`),
	})
	if !ok {
		t.Fatal("restoreRSSResource() rejected valid item")
	}
	if resource.DownloadID != locator {
		t.Fatalf("restoreRSSResource() DownloadID = %q, want decrypted locator", resource.DownloadID)
	}
}

const rssFlowItemToken = "22222222-2222-4222-8222-222222222222"

var rssFlowItemUUID = uuid.MustParse(rssFlowItemToken)

type rssFlowStore struct {
	*subscriptionFlowStore
	rssSitesSnapshot   database.SubscriptionRSSSitesSnapshot
	rssSites           []int
	rssSitesFound      bool
	rssSitesErr        error
	rssSnapshotCalls   int
	rssSettingCalls    int
	cursor             database.RSSCursor
	items              []database.RSSItem
	committed          database.RSSPollResult
	commitReplaceCache bool
	completedID        int64
	completedState     string
	ensureEndpoint     string
	leaseErr           error
	acquireResults     []bool
	commitCalls        int
	nextPollAt         time.Time
	failedPolls        int
	recoverAt          time.Time
	cleanupErr         error
	trimDeletes        []int
	trimErrors         []error
	trimCalls          int
}

func newRSSFlowStore(target database.Subscription) *rssFlowStore {
	base := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			projected, err := subscription.V2State(target.InternalState)
			if err != nil || (projected != "R" && projected != "P") {
				return nil, nil
			}
			return []database.Subscription{target}, nil
		},
		downloaders: []downloadconfig.Downloader{{ID: subscriptionFlowDownloaderID, Type: "qbittorrent", Enabled: true, Default: true}},
	}
	return &rssFlowStore{
		subscriptionFlowStore: base,
		rssSitesSnapshot: database.SubscriptionRSSSitesSnapshot{
			HasSubscriptions: true,
			SiteSelections:   [][]int{configIntSlice(target.Config, "sites")},
		},
		cursor: database.RSSCursor{SiteID: 7, Endpoint: "sha256:cursor-1", NextPollAt: time.Now().UTC()},
	}
}

func (s *rssFlowStore) GetSubscriptionRSSSites(context.Context) (database.SubscriptionRSSSitesSnapshot, error) {
	s.rssSnapshotCalls++
	return s.rssSitesSnapshot, s.rssSitesErr
}

func (s *rssFlowStore) GetSetting(ctx context.Context, key string, target any) (bool, error) {
	if key != rssSitesSetting {
		return s.subscriptionFlowStore.GetSetting(ctx, key, target)
	}
	s.rssSettingCalls++
	if s.rssSitesErr != nil {
		return false, s.rssSitesErr
	}
	destination, ok := target.(*[]int)
	if !ok {
		return false, errors.New("unexpected RSS setting target")
	}
	*destination = append([]int(nil), s.rssSites...)
	return s.rssSitesFound, nil
}

func (s *rssFlowStore) EnsureRSSCursor(_ context.Context, siteID int64, endpoint string) (database.RSSCursor, error) {
	s.ensureEndpoint = endpoint
	if s.cursor.SiteID == 0 {
		s.cursor.SiteID = siteID
		s.cursor.Endpoint = endpoint
	}
	return s.cursor, nil
}

func (s *rssFlowStore) ListDueRSSCursors(_ context.Context, _ time.Time, _ int) ([]database.RSSCursor, error) {
	return []database.RSSCursor{s.cursor}, nil
}

func (s *rssFlowStore) TryAcquireRSSLease(_ context.Context, _ int64, _ string, _ time.Time, _ time.Duration) (database.RSSCursor, bool, error) {
	if s.leaseErr != nil {
		return database.RSSCursor{}, false, s.leaseErr
	}
	if len(s.acquireResults) > 0 {
		acquired := s.acquireResults[0]
		s.acquireResults = s.acquireResults[1:]
		if !acquired {
			return s.cursor, false, nil
		}
	}
	s.cursor.LeaseToken = rssFlowItemUUID
	return s.cursor, true, nil
}

func (s *rssFlowStore) CommitRSSPoll(_ context.Context, _ int64, _ string, _ uuid.UUID, result database.RSSPollResult, nextPollAt time.Time) error {
	s.commitCalls++
	s.committed = result
	s.commitReplaceCache = result.ReplaceCache
	s.nextPollAt = nextPollAt
	for _, input := range result.Items {
		s.items = append(s.items, database.RSSItem{
			ID: int64(len(s.items) + 1), SiteID: 7, ItemKey: input.ItemKey, Title: input.Title,
			DownloadID: input.DownloadID, ResourceSecret: input.ResourceSecret,
			DetailURL: input.DetailURL, DownloadURL: input.DownloadURL,
			Resource: input.Resource, RecognizedMediaTitle: input.RecognizedMediaTitle,
			MediaRecognizeFailCount: input.MediaRecognizeFailCount,
			LeaseToken:              rssFlowItemUUID,
		})
	}
	return nil
}

func (s *rssFlowStore) FailRSSPoll(context.Context, int64, string, uuid.UUID, time.Time, string) error {
	s.failedPolls++
	return nil
}

func (s *rssFlowStore) ClaimRSSItems(_ context.Context, _ int64, _ int, _ time.Duration) ([]database.RSSItem, error) {
	items := append([]database.RSSItem(nil), s.items...)
	s.items = nil
	return items, nil
}

func (s *rssFlowStore) CompleteRSSItem(_ context.Context, itemID int64, _ uuid.UUID, state, _ string) error {
	s.completedID = itemID
	s.completedState = state
	return nil
}

func (s *rssFlowStore) UpdateRSSItemMediaRecognition(_ context.Context, itemID int64, canonicalTitle string) error {
	for index := range s.items {
		if s.items[index].ID != itemID {
			continue
		}
		if strings.TrimSpace(canonicalTitle) != "" {
			s.items[index].RecognizedMediaTitle = strings.TrimSpace(canonicalTitle)
			s.items[index].MediaRecognizeFailCount = 0
		} else if s.items[index].MediaRecognizeFailCount < 3 {
			s.items[index].MediaRecognizeFailCount++
		}
		return nil
	}
	return nil
}

func (s *rssFlowStore) RecoverExpiredRSSLeases(_ context.Context, now time.Time, _ int) (int, error) {
	s.recoverAt = now
	return 0, nil
}
func (s *rssFlowStore) CleanupProcessedRSSItems(context.Context, int64, time.Time, int) (int, error) {
	return 0, s.cleanupErr
}

func (s *rssFlowStore) ListSubscriptionsForRSS(ctx context.Context, cutoff, after time.Time, afterID int64, limit int) ([]database.Subscription, error) {
	return s.ListSubscriptionsForPreciseSearch(ctx, cutoff, after, afterID, limit)
}

type rssFlowAdapter struct {
	subscriptionFlowAdapter
	key      string
	response indexer.RSSResponse
	noCache  bool
}

func (a rssFlowAdapter) RSSCursorKey() string { return a.key }
func (a rssFlowAdapter) RSSNoCache() bool     { return a.noCache }
func (a rssFlowAdapter) FetchRSS(context.Context, indexer.RSSRequest) (indexer.RSSResponse, error) {
	if a.response.Items == nil && !a.response.NotModified {
		return indexer.RSSResponse{Items: []indexer.RSSItem{{ItemKey: "sha256:item-1", Resource: indexer.Resource{SiteID: a.id, Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "42"}}}}, nil
	}
	return a.response, nil
}

type rssFlowEnqueuer struct {
	args         []jobs.RSSPollArgs
	matchCutoff  time.Time
	matchSiteIDs []int
	matchCalls   int
}

func (e *rssFlowEnqueuer) EnqueueRSSPoll(_ context.Context, args jobs.RSSPollArgs) error {
	e.args = append(e.args, args)
	return nil
}

func (e *rssFlowEnqueuer) EnqueueRSSMatch(_ context.Context, cutoff time.Time, siteIDs []int) error {
	e.matchCalls++
	e.matchCutoff = cutoff
	e.matchSiteIDs = append([]int(nil), siteIDs...)
	return nil
}

type rssPartialFailureSubmission struct {
	attempts   []int64
	successful map[int64]bool
	failedBob  bool
}

func (s *rssPartialFailureSubmission) SubmitSubscriptionEpisodes(
	context.Context,
	string,
	int64,
	downloader.Intent,
	database.DownloadIntentPlan,
	organization.SuccessInput,
	[]subscription.Episode,
) (uuid.UUID, error) {
	return uuid.Nil, errors.New("unexpected precise-search submission")
}

func (s *rssPartialFailureSubmission) SubmitSubscriptionTargetEpisodes(
	ctx context.Context, username string, subscriptionID int64, _ time.Time,
	intent downloader.Intent, plan database.DownloadIntentPlan,
	successInput organization.SuccessInput, episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.SubmitSubscriptionEpisodes(ctx, username, subscriptionID, intent, plan, successInput, episodes)
}

func (s *rssPartialFailureSubmission) SubmitRSSSubscriptionEpisodes(
	_ context.Context,
	_ string,
	subscriptionID int64,
	_ subscription.State,
	intent downloader.Intent,
	_ database.DownloadIntentPlan,
	_ organization.SuccessInput,
	_ []subscription.Episode,
) (uuid.UUID, error) {
	s.attempts = append(s.attempts, subscriptionID)
	if subscriptionID == 42 && !s.failedBob {
		s.failedBob = true
		return uuid.Nil, errors.New("transient submission failure")
	}
	s.successful[subscriptionID] = true
	return intent.ID, nil
}

func (s *rssPartialFailureSubmission) SubmitRSSTargetEpisodes(
	ctx context.Context, username string, subscriptionID int64, expected subscription.State, _ time.Time,
	intent downloader.Intent, plan database.DownloadIntentPlan,
	successInput organization.SuccessInput, episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.SubmitRSSSubscriptionEpisodes(ctx, username, subscriptionID, expected, intent, plan, successInput, episodes)
}

func (s *subscriptionFlowSubmission) SubmitRSSSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expected subscription.State,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	s.rssState = expected
	s.rssUsernames = append(s.rssUsernames, username)
	s.rssIDs = append(s.rssIDs, subscriptionID)
	s.rssStates = append(s.rssStates, expected)
	return s.SubmitSubscriptionEpisodes(ctx, username, subscriptionID, intent, plan, successInput, episodes)
}

func (s *subscriptionFlowSubmission) SubmitRSSTargetEpisodes(
	ctx context.Context, username string, subscriptionID int64, expected subscription.State, _ time.Time,
	intent downloader.Intent, plan database.DownloadIntentPlan,
	successInput organization.SuccessInput, episodes []subscription.Episode,
) (uuid.UUID, error) {
	return s.SubmitRSSSubscriptionEpisodes(ctx, username, subscriptionID, expected, intent, plan, successInput, episodes)
}

var _ RSSStore = (*rssFlowStore)(nil)
var _ indexer.RSSCursorSource = rssFlowAdapter{}
var _ RSSPollEnqueuer = (*rssFlowEnqueuer)(nil)
