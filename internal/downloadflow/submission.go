package downloadflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const submissionRollbackTimeout = 5 * time.Second

var (
	// ErrInvalidSubmissionService 表示下载提交服务缺少必要依赖。
	ErrInvalidSubmissionService = errors.New("下载提交服务依赖不完整")
	// ErrDownloadSubmission 表示下载请求未能安全地原子提交。
	ErrDownloadSubmission = errors.New("提交下载请求失败")
	// ErrDownloadPlanConflict marks a candidate that cannot reuse its immutable plan.
	ErrDownloadPlanConflict = fmt.Errorf("%w：不可变执行计划冲突", ErrDownloadSubmission)
)

// SubmissionTransactionBeginner 是下载提交服务需要的最小事务入口。
type SubmissionTransactionBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// SubmissionStore 是下载提交服务需要的最小持久化接口。
type SubmissionStore interface {
	CreateDownloadIntentTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		intent downloader.Intent,
	) (downloader.Intent, error)
	CreateDownloadIntentPlanTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		plan database.DownloadIntentPlan,
	) (database.DownloadIntentPlan, error)
	CreateDownloadIntentSuccessInputTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		input organization.SuccessInput,
	) (organization.SuccessInput, error)
}

// SubscriptionSubmissionStore extends the normal submission transaction with
// the ownership link and subscription state transition required by automatic
// subscription downloads.
type SubscriptionSubmissionStore interface {
	SubmissionStore
	BindDownloadIntentSubscriptionTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		intentID uuid.UUID,
		subscriptionID int64,
	) error
	CompareAndSwapSubscriptionPreciseSearchStateTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		id int64,
		next subscription.State,
	) error
	BindSubscriptionEpisodesTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		subscriptionID int64,
		intentID uuid.UUID,
		episodes []subscription.Episode,
	) error
	VerifySubscriptionEpisodesBoundToIntentTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		subscriptionID int64,
		intentID uuid.UUID,
		episodes []subscription.Episode,
	) error
}

type TargetSubscriptionSubmissionStore interface {
	SubscriptionSubmissionStore
	VerifySubscriptionTargetVersionTx(context.Context, pgx.Tx, string, int64, time.Time) error
}

// TVBestVersionSubmissionStore verifies independent immutable download coverage
// without replacing episode rows that describe an earlier accepted download.
type TVBestVersionSubmissionStore interface {
	VerifyTVBestVersionSubmissionTx(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode) (bool, error)
	VerifyTVBestVersionSubmissionReplayTx(context.Context, pgx.Tx, string, int64, uuid.UUID, []subscription.Episode, subscription.State) error
}

// RSSSubscriptionSubmissionStore extends the normal subscription transaction
// with the active-state CAS used by RSS.
type RSSSubscriptionSubmissionStore interface {
	SubscriptionSubmissionStore
	CompareAndSwapSubscriptionRSSStateTx(
		ctx context.Context,
		tx pgx.Tx,
		username string,
		subscriptionID int64,
		expected subscription.State,
	) error
}

// SubmissionEnqueuer 是下载提交服务需要的最小事务入队接口。
type SubmissionEnqueuer interface {
	EnqueueDownloadTx(ctx context.Context, tx pgx.Tx, args jobs.DownloadArgs) error
}

// SubmissionService 将下载意图、不可变计划、成功通知输入和 River 任务原子持久化。
type SubmissionService struct {
	transactions SubmissionTransactionBeginner
	store        SubmissionStore
	enqueuer     SubmissionEnqueuer
}

// NewSubmissionService 创建下载请求原子提交服务。
func NewSubmissionService(
	transactions SubmissionTransactionBeginner,
	store SubmissionStore,
	enqueuer SubmissionEnqueuer,
) (*SubmissionService, error) {
	if transactions == nil || store == nil || enqueuer == nil {
		return nil, ErrInvalidSubmissionService
	}
	return &SubmissionService{
		transactions: transactions,
		store:        store,
		enqueuer:     enqueuer,
	}, nil
}

// Submit 在一个 PostgreSQL 事务内创建下载意图、计划、成功通知输入并入队。
// 返回的 UUID 同时是稳定的下载意图标识和任务业务标识。
func (s *SubmissionService) Submit(
	ctx context.Context,
	username string,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
) (id uuid.UUID, returnErr error) {
	return s.submit(ctx, username, 0, intent, plan, successInput, nil)
}

