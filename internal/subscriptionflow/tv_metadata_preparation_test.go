package subscriptionflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type tvMetadataNativeCase struct {
	Name  string `json:"name"`
	Input struct {
		Entrypoint        string           `json:"entrypoint"`
		State             string           `json:"state"`
		InitialRecognized bool             `json:"initial_recognized"`
		PrimarySeasons    map[string][]int `json:"primary_seasons"`
		RetryRecognized   bool             `json:"retry_recognized"`
		RetrySeasons      map[string][]int `json:"retry_seasons"`
		LibrarySeasons    map[string][]int `json:"library_seasons"`
		Manual            bool             `json:"manual_total_episode"`
		Total             int              `json:"total_episode"`
		Start             int              `json:"start_episode"`
		Note              []int            `json:"note"`
		MatchingContexts  int              `json:"matching_context_count"`
		CompletionCancel  bool             `json:"completion_cancel"`
	} `json:"input"`
	Expected struct {
		Resolution *struct {
			Satisfied bool                       `json:"satisfied"`
			NoExists  map[string]json.RawMessage `json:"no_exists"`
		} `json:"resolution"`
		Routing struct {
			Skipped            bool `json:"initial_skipped"`
			PreflightSatisfied bool `json:"preflight_satisfied"`
			Searched           bool `json:"searched"`
			RSSMatching        bool `json:"rss_matching_reached"`
		} `json:"routing"`
		RetryCalls     []json.RawMessage `json:"retry_calls"`
		LibraryCalls   int               `json:"library_calls"`
		ProgressWrites []json.RawMessage `json:"progress_writes"`
		Lack           int               `json:"lack_episode"`
		Completed      bool              `json:"completed"`
		RemainingState *string           `json:"remaining_state"`
	} `json:"expected"`
}

func tvMetadataNativeCases(t *testing.T) []tvMetadataNativeCase {
	t.Helper()
	payload, err := os.ReadFile("../../testdata/contracts/fixtures/r08/tv-metadata-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []tvMetadataNativeCase `json:"cases"`
	}
	if err := json.Unmarshal(payload, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) != 30 {
		t.Fatalf("native cases=%d, want 30", len(corpus.Cases))
	}
	return corpus.Cases
}

func nativeMetadataDetail(seasons map[string][]int) metadata.Media {
	result := metadata.Media{Source: "themoviedb", ID: "tv:123", Type: "tv", Title: "Fixture TV"}
	for season, episodes := range seasons {
		number, _ := strconv.Atoi(season)
		result.SeasonInfo = append(result.SeasonInfo, metadata.SeasonInfo{SeasonNumber: number, EpisodeCount: len(episodes)})
	}
	sort.Slice(result.SeasonInfo, func(i, j int) bool { return result.SeasonInfo[i].SeasonNumber < result.SeasonInfo[j].SeasonNumber })
	return result
}

type tvMetadataBranchReader struct {
	initial, retry       metadata.Media
	initialErr, retryErr error
	freshCalls           int
	retryCalls           int
	beforeFresh          func(int)
}

func (r *tvMetadataBranchReader) Detail(context.Context, string, string) (metadata.Media, error) {
	r.retryCalls++
	return r.retry, r.retryErr
}

func (r *tvMetadataBranchReader) DetailFresh(_ context.Context, source, id string) (metadata.Media, error) {
	if source != "themoviedb" || id != "tv:123" {
		return metadata.Media{}, errors.New("unexpected primary identity")
	}
	r.freshCalls++
	if r.beforeFresh != nil {
		r.beforeFresh(r.freshCalls)
	}
	if r.freshCalls == 1 {
		return r.initial, r.initialErr
	}
	return r.retry, r.retryErr
}

type tvMetadataProgressStore struct {
	*rssFlowStore
	progressCalls         int
	progressErr           error
	progressOwner         string
	progressID            int64
	progressState         subscription.State
	progressVersion       time.Time
	versionAfterReconcile time.Time
}

type tvMetadataRSSAdapter struct{ *v2TVTestAdapter }

func (*tvMetadataRSSAdapter) RSSCursorKey() string { return "sha256:metadata-rss" }
func (*tvMetadataRSSAdapter) FetchRSS(context.Context, indexer.RSSRequest) (indexer.RSSResponse, error) {
	return indexer.RSSResponse{}, nil
}

func (s *tvMetadataProgressStore) RecordEmptyTVSubscriptionProgress(_ context.Context, owner string, id int64, state subscription.State, version time.Time) error {
	s.progressCalls++
	s.progressOwner, s.progressID, s.progressState, s.progressVersion = owner, id, state, version
	return s.progressErr
}

