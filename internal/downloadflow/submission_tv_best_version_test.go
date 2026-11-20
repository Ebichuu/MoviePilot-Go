package downloadflow

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubmissionTVBestVersionPreservesEarlierEpisodeBinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		best       bool
		rss        bool
		replay     bool
		knownEmpty bool
	}{
		{name: "precise independent coverage", best: true},
		{name: "RSS independent coverage", best: true, rss: true},
		{name: "ordinary still binds episodes"},
		{name: "precise independent replay", best: true, replay: true},
		{name: "RSS P independent replay", best: true, rss: true, replay: true},
		{name: "known empty still verifies independent replay", best: true, replay: true, knownEmpty: true},
		{name: "ordinary replay still checks exact bound rows", replay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newTVSubmissionTestFixture(t)
			if tc.knownEmpty {
				fixture.episodes = []subscription.Episode{}
				fixture.plan.TVCoverage = []subscription.Episode{}
			}
			fixture.store.verifyTV = func(_ context.Context, tx pgx.Tx, owner string, id int64, intentID uuid.UUID, actual []subscription.Episode) (bool, error) {
				if tx != fixture.tx || owner != "alice" || id != 42 || intentID != fixture.intent.ID || !slices.Equal(actual, fixture.episodes) {
					t.Fatalf("independent verification received another binding: %q %d %v %v", owner, id, intentID, actual)
				}
				return tc.best, nil
			}
			if tc.replay {
				fixture.store.cas = func(context.Context, pgx.Tx, string, int64, subscription.State) error {
					return database.ErrSubscriptionPreciseSearchStateConflict
				}
				fixture.store.rssCAS = fixture.store.cas
			}
			fixture.store.verifyReplay = func(_ context.Context, tx pgx.Tx, owner string, id int64, intentID uuid.UUID, actual []subscription.Episode, expected subscription.State) error {
				wantState := subscription.PendingSearch
				if tc.rss {
					wantState = subscription.PendingIdentification
				}
				if tx != fixture.tx || owner != "alice" || id != 42 || intentID != fixture.intent.ID || !slices.Equal(actual, fixture.episodes) || expected != wantState {
					t.Fatalf("replay did not retain exact independent facts and expected state: state=%s episodes=%v", expected, actual)
				}
				return nil
			}
			var id uuid.UUID
			var err error
			if tc.rss {
				id, err = fixture.service.StageRSSTargetEpisodes(t.Context(), "alice", 42, subscription.PendingIdentification,
					time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), fixture.intent, fixture.plan, fixture.input, fixture.episodes)
			} else {
				id, err = fixture.service.SubmitSubscriptionTargetEpisodes(t.Context(), "alice", 42,
					time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), fixture.intent, fixture.plan, fixture.input, fixture.episodes)
			}
			if err != nil || id != fixture.intent.ID || fixture.tx.commitCalls != 1 || fixture.tx.rollbackCalls != 0 {
				t.Fatalf("submit id=%v err=%v committed=%d rolledBack=%d", id, err, fixture.tx.commitCalls, fixture.tx.rollbackCalls)
			}
			want := []string{"begin", "intent", "plan", "success-input", "target-version", "bind", "tv-verify"}
			if tc.rss {
				want = append(want, "rss-subscription-cas")
			} else {
				want = append(want, "subscription-cas")
			}
			if tc.replay && tc.best {
				want = append(want, "tv-replay")
			} else if tc.replay {
				want = append(want, "episode-verify")
			} else if !tc.best {
				want = append(want, "episode-bind")
			}
			if !tc.rss {
				want = append(want, "enqueue")
			}
			want = append(want, "commit")
			if !reflect.DeepEqual(*fixture.calls, want) {
				t.Fatalf("submission calls=%v want=%v", *fixture.calls, want)
			}
		})
	}
}

func TestSubmissionTVBestVersionVerificationFailuresRollback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		initialErr error
		replayErr  error
		replay     bool
	}{
		{name: "coverage or owner conflict", initialErr: database.ErrSubscriptionEpisodeBindingConflict},
		{name: "initial context cancellation", initialErr: context.Canceled},
		{name: "replay parent state changed", replay: true, replayErr: database.ErrSubscriptionEpisodeBindingConflict},
		{name: "replay context deadline", replay: true, replayErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newTVSubmissionTestFixture(t)
			fixture.store.verifyTV = func(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode) (bool, error) {
				return true, tc.initialErr
			}
			fixture.store.verifyReplay = func(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode, subscription.State) error {
				return tc.replayErr
			}
			if tc.replay {
				fixture.store.cas = func(context.Context, pgx.Tx, string, int64, subscription.State) error {
					return database.ErrSubscriptionPreciseSearchStateConflict
				}
			}
			id, err := fixture.service.SubmitSubscriptionEpisodes(t.Context(), "alice", 42,
				fixture.intent, fixture.plan, fixture.input, fixture.episodes)
			wantErr := ErrDownloadSubmission
			cause := tc.initialErr
			if tc.replay {
				cause = tc.replayErr
			}
			if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
				wantErr = cause
			}
			if id != uuid.Nil || !errors.Is(err, wantErr) || fixture.tx.commitCalls != 0 || fixture.tx.rollbackCalls != 1 {
				t.Fatalf("verification failure id=%v err=%v commit=%d rollback=%d", id, err, fixture.tx.commitCalls, fixture.tx.rollbackCalls)
			}
			for _, forbidden := range []string{"episode-bind", "episode-verify", "enqueue", "commit"} {
				if slices.Contains(*fixture.calls, forbidden) {
					t.Fatalf("failed independent verification executed %s: %v", forbidden, *fixture.calls)
				}
			}
		})
	}
}

