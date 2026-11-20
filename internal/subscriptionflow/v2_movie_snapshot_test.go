package subscriptionflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadflow"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestMovieResolveChangeStopsBeforeStaging(t *testing.T) {
	for _, mode := range []string{"version", "best-version", "identity", "paused", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
			baseStore := fixture.store.TargetStore.(*subscriptionFlowStore)
			fixture.adapter.onResolve = func(string) {
				fixture.target.UpdatedAt = fixture.target.UpdatedAt.Add(time.Second)
				switch mode {
				case "best-version":
					fixture.target.Config["best_version"] = true
				case "identity":
					fixture.target.MediaID = "new-media"
				case "paused":
					fixture.target.InternalState = subscription.Paused
				case "deleted":
					baseStore.activeTarget = nil
				}
			}
			// The outer handler must not finishTarget after the stale loop stops.
			fixture.handler.searcher = &subscriptionFlowSearcher{search: func(_ context.Context, _ []indexer.Adapter, _ indexer.Query, emit func(indexer.Event)) ([]indexer.Resource, error) {
				emit(indexer.Event{Site: "fixture", Resources: []indexer.Resource{fixture.candidates[0].Resource}})
				return nil, nil
			}}
			if err := fixture.handler.handleTarget(t.Context(), fixture.target); err != nil {
				t.Fatal(err)
			}
			if fixture.submission.stageCalls != 0 || len(fixture.execution.calls) != 0 || baseStore.casCalls != 0 {
				t.Fatalf("stale target progressed: stage=%d execute=%d finish=%d", fixture.submission.stageCalls, len(fixture.execution.calls), baseStore.casCalls)
			}
		})
	}
}

func TestMovieRecoveredAttemptRefreshesVersionAndState(t *testing.T) {
	for _, initial := range []subscription.State{subscription.PendingSearch, subscription.Active, subscription.PendingIdentification} {
		t.Run(string(initial), func(t *testing.T) {
			fixture := newV2MovieExecutionFixture(t, initial)
			stager := &movieSnapshotSubmission{subscriptionFlowSubmission: fixture.submission, current: &fixture.target}
			fixture.handler.submission = stager
			fixture.execution.results = []downloadflow.IntentExecutionResult{
				{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered},
				{Outcome: downloadflow.IntentExecutionAccepted},
			}
			firstVersion := fixture.target.UpdatedAt
			result, err := fixture.handler.executeV2MovieCandidates(t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, initial != subscription.PendingSearch)
			if err != nil || !result.Accepted || !reflect.DeepEqual(stager.versions, []time.Time{firstVersion, firstVersion.Add(time.Second)}) {
				t.Fatalf("result=%+v err=%v versions=%v", result, err, stager.versions)
			}
			if initial == subscription.PendingIdentification {
				if !reflect.DeepEqual(fixture.submission.rssStates, []subscription.State{initial, initial}) || fixture.submission.stageCalls != 0 {
					t.Fatalf("P changed: rss=%v precise=%d", fixture.submission.rssStates, fixture.submission.stageCalls)
				}
			} else if fixture.target.InternalState != subscription.PendingSearch {
				t.Fatalf("recovery state=%s", fixture.target.InternalState)
			}
		})
	}
}

func TestMovieRecoveredAttemptStopsAfterExternalChanges(t *testing.T) {
	for _, mode := range []string{"mode", "identity", "paused", "new-current", "missing", "storage"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newV2MovieExecutionFixture(t, subscription.Active)
			baseStore := fixture.store.TargetStore.(*subscriptionFlowStore)
			recover := fixture.execution.onResult
			fixture.execution.onResult = func(result downloadflow.IntentExecutionResult) {
				recover(result)
				switch mode {
				case "mode":
					fixture.target.Config["best_version"] = true
				case "identity":
					fixture.target.MediaID = "another"
				case "paused":
					fixture.target.InternalState = subscription.Paused
				case "new-current":
					fixture.target.InternalState = subscription.Acquired
				case "missing":
					baseStore.activeTarget = nil
				case "storage":
					baseStore.activeTargetErr = errors.New("database unavailable")
				}
			}
			fixture.execution.results = []downloadflow.IntentExecutionResult{{Outcome: downloadflow.IntentExecutionDefinitiveFailureRecovered}}
			result, err := fixture.handler.executeV2MovieCandidates(t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, true)
			if mode == "storage" {
				if !errors.Is(err, ErrSubscriptionSearchStorage) {
					t.Fatalf("storage error=%v", err)
				}
			} else if err != nil || !result.Stopped {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if len(fixture.execution.calls) != 1 || !reflect.DeepEqual(fixture.adapter.calls, []string{"101"}) {
				t.Fatalf("next stale candidate ran: execute=%v resolve=%v", fixture.execution.calls, fixture.adapter.calls)
			}
		})
	}
}

