package subscriptionflow

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestHandlerExcludeExistingEpisodes(t *testing.T) {
	t.Parallel()
	target := database.Subscription{
		Name: " Example Show ", Year: " 2026 ", MediaSource: " TMDB ", MediaID: " 123 ",
	}
	pending := []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 2, Episode: 1},
	}
	const sensitive = "emby-sensitive-marker"
	tests := []struct {
		name      string
		source    *existingEpisodesSourceFake
		cancel    bool
		want      []subscription.Episode
		wantErr   error
		sensitive string
		wantCalls int
	}{
		{name: "source is optional", want: pending},
		{
			name: "existing episodes are subtracted by season and episode",
			source: &existingEpisodesSourceFake{episodes: []subscription.Episode{
				{Season: 1, Episode: 2}, {Season: 1, Episode: 2}, {Season: 3, Episode: 1},
			}},
			want:      []subscription.Episode{{Season: 1, Episode: 1}, {Season: 2, Episode: 1}},
			wantCalls: 1,
		},
		{
			name:   "invalid upstream episode fails open",
			source: &existingEpisodesSourceFake{episodes: []subscription.Episode{{Season: -1, Episode: 1}}},
			want:   pending, wantCalls: 1,
		},
		{
			name:   "upstream failure fails open",
			source: &existingEpisodesSourceFake{err: errors.New(sensitive)},
			want:   pending, sensitive: sensitive, wantCalls: 1,
		},
		{
			name:   "context cancellation is preserved",
			source: &existingEpisodesSourceFake{err: errors.New("ignored")},
			cancel: true, wantErr: context.Canceled, wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tt.cancel {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			handler := &Handler{}
			if tt.source != nil {
				handler.SetExistingEpisodesSource(tt.source)
			}
			got, err := handler.excludeExistingEpisodes(ctx, ExistingEpisodesQuery{
				Title:       target.Name,
				Year:        target.Year,
				MediaSource: target.MediaSource,
				MediaID:     target.MediaID,
			}, target.Season, target.Config, pending)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("excludeExistingEpisodes() error = %v, want %v", err, tt.wantErr)
			}
			if tt.sensitive != "" && err != nil && strings.Contains(err.Error(), tt.sensitive) {
				t.Fatalf("excludeExistingEpisodes() leaked upstream error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("excludeExistingEpisodes() = %#v, want %#v", got, tt.want)
			}
			if tt.source != nil {
				if tt.source.calls != tt.wantCalls {
					t.Fatalf("source calls = %d, want %d", tt.source.calls, tt.wantCalls)
				}
				wantQuery := ExistingEpisodesQuery{
					Title: "Example Show", Year: "2026", MediaSource: "TMDB", MediaID: "123",
				}
				if tt.wantCalls > 0 && !reflect.DeepEqual(tt.source.query, wantQuery) {
					t.Fatalf("source query = %#v, want %#v", tt.source.query, wantQuery)
				}
			}
		})
	}
}

func TestHandlerExcludeExistingEpisodesAppliesNoteAndSkipsBestVersion(t *testing.T) {
	t.Parallel()
	pending := []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3},
	}
	season := 1
	t.Run("ordinary subscription subtracts library and note", func(t *testing.T) {
		t.Parallel()
		source := &existingEpisodesSourceFake{episodes: []subscription.Episode{{Season: 1, Episode: 1}}}
		handler := &Handler{existingEpisodes: source}
		got, err := handler.excludeExistingEpisodes(t.Context(), ExistingEpisodesQuery{Title: "Show"},
			&season, map[string]any{"note": []any{"2"}}, pending)
		if err != nil || !reflect.DeepEqual(got, []subscription.Episode{{Season: 1, Episode: 3}}) {
			t.Fatalf("excludeExistingEpisodes() = (%#v, %v)", got, err)
		}
	})
	t.Run("best version bypasses ordinary library difference", func(t *testing.T) {
		t.Parallel()
		source := &existingEpisodesSourceFake{episodes: pending}
		handler := &Handler{existingEpisodes: source}
		got, err := handler.excludeExistingEpisodes(t.Context(), ExistingEpisodesQuery{Title: "Show"},
			&season, map[string]any{"best_version": float64(1), "note": []int{2}}, pending)
		if err != nil || !reflect.DeepEqual(got, pending) || source.calls != 0 {
			t.Fatalf("excludeExistingEpisodes() = (%#v, %v), calls=%d", got, err, source.calls)
		}
	})
}