func (s *tvMetadataProgressStore) ReconcileSubscriptionTarget(ctx context.Context, input database.SubscriptionTargetReconcile) (time.Time, error) {
	version, err := s.subscriptionFlowStore.ReconcileSubscriptionTarget(ctx, input)
	if err == nil && !s.versionAfterReconcile.IsZero() {
		return s.versionAfterReconcile, nil
	}
	return version, err
}

func TestOrdinaryTVMetadataEmptyBranchesMatchFrozenOuterFlow(t *testing.T) {
	t.Parallel()
	covered := 0
	for _, tc := range tvMetadataNativeCases(t) {
		// Positive missing-container selection and completion-event veto have
		// separate slices. This test executes initial failure and empty results.
		if tc.Input.CompletionCancel || (tc.Expected.Resolution != nil && len(tc.Expected.Resolution.NoExists) > 0) {
			continue
		}
		covered++
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			state := subscription.PendingSearch
			if tc.Input.State == "R" {
				state = subscription.Active
			}
			if tc.Input.State == "P" {
				state = subscription.PendingIdentification
			}
			target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", Year: "2026",
				MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Season: intPointer(1),
				InternalState: state, V2State: tc.Input.State, UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
				Config: map[string]any{"total_episode": tc.Input.Total, "start_episode": tc.Input.Start,
					"lack_episode": 3, "manual_total_episode": tc.Input.Manual, "note": tc.Input.Note}}
			store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
			store.reflectUpserts = true
			reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(tc.Input.PrimarySeasons), retry: nativeMetadataDetail(tc.Input.RetrySeasons)}
			if !tc.Input.InitialRecognized {
				reader.initialErr = errors.New("initial unavailable")
			}
			if !tc.Input.RetryRecognized {
				reader.retryErr = errors.New("retry unavailable")
			}
			library := &existingEpisodesSourceFake{}
			for season, episodes := range tc.Input.LibrarySeasons {
				number, _ := strconv.Atoi(season)
				for _, episode := range episodes {
					library.episodes = append(library.episodes, subscription.Episode{Season: number, Episode: episode})
				}
			}
			searchCalls := 0
			resources := []indexer.Resource{}
			if tc.Input.MatchingContexts > 0 {
				resources = append(resources, indexer.Resource{SiteID: 7, Title: "Fixture.TV.S01E01.2026.1080p.WEB-DL", DownloadID: "resource"})
			}
			searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
				searchCalls++
				onEvent(indexer.Event{Resources: resources})
				return resources, nil
			}}
			adapter := &tvMetadataRSSAdapter{v2TVTestAdapter: &v2TVTestAdapter{id: 7}}
			submission := &subscriptionFlowSubmission{}
			execution := &v2TVTestExecution{}
			handler, err := NewHandler(store, searcher, subscriptionFlowAdapters{adapter}, submission, execution)
			if err != nil {
				t.Fatal(err)
			}
			handler.SetMediaDetailResolver(reader)
			handler.SetExistingEpisodesSource(library)
			if tc.Input.Entrypoint == "precise" {
				err = handler.handleTarget(t.Context(), target)
			} else {
				rss, createErr := NewRSSHandler(store, handler, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
				if createErr != nil {
					t.Fatal(createErr)
				}
				matched, matchErr := rss.matchRSSTarget(t.Context(), rssSources([]indexer.Adapter{adapter}), map[int][]indexer.Resource{7: resources}, target)
				if matched {
					t.Fatal("empty missing container accepted a download")
				}
				err = matchErr
			}
			if err != nil {
				t.Fatal(err)
			}
			wantComplete := 0
			if tc.Expected.Completed {
				wantComplete = 1
			}
			wantProgress := 0
			if state == subscription.PendingIdentification && len(tc.Expected.ProgressWrites) > 0 {
				wantProgress = 1
			}
			if store.completeCalls != wantComplete || store.progressCalls != wantProgress || submission.calls+submission.stageCalls+submission.rssStageCalls != 0 || execution.calls != 0 || adapter.resolveCalls != 0 {
				t.Fatalf("complete/progress/stage/execute/resolve=%d/%d/%d/%d/%v want %d/%d/0/0/[]", store.completeCalls, store.progressCalls, submission.calls+submission.stageCalls+submission.rssStageCalls, execution.calls, adapter.resolveCalls, wantComplete, wantProgress)
			}
			if (searchCalls > 0) != tc.Expected.Routing.Searched || (store.sortCalls > 0) != (tc.Expected.Routing.Searched || tc.Expected.Routing.RSSMatching) {
				t.Fatalf("search=%d sort=%d native routing=%+v", searchCalls, store.sortCalls, tc.Expected.Routing)
			}
			if reader.freshCalls != 1 || reader.retryCalls != len(tc.Expected.RetryCalls) || library.calls != tc.Expected.LibraryCalls {
				t.Fatalf("fresh/retry/library=%d/%d/%d want 1/%d/%d", reader.freshCalls, reader.retryCalls, library.calls, len(tc.Expected.RetryCalls), tc.Expected.LibraryCalls)
			}
			if wantComplete > 0 && (store.completeOwner != target.Username || store.completeID != target.ID || store.completeState != state) {
				t.Fatal("completion lost owner/state")
			}
			if wantProgress > 0 && (store.progressOwner != target.Username || store.progressID != target.ID || store.progressState != state || !store.progressVersion.Equal(target.UpdatedAt)) {
				t.Fatal("P progress lost owner/state/version")
			}
			wantCAS := 0
			if tc.Expected.Routing.Skipped && state == subscription.PendingSearch {
				wantCAS = 1
			}
			if store.casCalls != wantCAS {
				t.Fatalf("CAS=%d want%d", store.casCalls, wantCAS)
			}
		})
	}
	if covered != 24 {
		t.Fatalf("empty branch cases=%d, want24", covered)
	}
}

