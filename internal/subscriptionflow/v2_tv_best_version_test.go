package subscriptionflow

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestExecuteV2TVBestVersionUsesQualityPendingAndActualSelection(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, map[int]int{1: 100, 2: 20}, false)
	fixture.store.episodes = nil // Ordinary missing rows cannot represent upgrade need.
	candidate := v2TVBestVersionCandidate("partial-upgrade", 30, 1)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv")

	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubscriptionState != subscription.V2TVSubscriptionStateReady ||
		!reflect.DeepEqual(got.RemainingEpisodes, v2TVTestEpisodes(1, 2)) ||
		!reflect.DeepEqual(got.AcceptedCandidateKeys, []string{candidate.Key}) {
		t.Fatalf("best-version selection = %#v", got)
	}
	if len(fixture.submission.calls) != 1 {
		t.Fatalf("stages = %#v", fixture.submission.calls)
	}
	call := fixture.submission.calls[0]
	if !call.plan.Paused || call.plan.TVFullCoverage || call.plan.TVPriority == nil || *call.plan.TVPriority != 30 ||
		!reflect.DeepEqual(call.plan.TVCoverage, v2TVTestEpisodes(1, 2)) ||
		!reflect.DeepEqual(call.episodes, v2TVTestEpisodes(1, 2)) ||
		!reflect.DeepEqual(call.input.Episodes, []organization.Episode{{Season: 1, Episode: 2}}) {
		t.Fatalf("actual selected snapshot = %#v", call)
	}
	if fixture.store.casCalls != 0 || fixture.store.episodeReadCalls != 0 {
		t.Fatalf("quality progress incorrectly used normal facts: CAS=%d episode reads=%d",
			fixture.store.casCalls, fixture.store.episodeReadCalls)
	}
}