// SubmitSubscription atomically binds the immutable download facts to one
// pending subscription and schedules its download.
func (s *SubmissionService) SubmitSubscription(
	ctx context.Context,
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
) (id uuid.UUID, returnErr error) {
	if subscriptionID <= 0 || s == nil {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(SubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submit(ctx, username, subscriptionID, intent, plan, successInput, nil)
}

// SubmitSubscriptionEpisodes atomically binds the selected TV episode
// coverage to the immutable download facts and schedules its download.
func (s *SubmissionService) SubmitSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (id uuid.UUID, returnErr error) {
	if subscriptionID <= 0 || s == nil {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(SubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submit(ctx, username, subscriptionID, intent, plan, successInput, episodes)
}

// SubmitSubscriptionTargetEpisodes is the version-checked TV submission path.
func (s *SubmissionService) SubmitSubscriptionTargetEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expectedUpdatedAt time.Time,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	if subscriptionID <= 0 || expectedUpdatedAt.IsZero() || s == nil {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(TargetSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, nil, &expectedUpdatedAt, true)
}

// StageSubscriptionEpisodes atomically persists an automatic subscription
// intent without creating a separate River download job. The caller owns its
// immediate execution through Handler.ExecuteIntent.
func (s *SubmissionService) StageSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (id uuid.UUID, returnErr error) {
	if subscriptionID <= 0 || s == nil {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(SubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, nil, nil, false)
}

func (s *SubmissionService) StageSubscriptionTargetEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expectedUpdatedAt time.Time,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	if subscriptionID <= 0 || expectedUpdatedAt.IsZero() || s == nil {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(TargetSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, nil, &expectedUpdatedAt, false)
}

// SubmitRSSSubscriptionEpisodes atomically submits an RSS match for a V2 R or
// P subscription. P remains pending-identification so it cannot complete.
func (s *SubmissionService) SubmitRSSSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expectedState subscription.State,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (id uuid.UUID, returnErr error) {
	if subscriptionID <= 0 || s == nil ||
		(expectedState != subscription.Active && expectedState != subscription.PendingIdentification) {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(RSSSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, &expectedState, nil, true)
}

func (s *SubmissionService) SubmitRSSTargetEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	if subscriptionID <= 0 || expectedUpdatedAt.IsZero() || s == nil ||
		(expectedState != subscription.Active && expectedState != subscription.PendingIdentification) {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(TargetSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	if _, ok := s.store.(RSSSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, &expectedState, &expectedUpdatedAt, true)
}

// StageRSSSubscriptionEpisodes persists one RSS candidate without creating a
// separate River download job. Active and pending-identification CAS semantics
// remain identical to SubmitRSSSubscriptionEpisodes.
func (s *SubmissionService) StageRSSSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expectedState subscription.State,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (id uuid.UUID, returnErr error) {
	if subscriptionID <= 0 || s == nil ||
		(expectedState != subscription.Active && expectedState != subscription.PendingIdentification) {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(RSSSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, &expectedState, nil, false)
}

func (s *SubmissionService) StageRSSTargetEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (uuid.UUID, error) {
	if subscriptionID <= 0 || expectedUpdatedAt.IsZero() || s == nil ||
		(expectedState != subscription.Active && expectedState != subscription.PendingIdentification) {
		return uuid.Nil, ErrDownloadSubmission
	}
	if _, ok := s.store.(TargetSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	if _, ok := s.store.(RSSSubscriptionSubmissionStore); !ok {
		return uuid.Nil, ErrInvalidSubmissionService
	}
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, &expectedState, &expectedUpdatedAt, false)
}

func (s *SubmissionService) submit(
	ctx context.Context,
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
) (id uuid.UUID, returnErr error) {
	return s.submitWithMode(ctx, username, subscriptionID, intent, plan, successInput, episodes, nil, nil, true)
}

func (s *SubmissionService) submitWithMode(
	ctx context.Context,
	username string,
	subscriptionID int64,
	intent downloader.Intent,
	plan database.DownloadIntentPlan,
	successInput organization.SuccessInput,
	episodes []subscription.Episode,
	rssExpectedState *subscription.State,
	targetExpectedUpdatedAt *time.Time,
	enqueue bool,
) (id uuid.UUID, returnErr error) {
	if ctx == nil || s == nil || s.transactions == nil || s.store == nil || s.enqueuer == nil {
		return uuid.Nil, ErrDownloadSubmission
	}
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}

	tx, err := s.transactions.Begin(ctx)
	if err != nil || tx == nil {
		return uuid.Nil, sanitizedSubmissionError(ctx, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submissionRollbackTimeout)
		defer cancel()
		if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) && returnErr == nil {
			id = uuid.Nil
			returnErr = sanitizedSubmissionError(ctx, err)
		}
	}()

	storedIntent, err := s.store.CreateDownloadIntentTx(ctx, tx, username, intent)
	if err != nil || storedIntent.ID == uuid.Nil {
		return uuid.Nil, sanitizedSubmissionError(ctx, err)
	}

	plan.IntentID = storedIntent.ID
	storedPlan, err := s.store.CreateDownloadIntentPlanTx(ctx, tx, username, plan)
	if ctx.Err() == nil && errors.Is(err, database.ErrDownloadIntentPlanConflict) {
		return uuid.Nil, ErrDownloadPlanConflict
	}
	if err != nil || storedPlan.IntentID != storedIntent.ID {
		return uuid.Nil, sanitizedSubmissionError(ctx, err)
	}
	successInput.IntentID = storedIntent.ID
	storedSuccessInput, err := s.store.CreateDownloadIntentSuccessInputTx(ctx, tx, username, successInput)
	if err != nil || storedSuccessInput.IntentID != storedIntent.ID {
		return uuid.Nil, sanitizedSubmissionError(ctx, err)
	}
	if subscriptionID > 0 {
		subscriptionStore := s.store.(SubscriptionSubmissionStore)
		if targetExpectedUpdatedAt != nil {
			if err := s.store.(TargetSubscriptionSubmissionStore).VerifySubscriptionTargetVersionTx(
				ctx, tx, username, subscriptionID, *targetExpectedUpdatedAt,
			); err != nil {
				return uuid.Nil, sanitizedSubmissionError(ctx, err)
			}
		}
		if err := subscriptionStore.BindDownloadIntentSubscriptionTx(
			ctx,
			tx,
			username,
			storedIntent.ID,
			subscriptionID,
		); err != nil {
			return uuid.Nil, sanitizedSubmissionError(ctx, err)
		}
		independentTVFacts := false
		var tvBestStore TVBestVersionSubmissionStore
		if storedPlan.TVPriority != nil && storedPlan.TVCoverage != nil {
			var ok bool
			tvBestStore, ok = s.store.(TVBestVersionSubmissionStore)
			if !ok {
				return uuid.Nil, ErrInvalidSubmissionService
			}
			independentTVFacts, err = tvBestStore.VerifyTVBestVersionSubmissionTx(
				ctx, tx, username, subscriptionID, storedIntent.ID, episodes,
			)
			if err != nil {
				return uuid.Nil, sanitizedSubmissionError(ctx, err)
			}
		}
		replay := false
		var stateErr error
		if rssExpectedState != nil {
			stateErr = s.store.(RSSSubscriptionSubmissionStore).CompareAndSwapSubscriptionRSSStateTx(
				ctx,
				tx,
				username,
				subscriptionID,
				*rssExpectedState,
			)
		} else {
			stateErr = subscriptionStore.CompareAndSwapSubscriptionPreciseSearchStateTx(
				ctx,
				tx,
				username,
				subscriptionID,
				subscription.Acquired,
			)
		}
		if errors.Is(stateErr, database.ErrSubscriptionPreciseSearchStateConflict) && independentTVFacts {
			expectedState := subscription.PendingSearch
			if rssExpectedState != nil {
				expectedState = *rssExpectedState
			}
			if err := tvBestStore.VerifyTVBestVersionSubmissionReplayTx(
				ctx, tx, username, subscriptionID, storedIntent.ID, episodes, expectedState,
			); err != nil {
				return uuid.Nil, sanitizedSubmissionError(ctx, err)
			}
			replay = true
		} else if errors.Is(stateErr, database.ErrSubscriptionPreciseSearchStateConflict) && len(episodes) > 0 {
			if err := subscriptionStore.VerifySubscriptionEpisodesBoundToIntentTx(
				ctx,
				tx,
				username,
				subscriptionID,
				storedIntent.ID,
				episodes,
			); err != nil {
				return uuid.Nil, sanitizedSubmissionError(ctx, err)
			}
			replay = true
		} else if stateErr != nil {
			return uuid.Nil, sanitizedSubmissionError(ctx, stateErr)
		}
		if len(episodes) > 0 && !replay && !independentTVFacts {
			if err := subscriptionStore.BindSubscriptionEpisodesTx(
				ctx,
				tx,
				username,
				subscriptionID,
				storedIntent.ID,
				episodes,
			); err != nil {
				return uuid.Nil, sanitizedSubmissionError(ctx, err)
			}
		}
	}

	if enqueue {
		if err := s.enqueuer.EnqueueDownloadTx(ctx, tx, jobs.DownloadArgs{
			Username: username,
			IntentID: storedIntent.ID,
		}); err != nil {
			return uuid.Nil, sanitizedSubmissionError(ctx, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, sanitizedSubmissionError(ctx, err)
	}
	committed = true
	return storedIntent.ID, nil
}

func sanitizedSubmissionError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrDownloadSubmission
}