func TestMoviePlanConflictSkipsCandidateAndStorageErrorStops(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "storage failure", true: "immutable quality conflict"}[conflict], func(t *testing.T) {
			fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
			failure := errors.New("database unavailable")
			if conflict {
				failure = downloadflow.ErrDownloadPlanConflict
			}
			fixture.handler.submission = &movieSnapshotSubmission{
				subscriptionFlowSubmission: fixture.submission, current: &fixture.target,
				firstFailure: failure,
			}
			result, err := fixture.handler.executeV2MovieCandidates(t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, false)
			if conflict {
				if err != nil || !result.Accepted || len(fixture.execution.calls) != 1 || fixture.submission.plan.ResourceID != "102" {
					t.Fatalf("conflicting old plan reused: result=%+v error=%v calls=%v plan=%+v", result, err, fixture.execution.calls, fixture.submission.plan)
				}
			} else if !errors.Is(err, ErrSubscriptionSubmission) || len(fixture.execution.calls) != 0 || len(fixture.adapter.calls) != 1 {
				t.Fatalf("storage failure swallowed: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestMovieStagingRejectsVersionChangeAfterRead(t *testing.T) {
	fixture := newV2MovieExecutionFixture(t, subscription.PendingSearch)
	stager := &movieSnapshotSubmission{subscriptionFlowSubmission: fixture.submission, current: &fixture.target}
	stager.beforeVerify = func() { fixture.target.UpdatedAt = fixture.target.UpdatedAt.Add(time.Second) }
	fixture.handler.submission = stager
	_, err := fixture.handler.executeV2MovieCandidates(t.Context(), fixture.target, fixture.candidates, []indexer.Adapter{fixture.adapter}, false)
	if !errors.Is(err, ErrSubscriptionSubmission) || len(fixture.execution.calls) != 0 || fixture.submission.stageCalls != 0 {
		t.Fatalf("transactional version guard failed: error=%v executions=%v stages=%d", err, fixture.execution.calls, fixture.submission.stageCalls)
	}
}

type movieSnapshotSubmission struct {
	*subscriptionFlowSubmission
	current      *database.Subscription
	versions     []time.Time
	firstFailure error
	beforeVerify func()
}

func (s *movieSnapshotSubmission) StageSubscriptionTargetEpisodes(ctx context.Context, username string, subscriptionID int64, version time.Time, intent downloader.Intent, plan database.DownloadIntentPlan, input organization.SuccessInput, episodes []subscription.Episode) (uuid.UUID, error) {
	if err := s.verifyVersion(version); err != nil {
		return uuid.Nil, err
	}
	return s.subscriptionFlowSubmission.StageSubscriptionTargetEpisodes(ctx, username, subscriptionID, version, intent, plan, input, episodes)
}

func (s *movieSnapshotSubmission) StageRSSTargetEpisodes(ctx context.Context, username string, subscriptionID int64, expected subscription.State, version time.Time, intent downloader.Intent, plan database.DownloadIntentPlan, input organization.SuccessInput, episodes []subscription.Episode) (uuid.UUID, error) {
	if err := s.verifyVersion(version); err != nil {
		return uuid.Nil, err
	}
	return s.subscriptionFlowSubmission.StageRSSTargetEpisodes(ctx, username, subscriptionID, expected, version, intent, plan, input, episodes)
}

func (s *movieSnapshotSubmission) verifyVersion(version time.Time) error {
	s.versions = append(s.versions, version)
	if s.beforeVerify != nil {
		s.beforeVerify()
	}
	if s.firstFailure != nil {
		err := s.firstFailure
		s.firstFailure = nil
		return err
	}
	if !version.Equal(s.current.UpdatedAt) {
		return database.ErrSubscriptionPreciseSearchStateConflict
	}
	return nil
}
