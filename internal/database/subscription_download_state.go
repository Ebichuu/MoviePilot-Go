package database

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

var (
	// ErrInvalidSubscriptionDownloadState 表示下载意图关联状态参数无效。
	ErrInvalidSubscriptionDownloadState = errors.New("订阅下载状态参数无效")
	// ErrSubscriptionDownloadStateNotFound 隐藏不存在、无权或未绑定订阅的意图。
	ErrSubscriptionDownloadStateNotFound = errors.New("订阅下载状态不存在")
	// ErrSubscriptionDownloadStateConflict 表示父订阅已离开预期状态。
	ErrSubscriptionDownloadStateConflict = errors.New("订阅下载状态冲突")
	// ErrSubscriptionDownloadStatePersistence 表示父订阅状态暂时无法持久化。
	ErrSubscriptionDownloadStatePersistence = errors.New("订阅下载状态持久化失败")
)

const compareAndSwapSubscriptionStateByIntentSQL = `
	UPDATE subscriptions AS s
	SET internal_state=$3, v2_state=$4, updated_at=now()
	FROM download_intents AS di
	WHERE di.id=$1 AND di.username=$2 AND di.subscription_id=s.id
		AND s.username=$2 AND s.completed_at IS NULL
		AND s.current_download_intent_id=di.id AND s.internal_state=$5
	RETURNING s.id`

const getSubscriptionStateByIntentSQL = `
	SELECT s.internal_state
	FROM download_intents AS di
	JOIN subscriptions AS s ON s.id=di.subscription_id
	WHERE di.id=$1 AND di.username=$2 AND s.username=$2
		AND s.completed_at IS NULL AND s.current_download_intent_id=di.id`

// CompareAndSwapSubscriptionStateByIntent 通过 owner 的下载意图幂等推进父订阅状态。
func (s *Store) CompareAndSwapSubscriptionStateByIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	current subscription.State,
	next subscription.State,
) error {
	query, args, err := buildCompareAndSwapSubscriptionStateByIntent(username, intentID, current, next)
	if err != nil {
		return err
	}
	if ctx == nil {
		return ErrSubscriptionDownloadStatePersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !downloadIntentStoreReady(s) {
		return ErrSubscriptionDownloadStatePersistence
	}

	var subscriptionID int64
	if err := s.db.Pool.QueryRow(ctx, query, args...).Scan(&subscriptionID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return subscriptionDownloadStatePersistenceError(ctx)
	}

	var stored subscription.State
	if err := s.db.Pool.QueryRow(ctx, getSubscriptionStateByIntentSQL, args[0], args[1]).Scan(&stored); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubscriptionDownloadStateNotFound
		}
		return ErrSubscriptionDownloadStatePersistence
	}
	if stored == next {
		return nil
	}
	if _, err := subscription.V2State(stored); err != nil || stored == subscription.Completed {
		return ErrSubscriptionDownloadStatePersistence
	}
	return ErrSubscriptionDownloadStateConflict
}

func buildCompareAndSwapSubscriptionStateByIntent(
	username string,
	intentID uuid.UUID,
	current subscription.State,
	next subscription.State,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || intentID == uuid.Nil {
		return "", nil, ErrInvalidSubscriptionDownloadState
	}
	if _, err := subscription.Transition(current, next); err != nil || current == next {
		return "", nil, ErrInvalidSubscriptionDownloadState
	}
	projected, err := subscription.V2State(next)
	if err != nil || next == subscription.Completed {
		return "", nil, ErrInvalidSubscriptionDownloadState
	}
	return compareAndSwapSubscriptionStateByIntentSQL, []any{
		intentID,
		username,
		next,
		projected,
		current,
	}, nil
}

func subscriptionDownloadStatePersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrSubscriptionDownloadStatePersistence
}
