package downloadflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubmissionServiceSubmitCommitsStableIntentID(t *testing.T) {
	t.Parallel()

	inputID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	storedID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	successInput := submissionTestSuccessInput("alice", inputID)
	successInput.ResourceTerm = "WEB-DL 2160p"
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	transactions := &submissionTestTransactions{tx: tx, calls: &calls}
	store := &submissionTestStore{
		calls: &calls,
		createIntent: func(_ context.Context, gotTx pgx.Tx, username string, intent downloader.Intent) (downloader.Intent, error) {
			if gotTx != tx || username != "alice" || intent.ID != inputID {
				t.Fatalf("CreateDownloadIntentTx() 参数 = (%T, %q, %s)", gotTx, username, intent.ID)
			}
			intent.ID = storedID
			return intent, nil
		},
		createPlan: func(_ context.Context, gotTx pgx.Tx, username string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
			if gotTx != tx || username != "alice" || plan.IntentID != storedID {
				t.Fatalf("CreateDownloadIntentPlanTx() 未使用持久化 intent ID: %+v", plan)
			}
			return plan, nil
		},
		createSuccessInput: func(_ context.Context, gotTx pgx.Tx, username string, input organization.SuccessInput) (organization.SuccessInput, error) {
			want := successInput
			want.IntentID = storedID
			if gotTx != tx || username != "alice" || !reflect.DeepEqual(input, want) {
				t.Fatalf("CreateDownloadIntentSuccessInputTx() 参数 = (%T, %q, %#v), want %#v", gotTx, username, input, want)
			}
			return input, nil
		},
	}
	enqueuer := &submissionTestEnqueuer{
		calls: &calls,
		enqueue: func(_ context.Context, gotTx pgx.Tx, args jobs.DownloadArgs) error {
			if gotTx != tx || args.Username != "alice" || args.IntentID != storedID {
				t.Fatalf("EnqueueDownloadTx() 参数 = (%T, %+v)", gotTx, args)
			}
			return nil
		},
	}
	service, err := NewSubmissionService(transactions, store, enqueuer)
	if err != nil {
		t.Fatalf("NewSubmissionService() error = %v", err)
	}

	got, err := service.Submit(
		t.Context(),
		"alice",
		downloader.Intent{ID: inputID},
		database.DownloadIntentPlan{IntentID: inputID},
		successInput,
	)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got != storedID {
		t.Fatalf("Submit() ID = %s, want %s", got, storedID)
	}
	if tx.rollbackCalls != 0 {
		t.Fatalf("成功提交 Rollback() 次数 = %d, want 0", tx.rollbackCalls)
	}
	wantCalls := []string{"begin", "intent", "plan", "success-input", "enqueue", "commit"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitSubscriptionCommitsLinkStateAndJobAtomically(t *testing.T) {
	t.Parallel()

	const subscriptionID int64 = 42
	intentID := uuid.MustParse("77777777-7777-4777-8777-777777777777")
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
		},
		bind: func(_ context.Context, gotTx pgx.Tx, username string, gotIntentID uuid.UUID, gotSubscriptionID int64) error {
			if gotTx != tx || username != "alice" || gotIntentID != intentID || gotSubscriptionID != subscriptionID {
				t.Fatalf("BindDownloadIntentSubscriptionTx() 参数不匹配")
			}
			return nil
		},
		bindEpisodes: func(
			_ context.Context,
			gotTx pgx.Tx,
			username string,
			gotSubscriptionID int64,
			gotIntentID uuid.UUID,
			episodes []subscription.Episode,
		) error {
			want := []subscription.Episode{{Season: 1, Episode: 2}}
			if gotTx != tx || username != "alice" || gotSubscriptionID != subscriptionID ||
				gotIntentID != intentID || !reflect.DeepEqual(episodes, want) {
				t.Fatalf("BindSubscriptionEpisodesTx() 参数不匹配: episodes=%+v", episodes)
			}
			return nil
		},
		cas: func(_ context.Context, gotTx pgx.Tx, username string, gotSubscriptionID int64, next subscription.State) error {
			if gotTx != tx || username != "alice" || gotSubscriptionID != subscriptionID || next != subscription.Acquired {
				t.Fatalf("CompareAndSwapSubscriptionPreciseSearchStateTx() 参数不匹配")
			}
			return nil
		},
	}
	service, err := NewSubmissionService(
		&submissionTestTransactions{tx: tx, calls: &calls},
		store,
		&submissionTestEnqueuer{calls: &calls, enqueue: func(_ context.Context, gotTx pgx.Tx, args jobs.DownloadArgs) error {
			if gotTx != tx || args.Username != "alice" || args.IntentID != intentID {
				t.Fatalf("EnqueueDownloadTx() 参数不匹配")
			}
			return nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.SubmitSubscriptionEpisodes(
		t.Context(),
		"alice",
		subscriptionID,
		downloader.Intent{ID: intentID},
		database.DownloadIntentPlan{IntentID: intentID},
		submissionTestSuccessInput("alice", intentID),
		[]subscription.Episode{{Season: 1, Episode: 2}},
	)
	if err != nil || got != intentID {
		t.Fatalf("SubmitSubscription() = (%s, %v)", got, err)
	}
	wantCalls := []string{"begin", "intent", "plan", "success-input", "bind", "subscription-cas", "episode-bind", "enqueue", "commit"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitSubscriptionTargetVerifiesVersionBeforeBinding(t *testing.T) {
	t.Parallel()

	const subscriptionID int64 = 52
	intentID := uuid.MustParse("79797979-7979-4797-8797-797979797979")
	expected := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
		},
		verifyTarget: func(_ context.Context, gotTx pgx.Tx, username string, gotID int64, gotVersion time.Time) error {
			if gotTx != tx || username != "alice" || gotID != subscriptionID || !gotVersion.Equal(expected) {
				t.Fatalf("target version parameters = (%T,%q,%d,%v)", gotTx, username, gotID, gotVersion)
			}
			return nil
		},
	}
	service := mustSubmissionTestService(t, &submissionTestTransactions{tx: tx, calls: &calls}, store, &submissionTestEnqueuer{calls: &calls})
	got, err := service.SubmitSubscriptionTargetEpisodes(
		t.Context(), "alice", subscriptionID, expected, downloader.Intent{ID: intentID},
		database.DownloadIntentPlan{IntentID: intentID}, submissionTestSuccessInput("alice", intentID),
		[]subscription.Episode{{Season: 1, Episode: 2}},
	)
	if err != nil || got != intentID {
		t.Fatalf("SubmitSubscriptionTargetEpisodes() = (%s,%v)", got, err)
	}
	wantCalls := []string{
		"begin", "intent", "plan", "success-input", "target-version", "bind",
		"subscription-cas", "episode-bind", "enqueue", "commit",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("call order = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitRSSSubscriptionEpisodesUsesActiveCAS(t *testing.T) {
	t.Parallel()

	const subscriptionID int64 = 142
	intentID := uuid.MustParse("74747474-7474-4747-8747-747474747474")
	episodes := []subscription.Episode{{Season: 1, Episode: 4}}
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
		},
		rssCAS: func(
			_ context.Context,
			gotTx pgx.Tx,
			username string,
			gotSubscriptionID int64,
			expected subscription.State,
		) error {
			if gotTx != tx || username != "alice" || gotSubscriptionID != subscriptionID ||
				expected != subscription.Active {
				t.Fatalf(
					"CompareAndSwapSubscriptionRSSStateTx() parameters = (%T, %q, %d, %q)",
					gotTx, username, gotSubscriptionID, expected,
				)
			}
			return nil
		},
	}
	service := mustSubmissionTestService(
		t,
		&submissionTestTransactions{tx: tx, calls: &calls},
		store,
		&submissionTestEnqueuer{calls: &calls},
	)

	got, err := service.SubmitRSSSubscriptionEpisodes(
		t.Context(),
		"alice",
		subscriptionID,
		subscription.Active,
		downloader.Intent{ID: intentID},
		database.DownloadIntentPlan{IntentID: intentID},
		submissionTestSuccessInput("alice", intentID),
		episodes,
	)
	if err != nil || got != intentID {
		t.Fatalf("SubmitRSSSubscriptionEpisodes() = (%s, %v)", got, err)
	}
	wantCalls := []string{
		"begin", "intent", "plan", "success-input", "bind", "rss-subscription-cas", "episode-bind", "enqueue", "commit",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("call order = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitDoesNotRewriteSuccessInputOwner(t *testing.T) {
	t.Parallel()

	secretErr := errors.New("success input rejected: token=do-not-leak")
	intentID := uuid.MustParse("12121212-1212-4212-8212-121212121212")
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &submissionTestStore{
		calls: &calls,
		createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
			return downloader.Intent{ID: intentID}, nil
		},
		createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
			plan.IntentID = intentID
			return plan, nil
		},
		createSuccessInput: func(_ context.Context, _ pgx.Tx, username string, input organization.SuccessInput) (organization.SuccessInput, error) {
			if username != "alice" || input.Username != "mallory" || input.IntentID != intentID {
				t.Fatalf("success input owner/ID 被提交层改写: owner=%q input=%#v", username, input)
			}
			return organization.SuccessInput{}, secretErr
		},
	}
	service := mustSubmissionTestService(
		t,
		&submissionTestTransactions{tx: tx, calls: &calls},
		store,
		&submissionTestEnqueuer{calls: &calls},
	)

	got, err := service.Submit(
		t.Context(), "alice", downloader.Intent{ID: intentID}, database.DownloadIntentPlan{},
		submissionTestSuccessInput("mallory", intentID),
	)
	assertSubmissionTestFailure(t, got, err, secretErr.Error())
	if tx.commitCalls != 0 || tx.rollbackCalls != 1 {
		t.Fatalf("事务次数: commit=%d rollback=%d, want 0/1", tx.commitCalls, tx.rollbackCalls)
	}
	wantCalls := []string{"begin", "intent", "plan", "success-input", "rollback"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitSubscriptionRollsBackStateFailure(t *testing.T) {
	t.Parallel()

	secretErr := errors.New("database failure with hidden locator")
	intentID := uuid.MustParse("88888888-8888-4888-8888-888888888888")
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
		},
		bind: func(context.Context, pgx.Tx, string, uuid.UUID, int64) error { return nil },
		cas:  func(context.Context, pgx.Tx, string, int64, subscription.State) error { return secretErr },
	}
	service, err := NewSubmissionService(
		&submissionTestTransactions{tx: tx, calls: &calls},
		store,
		&submissionTestEnqueuer{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.SubmitSubscription(
		t.Context(), "alice", 42, downloader.Intent{ID: intentID}, database.DownloadIntentPlan{},
		submissionTestSuccessInput("alice", intentID),
	)
	assertSubmissionTestFailure(t, got, err, secretErr.Error())
	if tx.rollbackCalls != 1 || tx.commitCalls != 0 {
		t.Fatalf("事务次数: commit=%d rollback=%d", tx.commitCalls, tx.rollbackCalls)
	}
	wantCalls := []string{"begin", "intent", "plan", "success-input", "bind", "subscription-cas", "rollback"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitSubscriptionEpisodesReenqueuesVerifiedReplay(t *testing.T) {
	t.Parallel()

	const subscriptionID int64 = 43
	intentID := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	episodes := []subscription.Episode{{Season: 2, Episode: 3}}
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
		},
		cas: func(context.Context, pgx.Tx, string, int64, subscription.State) error {
			return database.ErrSubscriptionPreciseSearchStateConflict
		},
		verify: func(_ context.Context, gotTx pgx.Tx, username string, gotSubscriptionID int64, gotIntentID uuid.UUID, gotEpisodes []subscription.Episode) error {
			if gotTx != tx || username != "alice" || gotSubscriptionID != subscriptionID ||
				gotIntentID != intentID || !reflect.DeepEqual(gotEpisodes, episodes) {
				t.Fatalf("VerifySubscriptionEpisodesBoundToIntentTx() 参数不匹配")
			}
			return nil
		},
	}
	service, err := NewSubmissionService(
		&submissionTestTransactions{tx: tx, calls: &calls},
		store,
		&submissionTestEnqueuer{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.SubmitSubscriptionEpisodes(
		t.Context(), "alice", subscriptionID,
		downloader.Intent{ID: intentID}, database.DownloadIntentPlan{},
		submissionTestSuccessInput("alice", intentID), episodes,
	)
	if err != nil || got != intentID {
		t.Fatalf("verified replay = (%s, %v)", got, err)
	}
	wantCalls := []string{
		"begin", "intent", "plan", "success-input", "bind", "subscription-cas", "episode-verify", "enqueue", "commit",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitSubscriptionEpisodesRollsBackBindingFailure(t *testing.T) {
	t.Parallel()

	secretErr := errors.New("episode binding failed with sensitive-marker")
	intentID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	var calls []string
	tx := &submissionTestTx{calls: &calls}
	store := &subscriptionSubmissionTestStore{
		submissionTestStore: &submissionTestStore{
			calls: &calls,
			createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
				return downloader.Intent{ID: intentID}, nil
			},
			createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
				plan.IntentID = intentID
				return plan, nil
			},
		},
		bindEpisodes: func(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode) error {
			return secretErr
		},
	}
	service, err := NewSubmissionService(
		&submissionTestTransactions{tx: tx, calls: &calls}, store,
		&submissionTestEnqueuer{calls: &calls},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.SubmitSubscriptionEpisodes(
		t.Context(), "alice", 44, downloader.Intent{ID: intentID}, database.DownloadIntentPlan{},
		submissionTestSuccessInput("alice", intentID),
		[]subscription.Episode{{Season: 1, Episode: 1}},
	)
	assertSubmissionTestFailure(t, got, err, "sensitive-marker")
	if tx.rollbackCalls != 1 || tx.commitCalls != 0 {
		t.Fatalf("事务次数: commit=%d rollback=%d", tx.commitCalls, tx.rollbackCalls)
	}
	wantCalls := []string{"begin", "intent", "plan", "success-input", "bind", "subscription-cas", "episode-bind", "rollback"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitRollsBackStepFailures(t *testing.T) {
	t.Parallel()

	secretErr := errors.New("postgres failure: passkey=do-not-leak")
	tests := []struct {
		name            string
		intentErr       error
		planErr         error
		successInputErr error
		enqueueErr      error
		wantCalls       []string
	}{
		{name: "创建意图失败", intentErr: secretErr, wantCalls: []string{"begin", "intent", "rollback"}},
		{name: "创建计划失败", planErr: secretErr, wantCalls: []string{"begin", "intent", "plan", "rollback"}},
		{name: "创建成功输入失败", successInputErr: secretErr, wantCalls: []string{"begin", "intent", "plan", "success-input", "rollback"}},
		{name: "入队失败", enqueueErr: secretErr, wantCalls: []string{"begin", "intent", "plan", "success-input", "enqueue", "rollback"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			intent := downloader.Intent{ID: uuid.MustParse("33333333-3333-4333-8333-333333333333")}
			tx := &submissionTestTx{calls: &calls}
			service := mustSubmissionTestService(t, &submissionTestTransactions{tx: tx, calls: &calls}, &submissionTestStore{
				calls: &calls,
				createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
					return intent, tt.intentErr
				},
				createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
					return plan, tt.planErr
				},
				createSuccessInput: func(_ context.Context, _ pgx.Tx, _ string, input organization.SuccessInput) (organization.SuccessInput, error) {
					return input, tt.successInputErr
				},
			}, &submissionTestEnqueuer{
				calls: &calls,
				enqueue: func(context.Context, pgx.Tx, jobs.DownloadArgs) error {
					return tt.enqueueErr
				},
			})

			got, err := service.Submit(
				t.Context(), "alice", intent, database.DownloadIntentPlan{IntentID: intent.ID},
				submissionTestSuccessInput("alice", intent.ID),
			)
			assertSubmissionTestFailure(t, got, err, secretErr.Error())
			if tx.rollbackCalls != 1 || tx.commitCalls != 0 {
				t.Fatalf("事务次数: commit=%d rollback=%d", tx.commitCalls, tx.rollbackCalls)
			}
			if strings.Join(calls, ",") != strings.Join(tt.wantCalls, ",") {
				t.Fatalf("调用顺序 = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestSubmissionServiceSubmitRollsBackCommitFailure(t *testing.T) {
	t.Parallel()

	secretErr := errors.New("commit failed: password=do-not-leak")
	rollbackSecretErr := errors.New("rollback failed: token=do-not-leak")
	var calls []string
	intent := downloader.Intent{ID: uuid.MustParse("44444444-4444-4444-8444-444444444444")}
	tx := &submissionTestTx{calls: &calls, commitErr: secretErr, rollbackErr: rollbackSecretErr}
	service := mustSubmissionTestService(t, &submissionTestTransactions{tx: tx, calls: &calls}, &submissionTestStore{
		calls: &calls,
		createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
			return intent, nil
		},
		createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
			return plan, nil
		},
	}, &submissionTestEnqueuer{
		calls:   &calls,
		enqueue: func(context.Context, pgx.Tx, jobs.DownloadArgs) error { return nil },
	})

	got, err := service.Submit(
		t.Context(), "alice", intent, database.DownloadIntentPlan{IntentID: intent.ID},
		submissionTestSuccessInput("alice", intent.ID),
	)
	assertSubmissionTestFailure(t, got, err, secretErr.Error())
	if tx.commitCalls != 1 || tx.rollbackCalls != 1 {
		t.Fatalf("事务次数: commit=%d rollback=%d, want 1/1", tx.commitCalls, tx.rollbackCalls)
	}
	wantCalls := []string{"begin", "intent", "plan", "success-input", "enqueue", "commit", "rollback"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitRollsBackCancellationWithoutCanceledContext(t *testing.T) {
	t.Parallel()

	type contextKey struct{}
	key := contextKey{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), key, "trace-value"))
	defer cancel()

	var calls []string
	intent := downloader.Intent{ID: uuid.MustParse("55555555-5555-4555-8555-555555555555")}
	tx := &submissionTestTx{calls: &calls}
	service := mustSubmissionTestService(t, &submissionTestTransactions{tx: tx, calls: &calls}, &submissionTestStore{
		calls: &calls,
		createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
			cancel()
			return downloader.Intent{}, context.Canceled
		},
	}, &submissionTestEnqueuer{calls: &calls})

	got, err := service.Submit(
		ctx, "alice", intent, database.DownloadIntentPlan{IntentID: intent.ID},
		submissionTestSuccessInput("alice", intent.ID),
	)
	if got != uuid.Nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit() = (%s, %v), want (nil UUID, context.Canceled)", got, err)
	}
	if tx.rollbackCalls != 1 || tx.commitCalls != 0 {
		t.Fatalf("事务次数: commit=%d rollback=%d, want 0/1", tx.commitCalls, tx.rollbackCalls)
	}
	if tx.rollbackCtx == nil || tx.rollbackCtxErr != nil {
		t.Fatalf("Rollback() 调用时的 context error = %v, want nil", tx.rollbackCtxErr)
	}
	if _, ok := tx.rollbackCtx.Deadline(); !ok {
		t.Fatal("Rollback() context 缺少独立超时")
	}
	if gotValue := tx.rollbackCtx.Value(key); gotValue != "trace-value" {
		t.Fatalf("Rollback() context value = %v, want trace-value", gotValue)
	}
	wantCalls := []string{"begin", "intent", "rollback"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitCommitsWithRequestContextAndRollsBackCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var calls []string
	intent := downloader.Intent{ID: uuid.MustParse("66666666-6666-4666-8666-666666666666")}
	tx := &submissionTestTx{calls: &calls}
	service := mustSubmissionTestService(t, &submissionTestTransactions{tx: tx, calls: &calls}, &submissionTestStore{
		calls: &calls,
		createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
			return intent, nil
		},
		createPlan: func(_ context.Context, _ pgx.Tx, _ string, plan database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
			return plan, nil
		},
	}, &submissionTestEnqueuer{
		calls: &calls,
		enqueue: func(context.Context, pgx.Tx, jobs.DownloadArgs) error {
			cancel()
			return nil
		},
	})

	got, err := service.Submit(
		ctx, "alice", intent, database.DownloadIntentPlan{IntentID: intent.ID},
		submissionTestSuccessInput("alice", intent.ID),
	)
	if got != uuid.Nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Submit() = (%s, %v), want (nil UUID, context.Canceled)", got, err)
	}
	if tx.commitCtx != ctx || !errors.Is(tx.commitCtx.Err(), context.Canceled) {
		t.Fatalf("Commit() 未使用已取消的请求 context: %v", submissionTestContextError(tx.commitCtx))
	}
	if tx.rollbackCtx == nil || tx.rollbackCtxErr != nil {
		t.Fatalf("Rollback() 调用时的 context error = %v, want nil", tx.rollbackCtxErr)
	}
	if tx.commitCalls != 1 || tx.rollbackCalls != 1 {
		t.Fatalf("事务次数: commit=%d rollback=%d, want 1/1", tx.commitCalls, tx.rollbackCalls)
	}
	wantCalls := []string{"begin", "intent", "plan", "success-input", "enqueue", "commit", "rollback"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("调用顺序 = %v, want %v", calls, wantCalls)
	}
}

func TestSubmissionServiceSubmitSanitizesBeginFailure(t *testing.T) {
	t.Parallel()

	secretErr := errors.New("dial failed: postgres://user:secret@example")
	service := mustSubmissionTestService(t, &submissionTestTransactions{beginErr: secretErr}, &submissionTestStore{}, &submissionTestEnqueuer{})

	got, err := service.Submit(
		t.Context(), "alice", downloader.Intent{}, database.DownloadIntentPlan{}, organization.SuccessInput{},
	)
	assertSubmissionTestFailure(t, got, err, secretErr.Error())
}

func mustSubmissionTestService(
	t *testing.T,
	transactions SubmissionTransactionBeginner,
	store SubmissionStore,
	enqueuer SubmissionEnqueuer,
) *SubmissionService {
	t.Helper()
	service, err := NewSubmissionService(transactions, store, enqueuer)
	if err != nil {
		t.Fatalf("NewSubmissionService() error = %v", err)
	}
	return service
}

func assertSubmissionTestFailure(t *testing.T, got uuid.UUID, err error, secret string) {
	t.Helper()
	if got != uuid.Nil {
		t.Fatalf("Submit() ID = %s, want nil UUID", got)
	}
	if !errors.Is(err, ErrDownloadSubmission) {
		t.Fatalf("Submit() error = %v, want ErrDownloadSubmission", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("Submit() error 泄露底层秘密: %v", err)
	}
}

type submissionTestTransactions struct {
	tx       pgx.Tx
	beginErr error
	calls    *[]string
}

func TestSubmissionPlanConflictIsSafeTypedAndRolledBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		cause, errorWant error
		cancel           bool
	}{
		{"plan conflict", fmt.Errorf("private-plan-detail: %w", database.ErrDownloadIntentPlanConflict), ErrDownloadPlanConflict, false},
		{"persistence stays generic", errors.New("private-plan-detail"), ErrDownloadSubmission, false},
		{"cancellation wins over conflict", database.ErrDownloadIntentPlanConflict, context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			intent := downloader.Intent{ID: uuid.New()}
			tx := &submissionTestTx{calls: &calls}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service := mustSubmissionTestService(t, &submissionTestTransactions{tx: tx, calls: &calls}, &submissionTestStore{
				calls: &calls,
				createIntent: func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error) {
					return intent, nil
				},
				createPlan: func(context.Context, pgx.Tx, string, database.DownloadIntentPlan) (database.DownloadIntentPlan, error) {
					if tc.cancel {
						cancel()
					}
					return database.DownloadIntentPlan{}, tc.cause
				},
			}, &submissionTestEnqueuer{calls: &calls})
			id, err := service.Submit(ctx, "alice", intent, database.DownloadIntentPlan{IntentID: intent.ID}, submissionTestSuccessInput("alice", intent.ID))
			if id != uuid.Nil || !errors.Is(err, tc.errorWant) || strings.Contains(err.Error(), "private-plan-detail") {
				t.Fatalf("id=%s err=%v want=%v", id, err, tc.errorWant)
			}
			if tc.errorWant != ErrDownloadPlanConflict && errors.Is(err, ErrDownloadPlanConflict) {
				t.Fatal("unrelated failure marked as candidate conflict")
			}
			if tx.commitCalls != 0 || tx.rollbackCalls != 1 || strings.Join(calls, ",") != "begin,intent,plan,rollback" {
				t.Fatalf("transaction calls=%v commit=%d rollback=%d", calls, tx.commitCalls, tx.rollbackCalls)
			}
		})
	}
}

func (t *submissionTestTransactions) Begin(context.Context) (pgx.Tx, error) {
	if t.calls != nil {
		*t.calls = append(*t.calls, "begin")
	}
	return t.tx, t.beginErr
}

type submissionTestStore struct {
	calls              *[]string
	createIntent       func(context.Context, pgx.Tx, string, downloader.Intent) (downloader.Intent, error)
	createPlan         func(context.Context, pgx.Tx, string, database.DownloadIntentPlan) (database.DownloadIntentPlan, error)
	createSuccessInput func(context.Context, pgx.Tx, string, organization.SuccessInput) (organization.SuccessInput, error)
}

func (s *submissionTestStore) CreateDownloadIntentTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	intent downloader.Intent,
) (downloader.Intent, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, "intent")
	}
	if s.createIntent == nil {
		return downloader.Intent{}, nil
	}
	return s.createIntent(ctx, tx, username, intent)
}

func (s *submissionTestStore) CreateDownloadIntentPlanTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	plan database.DownloadIntentPlan,
) (database.DownloadIntentPlan, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, "plan")
	}
	if s.createPlan == nil {
		return database.DownloadIntentPlan{}, nil
	}
	return s.createPlan(ctx, tx, username, plan)
}