func TestTVMetadataRetryRetainsInitialConfiguredTotal(t *testing.T) {
	t.Parallel()
	for _, total := range []int{2, 4} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			t.Parallel()
			target := database.Subscription{ID: 123, Username: "owner", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Season: intPointer(1), Config: map[string]any{"total_episode": total, "lack_episode": total}}
			reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(map[string][]int{}), retry: nativeMetadataDetail(map[string][]int{"1": {1, 2, 3}})}
			store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
			handler := &Handler{store: store, mediaDetails: reader}
			refreshed, resolution, state, err := handler.prepareTVSearchTarget(t.Context(), target)
			if err != nil || state.MissingContainerEmpty || state.InitialRecognitionFailed || reader.freshCalls != 1 || reader.retryCalls != 1 || resolution.Snapshot.TotalEpisode != total || refreshed.Config["total_episode"] != total || len(resolution.TargetEpisodes) != total {
				t.Fatalf("resolution=%+v state=%+v refreshed=%v reads=%d err=%v", resolution, state, refreshed.Config, reader.freshCalls, err)
			}
		})
	}
}

func TestTVMetadataEmptyBranchCancellationAndStaleProgress(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"initial", "retry", "library", "progress conflict", "progress failure"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", InternalState: subscription.PendingIdentification, UpdatedAt: time.Now().UTC(), Config: map[string]any{"total_episode": 2}}
			store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
			reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(map[string][]int{}), retry: nativeMetadataDetail(map[string][]int{})}
			library := &existingEpisodesSourceFake{}
			wantErr := context.Canceled
			switch phase {
			case "initial":
				reader.initialErr = context.Canceled
			case "retry":
				reader.retryErr = context.Canceled
			case "library":
				reader.initial = nativeMetadataDetail(map[string][]int{"2": {1}})
				library.err = context.Canceled
			case "progress conflict":
				store.progressErr = database.ErrSubscriptionPreciseSearchStateConflict
				wantErr = ErrRSSMatch
			case "progress failure":
				store.progressErr = errors.New("storage")
				wantErr = ErrRSSStorage
			}
			handler, err := NewHandler(store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7}}, &subscriptionFlowSubmission{})
			if err != nil {
				t.Fatal(err)
			}
			handler.SetMediaDetailResolver(reader)
			handler.SetExistingEpisodesSource(library)
			rss, err := NewRSSHandler(store, handler, subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7}}, &rssFlowEnqueuer{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = rss.matchRSSTarget(t.Context(), nil, nil, target)
			if !errors.Is(err, wantErr) || store.completeCalls != 0 || store.casCalls != 0 {
				t.Fatalf("err=%v want%v complete=%d cas=%d", err, wantErr, store.completeCalls, store.casCalls)
			}
		})
	}
}

func TestRSSMetadataPreparationCarriesRefreshedVersion(t *testing.T) {
	t.Parallel()
	target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Season: intPointer(1), InternalState: subscription.Active, UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), Config: map[string]any{"total_episode": 1, "lack_episode": 1}}
	nextVersion := target.UpdatedAt.Add(time.Second)
	store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target), versionAfterReconcile: nextVersion}
	store.reflectUpserts = true
	reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(map[string][]int{"1": {1, 2}})}
	handler, err := NewHandler(store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7}}, &subscriptionFlowSubmission{})
	if err != nil {
		t.Fatal(err)
	}
	handler.SetMediaDetailResolver(reader)
	handler.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}})
	rss, err := NewRSSHandler(store, handler, subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7}}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rss.matchRSSTarget(t.Context(), nil, nil, target)
	if err != nil || store.completeCalls != 1 || !store.completeTime.Equal(nextVersion) || len(store.reconciled) != 1 || !reflect.DeepEqual(store.upserted, []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}) {
		t.Fatalf("complete=%d version=%v reconciles=%v err=%v", store.completeCalls, store.completeTime, store.reconciled, err)
	}
}

