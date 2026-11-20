package subscriptionflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestPreciseHandlerMovieUpgradeGate(t *testing.T) {
	fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
	fixture.target.Config["best_version"] = true
	fixture.target.Config["current_priority"] = 90
	baseStore := fixture.store.TargetStore.(*subscriptionFlowStore)
	baseStore.list = func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
		return []database.Subscription{fixture.target}, nil
	}
	fixture.handler.searcher = &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, onEvent func(indexer.Event)) ([]indexer.Resource, error) {
		first, second := fixture.candidates[0].Resource, fixture.candidates[1].Resource
		first.PriOrder, first.Seeders = 90, 100
		second.PriOrder, second.Seeders = 100, 1
		second.Title = "Example.Movie.2026.1080p.WEB-DL"
		resources := []indexer.Resource{first, second}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	if err := fixture.handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.adapter.calls, []string{"102"}) || fixture.submission.plan.MoviePriority == nil || *fixture.submission.plan.MoviePriority != 100 {
		t.Fatalf("precise gate calls=%v plan=%+v", fixture.adapter.calls, fixture.submission.plan)
	}
}

func TestPreciseHandlerCompletesAcceptedBestMovieBeforeSearching(t *testing.T) {
	t.Parallel()
	fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
	fixture.target.Config["best_version"], fixture.target.Config["current_priority"] = true, 100
	baseStore := fixture.store.TargetStore.(*subscriptionFlowStore)
	fixture.handler.store = baseStore
	baseStore.list = func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
		return []database.Subscription{fixture.target}, nil
	}
	fixture.handler.searcher = &subscriptionFlowSearcher{search: func(context.Context, []indexer.Adapter, indexer.Query, func(indexer.Event)) ([]indexer.Resource, error) {
		t.Fatal("already accepted 100 should complete before search")
		return nil, nil
	}}
	if err := fixture.handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if baseStore.completeCalls != 1 || baseStore.completeState != subscription.PendingSearch || !baseStore.completeTime.Equal(fixture.target.UpdatedAt) {
		t.Fatalf("completion calls=%d state=%s", baseStore.completeCalls, baseStore.completeState)
	}
}