func TestSubmissionTVFactsRequireVerifierAndLegacyKeepsBinding(t *testing.T) {
	t.Parallel()
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy unknown coverage", true: "new facts missing verifier"}[known], func(t *testing.T) {
			t.Parallel()
			fixture := newTVSubmissionTestFixture(t)
			if !known {
				fixture.plan.TVPriority, fixture.plan.TVCoverage = nil, nil
			}
			service, err := NewSubmissionService(&submissionTestTransactions{tx: fixture.tx, calls: fixture.calls},
				fixture.store.subscriptionSubmissionTestStore, &submissionTestEnqueuer{calls: fixture.calls})
			if err != nil {
				t.Fatal(err)
			}
			id, err := service.SubmitSubscriptionEpisodes(t.Context(), "alice", 42,
				fixture.intent, fixture.plan, fixture.input, fixture.episodes)
			if known {
				if id != uuid.Nil || !errors.Is(err, ErrInvalidSubmissionService) || fixture.tx.rollbackCalls != 1 {
					t.Fatalf("unverified TV facts admitted: id=%v err=%v", id, err)
				}
			} else if err != nil || id != fixture.intent.ID || !slices.Contains(*fixture.calls, "episode-bind") {
				t.Fatalf("legacy episode binding changed: id=%v err=%v calls=%v", id, err, *fixture.calls)
			}
		})
	}
}

type tvBestVersionSubmissionTestStore struct {
	*subscriptionSubmissionTestStore
	verifyTV     func(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode) (bool, error)
	verifyReplay func(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode, subscription.State) error
}

func (s *tvBestVersionSubmissionTestStore) VerifyTVBestVersionSubmissionTx(
	ctx context.Context, tx pgx.Tx, username string, id int64, intentID uuid.UUID, episodes []subscription.Episode,
) (bool, error) {
	*s.calls = append(*s.calls, "tv-verify")
	if s.verifyTV != nil {
		return s.verifyTV(ctx, tx, username, id, intentID, episodes)
	}
	return true, nil
}

func (s *tvBestVersionSubmissionTestStore) VerifyTVBestVersionSubmissionReplayTx(
	ctx context.Context, tx pgx.Tx, username string, id int64, intentID uuid.UUID,
	episodes []subscription.Episode, expected subscription.State,
) error {
	*s.calls = append(*s.calls, "tv-replay")
	if s.verifyReplay != nil {
		return s.verifyReplay(ctx, tx, username, id, intentID, episodes, expected)
	}
	return nil
}

type tvSubmissionTestFixture struct {
	calls    *[]string
	tx       *submissionTestTx
	store    *tvBestVersionSubmissionTestStore
	service  *SubmissionService
	intent   downloader.Intent
	plan     database.DownloadIntentPlan
	input    organization.SuccessInput
	episodes []subscription.Episode
}

func newTVSubmissionTestFixture(t *testing.T) tvSubmissionTestFixture {
	t.Helper()
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	intent := downloader.Intent{ID: uuid.New()}
	store := &tvBestVersionSubmissionTestStore{subscriptionSubmissionTestStore: &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return intent, nil
			},
			createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
				return plan, nil
			},
		},
	}}
	service, err := NewSubmissionService(&submissionTestTransactions{tx: tx, calls: &calls}, store, &submissionTestEnqueuer{calls: &calls})
	if err != nil {
		t.Fatal(err)
	}
	priority := 100
	episodes := []subscription.Episode{{Season: 1, Episode: 2}}
	return tvSubmissionTestFixture{calls: &calls, tx: tx, store: store, service: service, intent: intent,
		plan:  database.DownloadIntentPlan{TVPriority: &priority, TVCoverage: slices.Clone(episodes)},
		input: submissionTestSuccessInput("alice", intent.ID), episodes: episodes}
}

var _ TVBestVersionSubmissionStore = (*tvBestVersionSubmissionTestStore)(nil)