func (s *submissionTestStore) CreateDownloadIntentSuccessInputTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	input organization.SuccessInput,
) (organization.SuccessInput, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, "success-input")
	}
	if s.createSuccessInput == nil {
		return input, nil
	}
	return s.createSuccessInput(ctx, tx, username, input)
}

type submissionTestEnqueuer struct {
	calls   *[]string
	enqueue func(context.Context, pgx.Tx, jobs.DownloadArgs) error
}

type subscriptionSubmissionTestStore struct {
	*submissionTestStore
	bind         func(context.Context, pgx.Tx, string, uuid.UUID, int64) error
	bindEpisodes func(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode) error
	cas          func(context.Context, pgx.Tx, string, int64, subscription.State) error
	rssCAS       func(context.Context, pgx.Tx, string, int64, subscription.State) error
	verify       func(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode) error
	verifyTarget func(context.Context, pgx.Tx, string, int64, time.Time) error
}

func (s *subscriptionSubmissionTestStore) VerifySubscriptionTargetVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	expected time.Time,
) error {
	if s.calls != nil {
		*s.calls = append(*s.calls, "target-version")
	}
	if s.verifyTarget == nil {
		return nil
	}
	return s.verifyTarget(ctx, tx, username, subscriptionID, expected)
}

func (s *subscriptionSubmissionTestStore) BindDownloadIntentSubscriptionTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	intentID uuid.UUID,
	subscriptionID int64,
) error {
	if s.calls != nil {
		*s.calls = append(*s.calls, "bind")
	}
	if s.bind == nil {
		return nil
	}
	return s.bind(ctx, tx, username, intentID, subscriptionID)
}