func TestHandlerExistingMovieSatisfiedUsesLibraryAndDownloadNote(t *testing.T) {
	t.Parallel()
	const sensitive = "emby-movie-sensitive-marker"
	tests := []struct {
		name      string
		source    *existingMovieSourceFake
		config    map[string]any
		cancel    bool
		want      bool
		wantErr   error
		wantCalls int
	}{
		{name: "source is optional", config: map[string]any{}},
		{name: "library hit satisfies movie", source: &existingMovieSourceFake{found: true}, config: map[string]any{}, want: true, wantCalls: 1},
		{name: "download note satisfies movie", source: &existingMovieSourceFake{}, config: map[string]any{"note": []any{float64(1)}}, want: true, wantCalls: 1},
		{name: "library failure still uses note", source: &existingMovieSourceFake{err: errors.New(sensitive)}, config: map[string]any{"note": []int{1}}, want: true, wantCalls: 1},
		{name: "library failure fails open", source: &existingMovieSourceFake{err: errors.New(sensitive)}, config: map[string]any{}, wantCalls: 1},
		{name: "best version bypasses library and note", source: &existingMovieSourceFake{found: true}, config: map[string]any{"best_version": true, "note": []int{1}}},
		{name: "best version 100 is already satisfied", source: &existingMovieSourceFake{}, config: map[string]any{"best_version": true, "current_priority": 100}, want: true},
		{name: "best version 90 is not satisfied", source: &existingMovieSourceFake{found: true}, config: map[string]any{"best_version": true, "current_priority": 90}},
		{name: "best version 101 is not terminal", config: map[string]any{"best_version": true, "current_priority": 101}},
		{name: "context cancellation is preserved", source: &existingMovieSourceFake{err: errors.New("ignored")}, config: map[string]any{}, cancel: true, wantErr: context.Canceled, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tt.cancel {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			handler := &Handler{}
			if tt.source != nil {
				handler.SetExistingMovieSource(tt.source)
			}
			got, err := handler.existingMovieSatisfied(
				ctx, " Example Movie ", " 2026 ", " TMDB ", " 123 ", tt.config,
			)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("existingMovieSatisfied() = (%v, %v), want (%v, %v)", got, err, tt.want, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitive) {
				t.Fatalf("existingMovieSatisfied() leaked upstream error: %v", err)
			}
			if tt.source != nil {
				if tt.source.calls != tt.wantCalls {
					t.Fatalf("source calls = %d, want %d", tt.source.calls, tt.wantCalls)
				}
				wantQuery := ExistingMovieQuery{
					Title: "Example Movie", Year: "2026", MediaSource: "TMDB", MediaID: "123",
				}
				if tt.wantCalls > 0 && !reflect.DeepEqual(tt.source.query, wantQuery) {
					t.Fatalf("source query = %#v, want %#v", tt.source.query, wantQuery)
				}
			}
		})
	}
}

