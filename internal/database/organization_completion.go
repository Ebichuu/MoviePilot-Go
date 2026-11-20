package database

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// CompleteOrganizationBatch 在一个事务内确认全部文件进入终态。新下载接受路径
// 已经结算订阅；这里只为仍绑定 current intent 的 legacy 整理状态机（包括
// pending_index）补做逐集与父订阅结算。
func (s *Store) CompleteOrganizationBatch(ctx context.Context, batchID, token uuid.UUID) (returnErr error) {
	_, _, _, _, err := s.CompleteOrganizationBatchWithSideEffects(
		ctx,
		batchID,
		token,
		mediaindex.CompletionPlan{},
		scrapeflow.Plan{},
		notificationflow.Plan{},
	)
	return err
}

func completeOrganizationSubscription(ctx context.Context, tx pgx.Tx, subscriptionID int64, intentID uuid.UUID) error {
	var (
		state         subscription.State
		currentIntent *uuid.UUID
		completedAt   *time.Time
	)
	if err := tx.QueryRow(ctx, `
		SELECT internal_state, current_download_intent_id, completed_at
		FROM subscriptions
		WHERE id=$1
		FOR UPDATE`, subscriptionID).Scan(&state, &currentIntent, &completedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOrganizationStateConflict
		}
		return organizationPersistenceError(ctx)
	}
	if completedAt != nil {
		return nil
	}
	if currentIntent == nil || *currentIntent != intentID {
		return nil
	}
	switch state {
	case subscription.PendingOrganization:
		if _, err := tx.Exec(ctx, `
			UPDATE subscriptions
			SET internal_state='organizing', v2_state='R', updated_at=now()
			WHERE id=$1`, subscriptionID); err != nil {
			return organizationPersistenceError(ctx)
		}
		state = subscription.Organizing
	case subscription.Organizing, subscription.Indexed:
	case subscription.PendingIndex: // 兼容升级前由 Emby 确认推进的遗留记录。
	case subscription.PendingIdentification, subscription.PendingSearch,
		subscription.Active, subscription.Acquired, subscription.Downloading,
		subscription.Paused, subscription.Completed:
		// Organization is not the subscription fact source for new downloads.
		// Preserve non-legacy states even if a stale current intent remains.
		return nil
	default:
		return ErrOrganizationStateConflict
	}

	if _, err := tx.Exec(ctx, `
		UPDATE subscription_episodes
		SET state='organizing', updated_at=now()
		WHERE subscription_id=$1 AND download_intent_id=$2
			AND state='pending_organization'`, subscriptionID, intentID); err != nil {
		return organizationPersistenceError(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE subscription_episodes
		SET state='indexed', updated_at=now()
		WHERE subscription_id=$1 AND download_intent_id=$2
			AND state IN ('organizing', 'pending_index')`, subscriptionID, intentID); err != nil {
		return organizationPersistenceError(ctx)
	}
	var unfinished int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM subscription_episodes
		WHERE subscription_id=$1 AND download_intent_id=$2 AND state<>'indexed'`, subscriptionID, intentID).Scan(&unfinished); err != nil {
		return organizationPersistenceError(ctx)
	}
	if unfinished != 0 {
		return ErrOrganizationStateConflict
	}
	if state != subscription.Indexed {
		command, err := tx.Exec(ctx, `
			UPDATE subscriptions
			SET internal_state='indexed', v2_state='R', updated_at=now()
			WHERE id=$1 AND completed_at IS NULL AND current_download_intent_id=$2
				AND internal_state IN ('organizing', 'pending_index')`, subscriptionID, intentID)
		if err != nil {
			return organizationPersistenceError(ctx)
		}
		if command.RowsAffected() != 1 {
			return ErrOrganizationStateConflict
		}
	}

	var totalEpisodes, indexedEpisodes int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE state='indexed')
		FROM subscription_episodes
		WHERE subscription_id=$1`, subscriptionID).Scan(&totalEpisodes, &indexedEpisodes); err != nil {
		return organizationPersistenceError(ctx)
	}
	if indexedEpisodes != totalEpisodes {
		command, err := tx.Exec(ctx, `
			UPDATE subscriptions
			SET internal_state='pending_search', v2_state='N',
				current_download_intent_id=NULL, updated_at=now()
			WHERE id=$1 AND completed_at IS NULL AND current_download_intent_id=$2
				AND internal_state='indexed'`, subscriptionID, intentID)
		if err != nil {
			return organizationPersistenceError(ctx)
		}
		if command.RowsAffected() != 1 {
			return ErrOrganizationStateConflict
		}
	} else {
		command, err := tx.Exec(ctx, `
			UPDATE subscriptions
			SET internal_state='completed', v2_state='', completed_at=COALESCE(completed_at, now()),
				current_download_intent_id=NULL, updated_at=now()
			WHERE id=$1 AND completed_at IS NULL AND current_download_intent_id=$2
				AND internal_state='indexed'`, subscriptionID, intentID)
		if err != nil {
			return organizationPersistenceError(ctx)
		}
		if command.RowsAffected() != 1 {
			return ErrOrganizationStateConflict
		}
	}
	return nil
}