func (s *subscriptionSubmissionTestStore) BindSubscriptionEpisodesTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) error {
	if s.calls != nil {
		*s.calls = append(*s.calls, "episode-bind")
	}
	if s.bindEpisodes == nil {
		return nil
	}
	return s.bindEpisodes(ctx, tx, username, subscriptionID, intentID, episodes)
}

func (s *subscriptionSubmissionTestStore) CompareAndSwapSubscriptionPreciseSearchStateTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	next subscription.State,
) error {
	if s.calls != nil {
		*s.calls = append(*s.calls, "subscription-cas")
	}
	if s.cas == nil {
		return nil
	}
	return s.cas(ctx, tx, username, subscriptionID, next)
}

func (s *subscriptionSubmissionTestStore) CompareAndSwapSubscriptionRSSStateTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	expected subscription.State,
) error {
	if s.calls != nil {
		*s.calls = append(*s.calls, "rss-subscription-cas")
	}
	if s.rssCAS == nil {
		return nil
	}
	return s.rssCAS(ctx, tx, username, subscriptionID, expected)
}

func (s *subscriptionSubmissionTestStore) VerifySubscriptionEpisodesBoundToIntentTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) error {
	if s.calls != nil {
		*s.calls = append(*s.calls, "episode-verify")
	}
	if s.verify == nil {
		return nil
	}
	return s.verify(ctx, tx, username, subscriptionID, intentID, episodes)
}