func TestWithStoredMissingEpisodesKeepsDurableStateAndFiltersQuery(t *testing.T) {
	t.Parallel()
	store := &subscriptionFlowStore{episodes: []subscription.SubscriptionEpisode{
		{Season: 1, Episode: 1, State: subscription.SubscriptionEpisodeMissing},
		{Season: 1, Episode: 2, State: subscription.SubscriptionEpisodeMissing},
	}}
	source := &existingEpisodesSourceFake{episodes: []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
	}}
	handler := &Handler{store: store}
	handler.SetExistingEpisodesSource(source)
	target := database.Subscription{
		ID: 7, Username: "alice", Name: "Show", Year: "2026", MediaType: string(media.TypeTV),
		MediaSource: "themoviedb", MediaID: "123", Config: map[string]any{},
	}

	resolution, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	query, known, matchable, err := handler.withCurrentTVMissingEpisodes(
		t.Context(), target, resolution, true, subscription.Query{MediaType: media.TypeTV},
	)
	if err != nil {
		t.Fatal(err)
	}
	if known || !matchable || len(query.MissingEpisodes) != 0 {
		t.Fatalf("filtered query = %#v, known=%v matchable=%v", query.MissingEpisodes, known, matchable)
	}
	if len(store.episodes) != 2 || len(store.upserted) != 0 {
		t.Fatalf("media lookup mutated durable episode facts: episodes=%#v upserted=%#v", store.episodes, store.upserted)
	}
}

func TestHandleSubscriptionSearchFailsOpenOnMediaServerFailure(t *testing.T) {
	t.Parallel()
	const sensitive = "emby-token-sensitive-marker"
	cutoff := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "123"
	target.Season = intPointer(1)
	target.Config["total_episode"] = 2
	target.UpdatedAt = cutoff.Add(-time.Minute)
	store := &subscriptionFlowStore{
		reflectUpserts: true,
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
	}
	searchCalls := 0
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		&subscriptionFlowSearcher{search: func(
			_ context.Context,
			_ []indexer.Adapter,
			_ indexer.Query,
			onEvent func(indexer.Event),
		) ([]indexer.Resource, error) {
			searchCalls++
			onEvent(indexer.Event{})
			return nil, nil
		}},
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		&subscriptionFlowSubmission{},
	)
	setSubscriptionFlowTVMetadata(handler, target)
	handler.SetExistingEpisodesSource(&existingEpisodesSourceFake{err: errors.New(sensitive)})

	err := handler.HandleSubscriptionSearch(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("HandleSubscriptionSearch() error = %v", err)
	}
	if searchCalls != 1 || store.casCalls != 1 || store.casNext != subscription.Active {
		t.Fatalf("failed media lookup did not continue V2 flow: search=%d cas=%d next=%s", searchCalls, store.casCalls, store.casNext)
	}
}

func TestHandleSubscriptionSearchSkipsIndexerWhenLibrarySatisfiesTarget(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 15, 13, 0, 0, 0, time.UTC)
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeTV)
	target.MediaSource = "themoviedb"
	target.MediaID = "123"
	target.Season = intPointer(1)
	target.Config["total_episode"] = 2
	target.UpdatedAt = cutoff.Add(-time.Minute)
	store := &subscriptionFlowStore{
		reflectUpserts: true,
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
	}
	searchCalls := 0
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		&subscriptionFlowSearcher{search: func(
			context.Context, []indexer.Adapter, indexer.Query, func(indexer.Event),
		) ([]indexer.Resource, error) {
			searchCalls++
			return nil, nil
		}},
		subscriptionFlowAdapters{},
		&subscriptionFlowSubmission{},
	)
	setSubscriptionFlowTVMetadata(handler, target)
	handler.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
	}})

	if err := handler.HandleSubscriptionSearch(t.Context(), cutoff); err != nil {
		t.Fatal(err)
	}
	if searchCalls != 0 || store.casCalls != 0 || store.completeCalls != 1 ||
		store.completeOwner != target.Username || store.completeID != target.ID ||
		store.completeState != subscription.PendingSearch || !store.completeTime.Equal(target.UpdatedAt) {
		t.Fatalf(
			"library-complete target = search:%d cas:%d complete:%d owner:%q id:%d state:%s time:%s",
			searchCalls, store.casCalls, store.completeCalls, store.completeOwner, store.completeID,
			store.completeState, store.completeTime,
		)
	}
}

