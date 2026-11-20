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
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestHandlerExecuteV2MovieCandidatesMatchesV2FailureFlow(t *testing.T) {
	tests := []struct {
		name          string
		configure     func(*v2MovieExecutionFixture)
		wantResult    v2MovieExecutionResult
		wantErr       error
		wantResolves  []string
		wantExecutes  int
		wantStages    []string
		wantTTLs      []time.Duration
		wantListCalls int
	}{
		{
			name: "active cooldown skips first candidate",
			configure: func(f *v2MovieExecutionFixture) {
				f.failureState.active[f.fingerprint(0)] = struct{}{}
			},
			wantResult: v2MovieExecutionResult{Accepted: true}, wantResolves: []string{"102"},
			wantExecutes: 1, wantStages: []string{"102"}, wantListCalls: 1,
		},
		{
			name: "all cooled candidates do not load downloader settings",
			configure: func(f *v2MovieExecutionFixture) {
				f.failureState.active[f.fingerprint(0)] = struct{}{}
				f.failureState.active[f.fingerprint(1)] = struct{}{}
				f.store.TargetStore.(*subscriptionFlowStore).downloadErr = errors.New("settings unavailable")
			},
			wantListCalls: 1,
		},
		{
			name: "cooldown lookup failure fails open",
			configure: func(f *v2MovieExecutionFixture) {
				f.failureState.listErr = errors.New("postgres unavailable")
			},
			wantResult: v2MovieExecutionResult{Accepted: true}, wantResolves: []string{"101"},
			wantExecutes: 1, wantStages: []string{"101"}, wantListCalls: 1,
		},
		{
			name: "empty torrent records resource failure and continues",
			configure: func(f *v2MovieExecutionFixture) {
				f.adapter.bodies["101"] = nil
			},
			wantResult: v2MovieExecutionResult{Accepted: true}, wantResolves: []string{"101", "102"},
			wantExecutes: 1, wantStages: []string{"102"},
			wantTTLs: []time.Duration{subscription.V2DownloadFailureResourceTTL}, wantListCalls: 1,
		},
		{
			name: "invalid torrent records resource failure and continues",
			configure: func(f *v2MovieExecutionFixture) {
				f.adapter.bodies["101"] = []byte("not bencode metainfo")
			},
			wantResult: v2MovieExecutionResult{Accepted: true}, wantResolves: []string{"101", "102"},
			wantExecutes: 1, wantStages: []string{"102"},
			wantTTLs: []time.Duration{subscription.V2DownloadFailureResourceTTL}, wantListCalls: 1,
		},
		{
			name: "failure persistence error still continues",
			configure: func(f *v2MovieExecutionFixture) {
				f.adapter.errs["101"] = errors.New("torrent request failed")
				f.failureState.recordErr = errors.New("postgres unavailable")
			},
			wantResult: v2MovieExecutionResult{Accepted: true}, wantResolves: []string{"101", "102"},
			wantExecutes: 1, wantStages: []string{"102"},
			wantTTLs: []time.Duration{subscription.V2DownloadFailureResourceTTL}, wantListCalls: 1,
		},
		{
			name: "definitive downloader failure records transient failure and continues",
			configure: func(f *v2MovieExecutionFixture) {
				f.execution.results = []downloadflow.IntentExecutionResult{
					{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered},
					{Outcome: downloadflow.IntentExecutionAccepted},
				}
			},
			wantResult: v2MovieExecutionResult{Accepted: true}, wantResolves: []string{"101", "102"},
			wantExecutes: 2, wantStages: []string{"101", "102"},
			wantTTLs: []time.Duration{subscription.V2DownloadFailureTransientTTL}, wantListCalls: 1,
		},
		{
			name: "unknown downloader result stops without cooldown",
			configure: func(f *v2MovieExecutionFixture) {
				f.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionPendingReconciliation}}
			},
			wantResult: v2MovieExecutionResult{Stopped: true}, wantResolves: []string{"101"},
			wantExecutes: 1, wantStages: []string{"101"}, wantListCalls: 1,
		},
		{
			name: "permanent candidate fact continues without cooldown",
			configure: func(f *v2MovieExecutionFixture) {
				f.execution.results = []downloadflow.IntentExecutionResult{
					{Outcome: downloadflow.IntentExecutionPermanentCandidateFailure},
					{Outcome: downloadflow.IntentExecutionAccepted},
				}
			},
			wantResult: v2MovieExecutionResult{Accepted: true}, wantResolves: []string{"101", "102"},
			wantExecutes: 2, wantStages: []string{"101", "102"}, wantListCalls: 1,
		},
		{
			name: "retryable dependency failure stops without cooldown",
			configure: func(f *v2MovieExecutionFixture) {
				f.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionRetryableFailure}}
			},
			wantErr: ErrSubscriptionSubmission, wantResolves: []string{"101"},
			wantExecutes: 1, wantStages: []string{"101"}, wantListCalls: 1,
		},
		{
			name: "wrapped context error stops without cooldown",
			configure: func(f *v2MovieExecutionFixture) {
				f.adapter.errs["101"] = fmt.Errorf("resolve stopped: %w", context.DeadlineExceeded)
			},
			wantErr: context.DeadlineExceeded, wantResolves: []string{"101"}, wantListCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
			if tt.configure != nil {
				tt.configure(fixture)
			}
			got, err := fixture.handler.executeV2MovieCandidates(
				t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, false,
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("executeV2MovieCandidates() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.wantResult {
				t.Fatalf("executeV2MovieCandidates() = %#v, want %#v", got, tt.wantResult)
			}
			if !reflect.DeepEqual(fixture.adapter.calls, tt.wantResolves) {
				t.Fatalf("resolver calls = %v, want %v", fixture.adapter.calls, tt.wantResolves)
			}
			if len(fixture.execution.calls) != tt.wantExecutes {
				t.Fatalf("execution calls = %d, want %d", len(fixture.execution.calls), tt.wantExecutes)
			}
			if !reflect.DeepEqual(fixture.submission.stageResourceIDs, tt.wantStages) {
				t.Fatalf("staged resources = %v, want %v", fixture.submission.stageResourceIDs, tt.wantStages)
			}
			if !reflect.DeepEqual(fixture.failureState.ttls, tt.wantTTLs) {
				t.Fatalf("failure TTLs = %v, want %v", fixture.failureState.ttls, tt.wantTTLs)
			}
			if fixture.failureState.listCalls != tt.wantListCalls {
				t.Fatalf("failure list calls = %d, want %d", fixture.failureState.listCalls, tt.wantListCalls)
			}
		})
	}
}

