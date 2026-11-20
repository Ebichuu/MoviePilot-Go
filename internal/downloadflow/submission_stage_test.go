package downloadflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubmissionServiceStageSubscriptionEpisodesCommitsWithoutDownloadJob(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	episodes := []subscription.Episode{{Season: 1, Episode: 2}}
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(
				_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan,
			) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
			createSuccessInput: func(
				_ context.Context, _ pgx.Tx, _ string, input organization.SuccessInput,
			) (organization.SuccessInput, error) {
				input.IntentID = intentID
				return input, nil
			},
		},
	}
	service := mustSubmissionTestService(
		t,
		&submissionTestTransactions{tx: tx, calls: &calls},
		store,
		&submissionTestEnqueuer{calls: &calls},
	)

	got, err := service.StageSubscriptionEpisodes(
		t.Context(), "alice", 42, downloader.Intent{ID: intentID},
		database.DownloadIntentPlan{IntentID: intentID},
		submissionTestSuccessInput("alice", intentID), episodes,
	)
	if err != nil || got != intentID {
		t.Fatalf("StageSubscriptionEpisodes() = (%s, %v)", got, err)
	}
	want := []string{
		"begin", "intent", "plan", "success-input", "bind", "subscription-cas", "episode-bind", "commit",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("call order = %v, want %v", calls, want)
	}
}

func TestSubmissionServiceStageRSSSubscriptionEpisodesUsesRSSCASWithoutDownloadJob(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(
				_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan,
			) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
			createSuccessInput: func(
				_ context.Context, _ pgx.Tx, _ string, input organization.SuccessInput,
			) (organization.SuccessInput, error) {
				input.IntentID = intentID
				return input, nil
			},
		},
	}
	service := mustSubmissionTestService(
		t,
		&submissionTestTransactions{tx: tx, calls: &calls},
		store,
		&submissionTestEnqueuer{calls: &calls},
	)

	got, err := service.StageRSSSubscriptionEpisodes(
		t.Context(), "alice", 43, subscription.Active,
		downloader.Intent{ID: intentID}, database.DownloadIntentPlan{IntentID: intentID},
		submissionTestSuccessInput("alice", intentID),
		[]subscription.Episode{{Season: 2, Episode: 3}},
	)
	if err != nil || got != intentID {
		t.Fatalf("StageRSSSubscriptionEpisodes() = (%s, %v)", got, err)
	}
	want := []string{"begin", "intent", "plan", "success-input", "bind", "rss-subscription-cas", "episode-bind", "commit"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("call order = %v, want %v", calls, want)
	}
}