func TestTVMetadataRejectsMalformedFactsWithoutCompletingFromAnotherSeason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		seasons []metadata.SeasonInfo
	}{
		{name: "oversized count", seasons: []metadata.SeasonInfo{{SeasonNumber: 1, EpisodeCount: 5001}}},
		{name: "oversized season", seasons: []metadata.SeasonInfo{{SeasonNumber: 1000, EpisodeCount: 1}}},
		{name: "duplicate season", seasons: []metadata.SeasonInfo{{SeasonNumber: 1, EpisodeCount: 0}, {SeasonNumber: 1, EpisodeCount: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Config: map[string]any{"total_episode": 2}}
			store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
			library := &existingEpisodesSourceFake{episodes: []subscription.Episode{{Season: 2, Episode: 1}}}
			reader := &tvMetadataBranchReader{initial: metadata.Media{Type: "tv", SeasonInfo: tc.seasons}}
			handler := &Handler{store: store, mediaDetails: reader, existingEpisodes: library}
			_, _, state, err := handler.prepareTVSearchTarget(t.Context(), target)
			if err != nil || !state.UnresolvedSeasonFacts || state.PreflightSatisfied || state.MissingContainerEmpty || library.calls != 0 || len(store.reconciled) != 0 {
				t.Fatalf("state=%+v library=%d err=%v", state, library.calls, err)
			}
		})
	}
}

func TestTVMetadataRetryNegativeCountRemainsUnresolved(t *testing.T) {
	t.Parallel()
	target := database.Subscription{ID: 123, Username: "owner", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Config: map[string]any{"total_episode": 2}}
	store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
	reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(map[string][]int{}), retry: metadata.Media{Type: "tv", EpisodeCount: -1}}
	handler := &Handler{store: store, mediaDetails: reader}
	_, _, state, err := handler.prepareTVSearchTarget(t.Context(), target)
	if err != nil || !state.UnresolvedSeasonFacts || state.MissingContainerEmpty || state.PreflightSatisfied || len(store.reconciled) != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestRSSPendingTVLibrarySatisfiedWritesProgressBeforePendingGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		seasons map[string][]int
		library []subscription.Episode
	}{
		{name: "metadata target season empty", seasons: map[string][]int{"2": {1}}, library: []subscription.Episode{{Season: 2, Episode: 1}}},
		{name: "known target all in library", seasons: map[string][]int{"1": {1, 2}}, library: []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Season: intPointer(1), InternalState: subscription.PendingIdentification, V2State: "P", UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), Config: map[string]any{"total_episode": 2, "lack_episode": 2}}
			store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
			store.reflectUpserts = true
			reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(tc.seasons)}
			submission := &subscriptionFlowSubmission{}
			handler, err := NewHandler(store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7}}, submission)
			if err != nil {
				t.Fatal(err)
			}
			handler.SetMediaDetailResolver(reader)
			handler.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: tc.library})
			rss, err := NewRSSHandler(store, handler, subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7}}, &rssFlowEnqueuer{})
			if err != nil {
				t.Fatal(err)
			}
			matched, err := rss.matchRSSTarget(t.Context(), nil, nil, target)
			if err != nil || matched || store.progressCalls != 1 || store.progressState != subscription.PendingIdentification || store.progressOwner != target.Username || store.progressID != target.ID || !store.progressVersion.Equal(target.UpdatedAt) || store.completeCalls != 0 || store.casCalls != 0 || store.sortCalls != 0 || submission.calls != 0 {
				t.Fatalf("matched=%v progress=%d/%s complete=%d cas=%d sort=%d err=%v", matched, store.progressCalls, store.progressState, store.completeCalls, store.casCalls, store.sortCalls, err)
			}
		})
	}
}