func (e *submissionTestEnqueuer) EnqueueDownloadTx(ctx context.Context, tx pgx.Tx, args jobs.DownloadArgs) error {
	if e.calls != nil {
		*e.calls = append(*e.calls, "enqueue")
	}
	if e.enqueue == nil {
		return nil
	}
	return e.enqueue(ctx, tx, args)
}

type submissionTestTx struct {
	pgx.Tx
	calls          *[]string
	commitErr      error
	rollbackErr    error
	commitCtx      context.Context
	rollbackCtx    context.Context
	rollbackCtxErr error
	commitCalls    int
	rollbackCalls  int
}

func (tx *submissionTestTx) Commit(ctx context.Context) error {
	tx.commitCalls++
	tx.commitCtx = ctx
	if tx.calls != nil {
		*tx.calls = append(*tx.calls, "commit")
	}
	if tx.commitErr != nil {
		return tx.commitErr
	}
	return ctx.Err()
}

func (tx *submissionTestTx) Rollback(ctx context.Context) error {
	tx.rollbackCalls++
	tx.rollbackCtx = ctx
	tx.rollbackCtxErr = ctx.Err()
	if tx.calls != nil {
		*tx.calls = append(*tx.calls, "rollback")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.rollbackErr
}

func submissionTestContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func submissionTestSuccessInput(username string, intentID uuid.UUID) organization.SuccessInput {
	return organization.SuccessInput{
		IntentID: intentID,
		Version:  organization.CurrentSuccessVersion,
		Username: username,
	}
}

var _ SubmissionTransactionBeginner = (*submissionTestTransactions)(nil)
var _ SubmissionStore = (*submissionTestStore)(nil)
var _ SubscriptionSubmissionStore = (*subscriptionSubmissionTestStore)(nil)
var _ RSSSubscriptionSubmissionStore = (*subscriptionSubmissionTestStore)(nil)
var _ TargetSubscriptionSubmissionStore = (*subscriptionSubmissionTestStore)(nil)
var _ SubmissionEnqueuer = (*submissionTestEnqueuer)(nil)
var _ pgx.Tx = (*submissionTestTx)(nil)