func TestExecuteV2TVBestVersionPrefersOnlyFullPackStrictlyAboveEveryTarget(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.PendingSearch, 2, map[int]int{1: 20, 2: 30}, false)
	oneEpisode := v2TVBestVersionCandidate("episode-first", 40, 1, 1)
	pack := v2TVBestVersionCandidate("pack-second", 40, 1)
	fixture.adapter.bodies = map[string][]byte{
		oneEpisode.Resource.DownloadID: v2TVTestMetainfo(t, "Show.S01E01.mkv"),
		pack.Resource.DownloadID:       v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv"),
	}

	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{oneEpisode, pack}, []indexer.Adapter{fixture.adapter}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{pack.Key}) ||
		got.SubscriptionState != subscription.V2TVSubscriptionStateReady || len(got.RemainingEpisodes) != 2 {
		t.Fatalf("full-pack priority selection = %#v", got)
	}
	if len(fixture.submission.calls) != 1 || fixture.submission.calls[0].plan.Paused ||
		!fixture.submission.calls[0].plan.TVFullCoverage ||
		!reflect.DeepEqual(fixture.submission.calls[0].plan.TVCoverage, v2TVTestEpisodes(1, 1, 2)) {
		t.Fatalf("full-pack snapshot = %#v", fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionReturnsAfterPackBeforeHigherEpisodeQuality(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, map[int]int{1: 20, 2: 30}, false)
	episode := v2TVBestVersionCandidate("episode-higher", 100, 1, 1)
	pack := v2TVBestVersionCandidate("pack", 40, 1)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{episode, pack}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil || !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{pack.Key}) ||
		len(fixture.submission.calls) != 1 || len(got.RemainingEpisodes) != 2 {
		t.Fatalf("full-pack batch return = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionDoesNotUpgradeSameEpisodeTwiceInOneBatch(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, map[int]int{1: 20, 2: 80}, false)
	first := v2TVBestVersionCandidate("first", 30, 1, 1)
	later := v2TVBestVersionCandidate("later-higher", 100, 1, 1)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{first, later}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil || !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{first.Key}) ||
		len(fixture.submission.calls) != 1 || len(got.RemainingEpisodes) != 2 ||
		fixture.submission.calls[0].plan.Paused || fixture.submission.calls[0].plan.TVFullCoverage {
		t.Fatalf("one download per episode per batch = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionKeepsTitlePassAheadOfUnpack(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, map[int]int{1: 20, 2: 80}, false)
	pack := v2TVBestVersionCandidate("pack-higher", 40, 1)
	episode := v2TVBestVersionCandidate("title-lower", 30, 1, 1)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{pack, episode}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil || !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{episode.Key}) ||
		len(fixture.submission.calls) != 1 || fixture.submission.calls[0].plan.Paused {
		t.Fatalf("title before unpack = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionTitleContainingWorseQualityUsesUnpack(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, map[int]int{1: 80, 2: 20}, false)
	candidate := v2TVBestVersionCandidate("mixed-quality", 40, 1, 1, 2)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil || len(fixture.submission.calls) != 1 ||
		!fixture.submission.calls[0].plan.Paused || fixture.submission.calls[0].plan.TVFullCoverage ||
		!reflect.DeepEqual(fixture.submission.calls[0].episodes, v2TVTestEpisodes(1, 2)) ||
		!reflect.DeepEqual(got.AcceptedCandidateKeys, []string{candidate.Key}) {
		t.Fatalf("fixed allowed episode gate = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionAcceptsMissingNegativeEpisodeQuality(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 1, nil, false)
	candidate := v2TVBestVersionCandidate("negative-first", -10, 1, 1)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E99.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil || len(fixture.submission.calls) != 1 ||
		!reflect.DeepEqual(got.AcceptedCandidateKeys, []string{candidate.Key}) ||
		fixture.submission.calls[0].plan.Paused || fixture.submission.calls[0].plan.TVFullCoverage ||
		len(got.RemainingEpisodes) != 1 {
		t.Fatalf("negative unknown quality title = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionProjectsFileEpisodeNumbersIntoTitleSeason(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 1, map[int]int{1: 20}, true)
	season := 2
	fixture.target.Season = &season
	fixture.store.activeTarget.Season = &season
	fixture.target.Config["current_priority"] = 20
	fixture.store.activeTarget.Config["current_priority"] = 20
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.E01.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVBestVersionCandidate("season-two-pack", 100, 2)}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil || len(fixture.submission.calls) != 1 ||
		got.SubscriptionState != subscription.V2TVSubscriptionStateCompleted ||
		!reflect.DeepEqual(fixture.submission.calls[0].plan.TVCoverage, v2TVTestEpisodes(2, 1)) {
		t.Fatalf("best-version title season projection = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionContinuesAfterOwnQualityChangesAndKeepsP(t *testing.T) {
	t.Parallel()
	for _, state := range []subscription.State{subscription.PendingSearch, subscription.PendingIdentification} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVBestVersionFixture(t, state, 2, nil, false)
			first := v2TVBestVersionCandidate("first", 100, 1, 1)
			second := v2TVBestVersionCandidate("second", 100, 1, 2)
			fixture.adapter.bodies = map[string][]byte{
				first.Resource.DownloadID:  v2TVTestMetainfo(t, "Show.S01E01.mkv"),
				second.Resource.DownloadID: v2TVTestMetainfo(t, "Show.S01E02.mkv"),
			}
			got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{first, second}, []indexer.Adapter{fixture.adapter}, state == subscription.PendingIdentification)
			if err != nil {
				t.Fatal(err)
			}
			wantState := subscription.V2TVSubscriptionStateCompleted
			wantStageState := subscription.Active
			if state == subscription.PendingIdentification {
				wantState = subscription.V2TVSubscriptionStatePending
				wantStageState = subscription.PendingIdentification
			}
			if got.Stopped || got.SubscriptionState != wantState || len(got.RemainingEpisodes) != 0 ||
				!reflect.DeepEqual(got.AcceptedCandidateKeys, []string{first.Key, second.Key}) ||
				len(fixture.submission.calls) != 2 || fixture.submission.calls[1].rssState != wantStageState ||
				!fixture.submission.calls[1].expectedUpdatedAt.Equal(fixture.target.UpdatedAt.Add(time.Second)) {
				t.Fatalf("serial best-version result/stages = %#v / %#v", got, fixture.submission.calls)
			}
		})
	}
}

func TestExecuteV2TVBestVersionTrustsCompleteTitleStageAsV2Does(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, nil, false)
	candidate := v2TVBestVersionCandidate("title-overstates", 100, 1, 1, 2)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubscriptionState != subscription.V2TVSubscriptionStateCompleted ||
		len(got.RemainingEpisodes) != 0 ||
		len(fixture.submission.calls) != 1 || !fixture.submission.calls[0].plan.TVFullCoverage {
		t.Fatalf("overstated title = result:%#v stages:%#v", got, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionPreservesCompleteTitleSupersetFacts(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, nil, true)
	candidate := v2TVBestVersionCandidate("superset-title", 100, 1, 1, 2, 3)
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil || got.SubscriptionState != subscription.V2TVSubscriptionStateCompleted || len(fixture.submission.calls) != 1 {
		t.Fatalf("complete title superset = %#v / %v / %#v", got, err, fixture.submission.calls)
	}
	call := fixture.submission.calls[0]
	if !reflect.DeepEqual(call.episodes, v2TVTestEpisodes(1, 1, 2)) ||
		!reflect.DeepEqual(call.plan.TVCoverage, v2TVTestEpisodes(1, 1, 2, 3)) ||
		!reflect.DeepEqual(call.input.Episodes, []organization.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}}) ||
		!call.plan.TVFullCoverage || call.plan.Paused {
		t.Fatalf("title facts versus target binding = %#v", call)
	}
}

func TestExecuteV2TVBestVersionFullModeRequiresRealCompleteCoverage(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 2, map[int]int{1: 20, 2: 20}, true)
	fixture.target.Config["current_priority"] = 20
	fixture.store.activeTarget.Config["current_priority"] = 20
	incomplete := v2TVBestVersionCandidate("not-complete", 100, 1, 1, 2)
	complete := v2TVBestVersionCandidate("really-complete", 100, 1)
	fixture.adapter.bodies = map[string][]byte{
		incomplete.Resource.DownloadID: v2TVTestMetainfo(t, "Show.S01E01.mkv"),
		complete.Resource.DownloadID:   v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv"),
	}
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{incomplete, complete}, []indexer.Adapter{fixture.adapter}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubscriptionState != subscription.V2TVSubscriptionStateCompleted ||
		!reflect.DeepEqual(got.AcceptedCandidateKeys, []string{complete.Key}) ||
		len(fixture.submission.calls) != 1 || !fixture.submission.calls[0].plan.TVFullCoverage {
		t.Fatalf("full upgrade coverage = result:%#v stages:%#v", got, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionContinuesOnlyDefinitiveFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		outcome     downloadflow.IntentExecutionOutcome
		wantStages  int
		wantStopped bool
	}{
		{"明确失败继续", downloadflow.IntentExecutionDefinitiveFailureRecovered, 2, false},
		{"永久候选失败继续", downloadflow.IntentExecutionPermanentCandidateFailure, 2, false},
		{"未知停止", downloadflow.IntentExecutionPendingReconciliation, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVBestVersionFixture(t, subscription.Active, 1, map[int]int{1: 20}, false)
			fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: tt.outcome}}
			got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{v2TVBestVersionCandidate("failed", 100, 1, 1),
					v2TVBestVersionCandidate("fallback", 100, 1, 1)}, []indexer.Adapter{fixture.adapter}, true)
			if err != nil {
				t.Fatal(err)
			}
			if got.Stopped != tt.wantStopped || len(fixture.submission.calls) != tt.wantStages {
				t.Fatalf("failure outcome selection = %#v / %#v", got, fixture.submission.calls)
			}
		})
	}
}