func TestTVMetadataEmptyMatchingNeverUsesAsyncFallbackSubmission(t *testing.T) {
	t.Parallel()
	for _, rssMode := range []bool{false, true} {
		t.Run(strconv.FormatBool(rssMode), func(t *testing.T) {
			t.Parallel()
			target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", Year: "2026", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", Season: intPointer(1), InternalState: subscription.PendingSearch, UpdatedAt: time.Now().UTC(), Config: map[string]any{"total_episode": 2}}
			if rssMode {
				target.InternalState = subscription.Active
			}
			store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
			reader := &tvMetadataBranchReader{initial: nativeMetadataDetail(map[string][]int{}), retryErr: errors.New("missing")}
			adapter := &tvMetadataRSSAdapter{v2TVTestAdapter: &v2TVTestAdapter{id: 7}}
			resources := []indexer.Resource{{SiteID: 7, Title: "Fixture.TV.S01E01.2026.1080p.WEB-DL", DownloadID: "resource"}}
			searcher := &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, emit func(indexer.Event)) ([]indexer.Resource, error) {
				emit(indexer.Event{Resources: resources})
				return resources, nil
			}}
			submission := &subscriptionFlowSubmission{}
			handler, err := NewHandler(store, searcher, subscriptionFlowAdapters{adapter}, submission)
			if err != nil {
				t.Fatal(err)
			}
			handler.SetMediaDetailResolver(reader)
			if rssMode {
				rss, createErr := NewRSSHandler(store, handler, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, err = rss.matchRSSTarget(t.Context(), rssSources([]indexer.Adapter{adapter}), map[int][]indexer.Resource{7: resources}, target)
			} else {
				err = handler.handleTarget(t.Context(), target)
			}
			if err != nil || store.completeCalls != 1 || store.sortCalls != 1 || submission.calls != 0 || submission.stageCalls != 0 || submission.rssStageCalls != 0 || adapter.resolveCalls != 0 {
				t.Fatalf("complete=%d sort=%d submit=%d stages=%d/%d resolve=%d err=%v", store.completeCalls, store.sortCalls, submission.calls, submission.stageCalls, submission.rssStageCalls, adapter.resolveCalls, err)
			}
		})
	}
}

type tvMetadataCache struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (c *tvMetadataCache) GetJSON(_ context.Context, key string, target any) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	payload, ok := c.values[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(payload, target)
}

func (c *tvMetadataCache) SetJSON(_ context.Context, key string, value any, _ time.Duration) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = payload
	return nil
}

type tvMetadataCachedProvider struct {
	calls   atomic.Int32
	current metadata.Media
}

func (*tvMetadataCachedProvider) Name() string { return "themoviedb" }
func (*tvMetadataCachedProvider) Search(context.Context, string) ([]metadata.Media, error) {
	return nil, nil
}
func (p *tvMetadataCachedProvider) Detail(context.Context, string) (metadata.Media, error) {
	p.calls.Add(1)
	return p.current, nil
}

func TestTVMetadataInitialRecognitionBypassesOldCacheButRetryUsesDefaultCache(t *testing.T) {
	t.Parallel()
	cache := &tvMetadataCache{values: make(map[string][]byte)}
	provider := &tvMetadataCachedProvider{current: nativeMetadataDetail(map[string][]int{"1": {1, 2, 3}})}
	coordinator, err := metadata.NewCoordinator([]metadata.Provider{provider}, metadata.Options{Cache: cache, RequestTimeout: time.Second, CacheTTL: time.Hour, MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = coordinator.Detail(t.Context(), "themoviedb", "tv:123"); err != nil {
		t.Fatal(err)
	}
	// The initial cache contains a positive count. The provider now has no seasons.
	provider.current = nativeMetadataDetail(map[string][]int{})
	target := database.Subscription{ID: 123, Username: "owner", Name: "Fixture TV", MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "123", InternalState: subscription.PendingSearch, Config: map[string]any{"total_episode": 2, "manual_total_episode": true}}
	store := &tvMetadataProgressStore{rssFlowStore: newRSSFlowStore(target)}
	handler := &Handler{store: store, mediaDetails: coordinator}
	_, resolution, state, err := handler.prepareTVSearchTarget(t.Context(), target)
	if err != nil || !state.MissingContainerEmpty || state.InitialRecognitionFailed || resolution.Snapshot.TotalEpisode != 2 || provider.calls.Load() != 2 || len(store.reconciled) != 0 {
		t.Fatalf("state=%+v resolution=%+v providerCalls=%d err=%v", state, resolution, provider.calls.Load(), err)
	}
	// Subsequent ordinary detail sees the fresh empty result. Retry did not force
	// another provider call through the default API cache.
	cached, err := coordinator.Detail(t.Context(), "themoviedb", "tv:123")
	if err != nil || len(cached.SeasonInfo) != 0 || provider.calls.Load() != 2 {
		t.Fatalf("cached=%+v calls=%d err=%v", cached, provider.calls.Load(), err)
	}
}