func TestRSSHandlerMovieUpgradeGate(t *testing.T) {
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.MediaSource, target.MediaID = "themoviedb", "42"
	target.Config["best_version"], target.Config["current_priority"] = true, 90
	failureState := &v2MovieFailureState{active: make(map[string]struct{})}
	store := &v2MovieRSSStore{rssFlowStore: newRSSFlowStore(target), state: failureState}
	store.subscriptionFlowStore.activeTarget = &target
	adapter := &v2MovieRSSAdapter{v2MovieTestAdapter: &v2MovieTestAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
		bodies:                  map[string][]byte{"101": v2TVTestMetainfo(t, "Example.Movie.2026.mkv"), "102": v2TVTestMetainfo(t, "Example.Movie.2026.Remux.mkv")},
		errs:                    make(map[string]error),
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
	resources := []indexer.Resource{
		{SiteID: 7, Site: "rss", Title: "Example.Movie.2026.1080p.WEB-DL", TorrentID: "101", DownloadID: "101", PriOrder: 90, Seeders: 100},
		{SiteID: 7, Site: "rss", Title: "Example.Movie.2026.1080p.WEB-DL", TorrentID: "102", DownloadID: "102", PriOrder: 100, Seeders: 1},
	}
	matched, err := handler.matchRSSTarget(t.Context(), rssSources(adapters), map[int][]indexer.Resource{7: resources}, target)
	if err != nil || !matched || !reflect.DeepEqual(adapter.calls, []string{"102"}) || submission.plan.MoviePriority == nil || *submission.plan.MoviePriority != 100 {
		t.Fatalf("RSS gate matched=%v err=%v calls=%v plan=%+v", matched, err, adapter.calls, submission.plan)
	}
}

func TestMovieUpgradeGateRunsAfterV2GroupsAndWithoutGroups(t *testing.T) {
	t.Parallel()
	for _, grouped := range []bool{false, true} {
		t.Run(map[bool]string{false: "no groups", true: "assigned priority"}[grouped], func(t *testing.T) {
			store := &v2FilterSettingsStore{subscriptionFlowStore: &subscriptionFlowStore{}, settings: map[string]any{}}
			target := persistedSubscription{MediaType: "电影", Config: map[string]any{"best_version": true, "current_priority": 90}}
			candidate := v2FlowFilterCandidate("Example 2160p")
			candidate.Resource.PriOrder = 90
			if grouped {
				target.Config["filter_groups"] = []any{"4k"}
				store.settings[v2UserFilterRuleGroupsSetting] = []subscription.V2FilterRuleGroup{{Name: "4k", RuleString: "4K"}}
				store.settings[v2CustomFilterRulesSetting] = []subscription.V2CustomFilterRule{}
			}
			got, err := (&Handler{store: store}).filterV2SubscriptionCandidates(t.Context(), target, []subscription.Candidate{candidate})
			if err != nil {
				t.Fatal(err)
			}
			if grouped {
				if len(got) != 1 || got[0].Resource.PriOrder != 100 {
					t.Fatalf("post-group gate=%+v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("same quality admitted without groups: %+v", got)
			}
			if candidate.Resource.PriOrder != 90 {
				t.Fatal("input priority mutated")
			}
		})
	}
}

func TestMovieExecutorGuardsQualityAndFreezesSnapshot(t *testing.T) {
	for _, rss := range []bool{false, true} {
		t.Run(map[bool]string{false: "precise", true: "rss"}[rss], func(t *testing.T) {
			state := subscription.PendingSearch
			if rss {
				state = subscription.Active
			}
			fixture := newV2MovieExecutionFixture(t, state)
			fixture.target.Config["best_version"] = true
			fixture.target.Config["current_priority"] = 90
			fixture.candidates[0].Resource.PriOrder = 90
			fixture.candidates[1].Resource.PriOrder = 100
			// Resource priority must be used, even if a generic quality score disagrees.
			fixture.candidates[0].QualityScore = 1000
			got, err := fixture.handler.executeV2MovieCandidates(t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, rss)
			if err != nil || !got.Accepted || !reflect.DeepEqual(fixture.adapter.calls, []string{"102"}) {
				t.Fatalf("gate result=%+v err=%v calls=%v", got, err, fixture.adapter.calls)
			}
			fixture.candidates[1].Resource.PriOrder = 1
			if fixture.submission.plan.MoviePriority == nil || *fixture.submission.plan.MoviePriority != 100 {
				t.Fatalf("snapshot=%+v", fixture.submission.plan)
			}
		})
	}
}

func TestMovieGateLeavesTVAndNormalMoviesUnchanged(t *testing.T) {
	t.Parallel()
	candidates := []subscription.Candidate{{Resource: indexer.Resource{PriOrder: 80}}, {Resource: indexer.Resource{PriOrder: 90}}}
	for _, target := range []persistedSubscription{
		{MediaType: "电视剧", Config: map[string]any{"best_version": true, "current_priority": 100}},
		{MediaType: "电影", Config: map[string]any{"best_version": false, "current_priority": 100}},
	} {
		got, err := filterMovieUpgradeCandidates(target, candidates)
		if err != nil || !reflect.DeepEqual(got, candidates) {
			t.Fatalf("target=%+v candidates=%+v err=%v", target, got, err)
		}
		if target.MediaType == "电视剧" && movieCandidatePriority(target, candidates[0]) != nil {
			t.Fatal("TV froze movie quality")
		}
	}
	_, err := filterMovieUpgradeCandidates(persistedSubscription{MediaType: "电影", Config: map[string]any{"best_version": true, "current_priority": "bad"}}, candidates)
	if err != ErrSubscriptionConfiguration {
		t.Fatalf("bad current quality error=%v", err)
	}
}