func TestExecuteV2TVBestVersionSkipsImmutablePlanConflict(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 1, nil, false)
	first := v2TVBestVersionCandidate("conflicting", 100, 1, 1)
	protected, _, protectErr := downloadflow.ProtectResourceLocator(first.Resource.DownloadID)
	if protectErr != nil {
		t.Fatal(protectErr)
	}
	fixture.submission.stageErrors = map[string]error{protected: downloadflow.ErrDownloadPlanConflict}
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{first, v2TVBestVersionCandidate("fallback", 100, 1, 1)},
		[]indexer.Adapter{fixture.adapter}, true)
	if err != nil || !reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"fallback"}) || fixture.execution.calls != 1 {
		t.Fatalf("immutable conflict fallback = %#v / %v / executions:%d", got, err, fixture.execution.calls)
	}
}

func TestExecuteV2TVBestVersionStopsExternalRuleChangeBeforeStage(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 1, nil, false)
	fixture.store.activeTarget.Config["filter"] = "4K"
	fixture.store.activeTarget.UpdatedAt = fixture.target.UpdatedAt.Add(time.Second)
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVBestVersionCandidate("stale", 100, 1, 1)},
		[]indexer.Adapter{fixture.adapter}, true)
	if err != nil || !got.Stopped || len(fixture.submission.calls) != 0 {
		t.Fatalf("external rule update = %#v / %v / stages:%#v", got, err, fixture.submission.calls)
	}
}

func TestExecuteV2TVBestVersionRequiresAcceptedQualityPersistence(t *testing.T) {
	t.Parallel()
	fixture := newV2TVBestVersionFixture(t, subscription.Active, 1, map[int]int{1: 20}, false)
	fixture.execution.afterExecution = nil
	_, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVBestVersionCandidate("accepted-no-quality", 100, 1, 1)},
		[]indexer.Adapter{fixture.adapter}, true)
	if !errors.Is(err, ErrSubscriptionSubmission) {
		t.Fatalf("acceptance without durable quality = %v", err)
	}
}