func TestHandleSubscriptionSearchCompletesExistingMovieWithoutIndexer(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC)
	target := subscriptionFlowTarget()
	target.MediaType = string(media.TypeMovie)
	target.MediaSource = "themoviedb"
	target.MediaID = "456"
	target.UpdatedAt = cutoff.Add(-time.Minute)
	store := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
	}
	searchCalls := 0
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		&subscriptionFlowSearcher{search: func(
			context.Context, []indexer.Adapter, indexer.Query, func(indexer.Event),
		) ([]indexer.Resource, error) {
			searchCalls++
			return nil, nil
		}},
		subscriptionFlowAdapters{},
		&subscriptionFlowSubmission{},
	)
	source := &existingMovieSourceFake{found: true}
	handler.SetExistingMovieSource(source)

	if err := handler.HandleSubscriptionSearch(t.Context(), cutoff); err != nil {
		t.Fatal(err)
	}
	if searchCalls != 0 || source.calls != 1 || store.completeCalls != 1 ||
		store.completeOwner != target.Username || store.completeID != target.ID ||
		store.completeState != subscription.PendingSearch || store.completeType != media.TypeMovie ||
		!store.completeTime.Equal(target.UpdatedAt) {
		t.Fatalf(
			"movie-complete target = search:%d source:%d complete:%d owner:%q id:%d state:%s type:%s time:%s",
			searchCalls, source.calls, store.completeCalls, store.completeOwner, store.completeID,
			store.completeState, store.completeType, store.completeTime,
		)
	}
}

func TestHandlerFinishSatisfiedTargetHandlesConflictAndSanitizesFailure(t *testing.T) {
	t.Parallel()
	const sensitive = "satisfied-store-sensitive-marker"
	target := database.Subscription{ID: 91, Username: "alice"}
	tests := []struct {
		name     string
		storeErr error
		wantErr  error
	}{
		{name: "completion succeeds"},
		{
			name:     "concurrent state change requests retry",
			storeErr: database.ErrSubscriptionPreciseSearchStateConflict,
			wantErr:  ErrSubscriptionTargetChanged,
		},
		{
			name:     "storage failure is sanitized",
			storeErr: errors.New(sensitive), wantErr: ErrSubscriptionSearchStorage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &subscriptionFlowStore{completeErr: tt.storeErr}
			handler := &Handler{store: store}
			err := handler.finishSatisfiedTarget(t.Context(), target, subscription.PendingSearch)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("finishSatisfiedTarget() error = %v, want %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), sensitive) {
				t.Fatalf("finishSatisfiedTarget() leaked storage error: %v", err)
			}
			if store.completeCalls != 1 || store.completeOwner != target.Username || store.completeID != target.ID ||
				store.completeState != subscription.PendingSearch || !store.completeTime.Equal(target.UpdatedAt) {
				t.Fatalf(
					"completion call = calls:%d owner:%q id:%d",
					store.completeCalls, store.completeOwner, store.completeID,
				)
			}
		})
	}
}

type existingEpisodesSourceFake struct {
	episodes []subscription.Episode
	err      error
	calls    int
	query    ExistingEpisodesQuery
}

type existingMovieSourceFake struct {
	found bool
	err   error
	calls int
	query ExistingMovieQuery
}

func (s *existingMovieSourceFake) FindExistingMovie(
	_ context.Context,
	query ExistingMovieQuery,
) (bool, error) {
	s.calls++
	s.query = query
	return s.found, s.err
}

func (s *existingEpisodesSourceFake) FindExistingEpisodes(
	_ context.Context,
	query ExistingEpisodesQuery,
) ([]subscription.Episode, error) {
	s.calls++
	s.query = query
	return append([]subscription.Episode(nil), s.episodes...), s.err
}

func intPointer(value int) *int { return &value }