func TestHandlerExecuteV2MovieCandidatesSkipsDuplicateFingerprintInSameRun(t *testing.T) {
	fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
	duplicate := fixture.candidates[0]
	duplicate.Resource.DownloadID = "111"
	fixture.adapter.bodies["101"] = nil
	fixture.adapter.bodies["111"] = []byte("must not be resolved")
	fixture.candidates = []subscription.Candidate{fixture.candidates[0], duplicate, fixture.candidates[1]}

	got, err := fixture.handler.executeV2MovieCandidates(
		t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil || !got.Accepted {
		t.Fatalf("executeV2MovieCandidates() = (%#v, %v)", got, err)
	}
	if !reflect.DeepEqual(fixture.adapter.calls, []string{"101", "102"}) {
		t.Fatalf("resolver calls = %v, duplicate fingerprint was retried", fixture.adapter.calls)
	}
}

func TestHandlerExecuteV2MovieCandidatesUsesSameFlowForBestVersion(t *testing.T) {
	for _, bestVersion := range []bool{false, true} {
		t.Run(fmt.Sprintf("best_version=%t", bestVersion), func(t *testing.T) {
			fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
			fixture.target.Config["best_version"] = bestVersion
			fixture.failureState.active[fixture.fingerprint(0)] = struct{}{}
			got, err := fixture.handler.executeV2MovieCandidates(
				t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, false,
			)
			if err != nil || !got.Accepted || !reflect.DeepEqual(fixture.adapter.calls, []string{"102"}) {
				t.Fatalf("best-version flow = result:%#v error:%v calls:%v", got, err, fixture.adapter.calls)
			}
		})
	}
}

func TestHandlerExecuteV2MovieCandidatesPreservesRSSStateSemantics(t *testing.T) {
	tests := []struct {
		name              string
		state             subscription.State
		wantPreciseStages int
		wantRSSStages     []subscription.State
	}{
		{name: "R retries through precise stage", state: subscription.Active, wantPreciseStages: 1, wantRSSStages: []subscription.State{subscription.Active}},
		{name: "P remains pending on every retry", state: subscription.PendingIdentification, wantRSSStages: []subscription.State{subscription.PendingIdentification, subscription.PendingIdentification}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newV2MovieExecutionFixture(t, tt.state)
			fixture.execution.results = []downloadflow.IntentExecutionResult{
				{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered},
				{Outcome: downloadflow.IntentExecutionAccepted},
			}
			got, err := fixture.handler.executeV2MovieCandidates(
				t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, true,
			)
			if err != nil || !got.Accepted {
				t.Fatalf("RSS movie execution = (%#v, %v)", got, err)
			}
			if fixture.submission.stageCalls != tt.wantPreciseStages ||
				!reflect.DeepEqual(fixture.submission.rssStates, tt.wantRSSStages) {
				t.Fatalf("RSS stages = precise:%d rss:%v", fixture.submission.stageCalls, fixture.submission.rssStates)
			}
		})
	}
}