func TestExecuteV2TVBestVersionConfirmsOwnerCompletionAndStopsDeletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		completed   bool
		changeRule  bool
		wantStopped bool
	}{
		{name: "持久完成", completed: true},
		{name: "删除不能冒充完成", wantStopped: true},
		{name: "完成后外部规则已变", completed: true, changeRule: true, wantStopped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVBestVersionFixture(t, subscription.Active, 1, nil, false)
			recordFacts := fixture.execution.afterExecution
			fixture.execution.afterExecution = func(intentID uuid.UUID, result downloadflow.IntentExecutionResult) {
				recordFacts(intentID, result)
				if result.Outcome != downloadflow.IntentExecutionAccepted {
					return
				}
				if tt.completed {
					completed := cloneV2TVTestTarget(*fixture.store.activeTarget)
					completed.InternalState = subscription.Completed
					completed.V2State = ""
					completed.CompletedAt = &completed.UpdatedAt
					if tt.changeRule {
						completed.Config["filter"] = "4K"
					}
					fixture.store.ownedTarget = &completed
				}
				fixture.store.activeTarget = nil
			}
			got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{v2TVBestVersionCandidate("accepted", 100, 1, 1)},
				[]indexer.Adapter{fixture.adapter}, true)
			if err != nil || got.Stopped != tt.wantStopped {
				t.Fatalf("owner completion verification = %#v / %v", got, err)
			}
			if tt.wantStopped {
				if len(got.AcceptedCandidateKeys) != 0 || got.SubscriptionState == subscription.V2TVSubscriptionStateCompleted {
					t.Fatalf("unconfirmed completion = %#v", got)
				}
			} else if got.SubscriptionState != subscription.V2TVSubscriptionStateCompleted ||
				!reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"accepted"}) || len(got.RemainingEpisodes) != 0 {
				t.Fatalf("durable completion = %#v", got)
			}
		})
	}
}

func newV2TVBestVersionFixture(
	t *testing.T,
	state subscription.State,
	total int,
	priorities map[int]int,
	full bool,
) v2TVExecutionFixture {
	t.Helper()
	numbers := make([]int, total)
	for index := range numbers {
		numbers[index] = index + 1
	}
	fixture := newV2TVExecutionFixture(t, state, v2TVTestEpisodes(1, numbers...))
	fixture.target.Config["best_version"] = true
	fixture.target.Config["best_version_full"] = full
	fixture.target.Config["episode_priority"] = priorities
	snapshot, _, snapshotErr := tvTargetSnapshot(fixture.target)
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	fixture.target.Config = jsonRoundTripV2TVTestConfig(t, applyTVTargetSnapshot(fixture.target.Config, snapshot))
	fixture.target.UpdatedAt = time.Date(2026, 10, 4, 1, 2, 3, 0, time.UTC)
	fixture.target.V2State, _ = subscription.V2State(state)
	current := cloneV2TVTestTarget(fixture.target)
	fixture.store.activeTarget = &current
	fixture.execution.retainEpisodes = true
	fixture.execution.afterExecution = func(intentID uuid.UUID, result downloadflow.IntentExecutionResult) {
		if result.Outcome == downloadflow.IntentExecutionDefinitiveFailureRecovered ||
			result.Outcome == downloadflow.IntentExecutionPermanentCandidateFailure {
			if current.InternalState != subscription.PendingIdentification {
				current.InternalState = subscription.PendingSearch
				current.V2State = "N"
			}
			current.UpdatedAt = current.UpdatedAt.Add(time.Second)
			return
		}
		if result.Outcome != downloadflow.IntentExecutionAccepted {
			return
		}
		var plan persistedDownloadIntentPlan
		for _, call := range fixture.submission.calls {
			if call.intent.ID == intentID {
				plan = call.plan
				break
			}
		}
		snapshot, _, err := tvTargetSnapshot(current)
		if err != nil {
			t.Fatal(err)
		}
		next, err := subscription.RecordTVDownloadPriority(snapshot, plan.TVPriority, plan.TVCoverage, plan.TVFullCoverage)
		if err != nil {
			t.Fatal(err)
		}
		current.Config = jsonRoundTripV2TVTestConfig(t, applyTVTargetSnapshot(current.Config, next))
		if current.InternalState != subscription.PendingIdentification {
			current.InternalState = subscription.Active
			current.V2State = "R"
		}
		current.UpdatedAt = current.UpdatedAt.Add(time.Second)
	}
	return fixture
}

func v2TVBestVersionCandidate(key string, priority, season int, episodes ...int) subscription.Candidate {
	candidate := v2TVExecutionCandidate(key, season, episodes...)
	candidate.Resource.PriOrder = priority
	return candidate
}