func TestHandlerExecuteV2MovieCandidatesDoesNotPersistPrivateLocator(t *testing.T) {
	fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
	privateLocator := "https://tracker.example/download.php?id=101&passkey=private-secret"
	fixture.candidates[0].Resource.DownloadID = privateLocator
	fixture.adapter.bodies[privateLocator] = v2TVTestMetainfo(t, "Example.Movie.2026.mkv")
	fixture.execution.results = []downloadflow.IntentExecutionResult{
		{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered},
		{Outcome: downloadflow.IntentExecutionAccepted},
	}

	got, err := fixture.handler.executeV2MovieCandidates(
		t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, false,
	)
	if err != nil || !got.Accepted || len(fixture.failureState.records) != 1 {
		t.Fatalf("private locator flow = result:%#v error:%v failures:%#v", got, err, fixture.failureState.records)
	}
	failure := fixture.failureState.records[0]
	if failure.ResourceKey != "101" || strings.Contains(fmt.Sprintf("%#v", failure), "private-secret") {
		t.Fatalf("persisted failure leaked locator: %#v", failure)
	}
}

func TestHandlerPreciseSearchUsesV2MovieCooldownFallback(t *testing.T) {
	fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
	baseStore := fixture.store.TargetStore.(*subscriptionFlowStore)
	baseStore.list = func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
		return []database.Subscription{fixture.target}, nil
	}
	fixture.failureState.active[fixture.fingerprint(0)] = struct{}{}
	fixture.handler.searcher = &subscriptionFlowSearcher{search: func(
		_ context.Context,
		_ []indexer.Adapter,
		_ indexer.Query,
		onEvent func(indexer.Event),
	) ([]indexer.Resource, error) {
		first := fixture.candidates[0].Resource
		first.Seeders = 20
		second := fixture.candidates[1].Resource
		second.Title = "Example.Movie.2026.1080p.WEB-DL"
		second.Seeders = 10
		resources := []indexer.Resource{first, second}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}

	if err := fixture.handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.adapter.calls, []string{"102"}) ||
		!reflect.DeepEqual(fixture.submission.stageResourceIDs, []string{"102"}) ||
		len(fixture.execution.calls) != 1 {
		t.Fatalf(
			"precise movie fallback = resolver:%v stages:%v executions:%d",
			fixture.adapter.calls,
			fixture.submission.stageResourceIDs,
			len(fixture.execution.calls),
		)
	}
}

func TestRSSHandlerMatchUsesV2MovieCooldownFallback(t *testing.T) {
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	first := subscription.Candidate{Resource: indexer.Resource{
		SiteID: 7, Site: "rss", Title: "Example.Movie.2026.1080p.WEB-DL",
		TorrentID: "101", DownloadID: "101", Seeders: 20,
	}}
	second := subscription.Candidate{Resource: indexer.Resource{
		SiteID: 7, Site: "rss", Title: "Example.Movie.2026.1080p.WEB-DL",
		TorrentID: "102", DownloadID: "102", Seeders: 10,
	}}
	failure, _ := subscription.NewV2DownloadFailure(
		v2DownloadFailureTarget(target), first, nil, "", v2SubscriptionSource(target), "",
	)
	failureState := &v2MovieFailureState{active: map[string]struct{}{failure.Fingerprint: {}}}
	store := &v2MovieRSSStore{rssFlowStore: newRSSFlowStore(target), state: failureState}
	store.activeTarget = &target
	adapter := &v2MovieRSSAdapter{v2MovieTestAdapter: &v2MovieTestAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
		bodies: map[string][]byte{
			"101": v2TVTestMetainfo(t, "Example.Movie.2026.mkv"),
			"102": v2TVTestMetainfo(t, "Example.Movie.2026.Remux.mkv"),
		},
		errs: make(map[string]error),
	}}
	submission := &subscriptionFlowSubmission{}
	execution := &v2MovieTestExecution{}
	adapters := subscriptionFlowAdapters{adapter}
	base, err := NewHandler(store, &subscriptionFlowSearcher{}, adapters, submission, execution)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewRSSHandler(store, base, adapters, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	matched, err := handler.matchRSSTarget(
		t.Context(), rssSources(adapters), map[int][]indexer.Resource{7: {first.Resource, second.Resource}}, target,
	)
	if err != nil || !matched {
		t.Fatalf("matchRSSTarget() = (%t, %v)", matched, err)
	}
	if !reflect.DeepEqual(adapter.calls, []string{"102"}) ||
		!reflect.DeepEqual(submission.stageResourceIDs, []string{"102"}) ||
		!reflect.DeepEqual(submission.rssStates, []subscription.State{subscription.Active}) {
		t.Fatalf(
			"RSS movie fallback = resolver:%v stages:%v states:%v",
			adapter.calls,
			submission.stageResourceIDs,
			submission.rssStates,
		)
	}
}

type v2MovieExecutionFixture struct {
	target       database.Subscription
	candidates   []subscription.Candidate
	failureState *v2MovieFailureState
	store        *v2MovieTestStore
	adapter      *v2MovieTestAdapter
	submission   *subscriptionFlowSubmission
	execution    *v2MovieTestExecution
	handler      *Handler
}

func newV2MovieExecutionFixture(t *testing.T, state subscription.State) *v2MovieExecutionFixture {
	t.Helper()
	target := subscriptionFlowTarget()
	target.InternalState = state
	target.MediaSource = "themoviedb"
	target.MediaID = "42"
	candidates := []subscription.Candidate{
		{Resource: indexer.Resource{SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL", TorrentID: "101", DownloadID: "101"}},
		{Resource: indexer.Resource{SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.720p.WEB-DL", TorrentID: "102", DownloadID: "102"}},
	}
	failureState := &v2MovieFailureState{active: make(map[string]struct{})}
	store := &v2MovieTestStore{
		TargetStore: &subscriptionFlowStore{downloaders: []downloadconfig.Downloader{{
			ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
		}}},
		state: failureState,
	}
	adapter := &v2MovieTestAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1},
		bodies: map[string][]byte{
			"101": v2TVTestMetainfo(t, "Example.Movie.2026.mkv"),
			"102": v2TVTestMetainfo(t, "Example.Movie.2026.Remux.mkv"),
		},
		errs: make(map[string]error),
	}
	submission := &subscriptionFlowSubmission{}
	execution := &v2MovieTestExecution{}
	handler, err := NewHandler(store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission, execution)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &v2MovieExecutionFixture{
		target: target, candidates: candidates, failureState: failureState, store: store,
		adapter: adapter, submission: submission, execution: execution, handler: handler,
	}
	baseStore := store.TargetStore.(*subscriptionFlowStore)
	baseStore.activeTarget = &fixture.target
	execution.onResult = func(result downloadflow.IntentExecutionResult) {
		if result.Outcome != downloadflow.IntentExecutionDefinitiveFailureRecovered &&
			result.Outcome != downloadflow.IntentExecutionPermanentCandidateFailure {
			return
		}
		if fixture.target.InternalState != subscription.PendingIdentification {
			fixture.target.InternalState = subscription.PendingSearch
		}
		fixture.target.V2State, _ = subscription.V2State(fixture.target.InternalState)
		fixture.target.UpdatedAt = fixture.target.UpdatedAt.Add(time.Second)
	}
	return fixture
}

func (f *v2MovieExecutionFixture) fingerprint(index int) string {
	failure, _ := subscription.NewV2DownloadFailure(
		v2DownloadFailureTarget(f.target), f.candidates[index], nil, "", v2SubscriptionSource(f.target), "",
	)
	return failure.Fingerprint
}

type v2MovieFailureState struct {
	active    map[string]struct{}
	listErr   error
	recordErr error
	listCalls int
	records   []subscription.V2DownloadFailure
	ttls      []time.Duration
}

type v2MovieTestStore struct {
	TargetStore
	state *v2MovieFailureState
}

type v2MovieRSSStore struct {
	*rssFlowStore
	state *v2MovieFailureState
}

func (s *v2MovieTestStore) ListActiveV2DownloadFailureFingerprints(
	_ context.Context,
	_ []string,
	_ time.Time,
) (map[string]struct{}, error) {
	s.state.listCalls++
	return s.state.active, s.state.listErr
}

func (s *v2MovieTestStore) RecordV2DownloadFailure(
	_ context.Context,
	failure subscription.V2DownloadFailure,
	_ time.Time,
	ttl time.Duration,
) error {
	s.state.records = append(s.state.records, failure)
	s.state.ttls = append(s.state.ttls, ttl)
	return s.state.recordErr
}

func (s *v2MovieRSSStore) ListActiveV2DownloadFailureFingerprints(
	_ context.Context,
	_ []string,
	_ time.Time,
) (map[string]struct{}, error) {
	s.state.listCalls++
	return s.state.active, s.state.listErr
}

func (s *v2MovieRSSStore) RecordV2DownloadFailure(
	_ context.Context,
	failure subscription.V2DownloadFailure,
	_ time.Time,
	ttl time.Duration,
) error {
	s.state.records = append(s.state.records, failure)
	s.state.ttls = append(s.state.ttls, ttl)
	return s.state.recordErr
}

type v2MovieTestAdapter struct {
	subscriptionFlowAdapter
	bodies    map[string][]byte
	errs      map[string]error
	calls     []string
	onResolve func(string)
}

type v2MovieRSSAdapter struct {
	*v2MovieTestAdapter
}

func (a *v2MovieRSSAdapter) RSSCursorKey() string { return "sha256:v2-movie-rss" }

func (a *v2MovieRSSAdapter) FetchRSS(
	context.Context,
	indexer.RSSRequest,
) (indexer.RSSResponse, error) {
	return indexer.RSSResponse{}, nil
}

func (a *v2MovieTestAdapter) ResolveDownload(
	_ context.Context,
	resourceID string,
) (indexer.ResolvedDownload, error) {
	a.calls = append(a.calls, resourceID)
	if a.onResolve != nil {
		a.onResolve(resourceID)
	}
	if err := a.errs[resourceID]; err != nil {
		return indexer.ResolvedDownload{}, err
	}
	return indexer.ResolvedDownload{Body: append([]byte(nil), a.bodies[resourceID]...)}, nil
}

type v2MovieTestExecution struct {
	results  []downloadflow.IntentExecutionResult
	errs     []error
	calls    []uuid.UUID
	bodies   [][]byte
	onResult func(downloadflow.IntentExecutionResult)
}

func (e *v2MovieTestExecution) ExecuteIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
) (downloadflow.IntentExecutionResult, error) {
	return e.execute(ctx, username, intentID, nil)
}

func (e *v2MovieTestExecution) ExecuteIntentWithMetainfo(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	body []byte,
) (downloadflow.IntentExecutionResult, error) {
	return e.execute(ctx, username, intentID, body)
}

func (e *v2MovieTestExecution) execute(
	_ context.Context,
	_ string,
	intentID uuid.UUID,
	body []byte,
) (downloadflow.IntentExecutionResult, error) {
	e.calls = append(e.calls, intentID)
	e.bodies = append(e.bodies, append([]byte(nil), body...))
	result := downloadflow.IntentExecutionResult{Outcome: downloadflow.IntentExecutionAccepted}
	if len(e.results) > 0 {
		result = e.results[0]
		e.results = e.results[1:]
	}
	var err error
	if len(e.errs) > 0 {
		err = e.errs[0]
		e.errs = e.errs[1:]
	}
	if e.onResult != nil {
		e.onResult(result)
	}
	return result, err
}

var _ indexer.VersionedDownloadResolver = (*v2MovieTestAdapter)(nil)
var _ DownloadIntentExecution = (*v2MovieTestExecution)(nil)
