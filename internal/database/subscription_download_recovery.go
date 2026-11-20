package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const recoverSubscriptionDownloadIntentSQL = `
	WITH target AS MATERIALIZED (
		UPDATE download_intents
		SET state='failed', updated_at=now()
		WHERE id=$1 AND username=$2 AND state=ANY($3::text[])
			AND ($4::timestamptz IS NULL OR updated_at=$4)
			AND (
				subscription_id IS NULL
				OR EXISTS (
					SELECT 1 FROM subscriptions AS active_subscription
					WHERE active_subscription.id=download_intents.subscription_id
						AND active_subscription.username=$2
						AND active_subscription.completed_at IS NULL
						AND active_subscription.current_download_intent_id=download_intents.id
						AND active_subscription.internal_state=ANY($7::text[])
				)
				OR (
					state='failed'
					AND EXISTS (
						SELECT 1 FROM subscriptions AS recovered_subscription
						WHERE recovered_subscription.id=download_intents.subscription_id
							AND recovered_subscription.username=$2
							AND recovered_subscription.completed_at IS NULL
							AND recovered_subscription.current_download_intent_id IS NULL
							AND recovered_subscription.internal_state IN ($5, $8)
					)
				)
			)
		RETURNING subscription_id
	), recovered_episodes AS (
		UPDATE subscription_episodes AS se
		SET state='missing', download_intent_id=NULL, updated_at=now()
		FROM target
		WHERE target.subscription_id IS NOT NULL
			AND se.subscription_id=target.subscription_id
			AND se.download_intent_id=$1
			AND se.state IN ('acquired', 'downloading')
			AND EXISTS (
				SELECT 1 FROM subscriptions AS owner
				WHERE owner.id=se.subscription_id AND owner.username=$2
					AND owner.completed_at IS NULL
			)
		RETURNING se.id
	), recovered_subscription AS (
		UPDATE subscriptions AS s
		SET internal_state=CASE WHEN s.internal_state=$8 THEN $8 ELSE $5 END,
			v2_state=CASE WHEN s.internal_state=$8 THEN $9 ELSE $6 END,
			current_download_intent_id=NULL, updated_at=now()
		FROM target
		WHERE target.subscription_id IS NOT NULL
			AND s.id=target.subscription_id AND s.username=$2
			AND s.completed_at IS NULL
			AND s.current_download_intent_id=$1
			AND s.internal_state=ANY($7::text[])
		RETURNING s.id
	)
	SELECT EXISTS(SELECT 1 FROM target)`

// RecoverSubscriptionDownloadIntent 原子记录一次确认失败，并释放订阅逐集与父状态。
// expectedUpdatedAt 非空时同时充当租约版本，避免过期观察覆盖并发进展。
func (s *Store) RecoverSubscriptionDownloadIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	fromStates []downloader.IntentState,
	expectedUpdatedAt *time.Time,
) error {
	query, args, err := buildRecoverSubscriptionDownloadIntent(
		username,
		intentID,
		fromStates,
		expectedUpdatedAt,
	)
	if err != nil {
		return err
	}
	if ctx == nil {
		return ErrDownloadIntentPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !downloadIntentStoreReady(s) {
		return ErrDownloadIntentPersistence
	}
	var recovered bool
	if err := s.db.Pool.QueryRow(ctx, query, args...).Scan(&recovered); err != nil {
		return downloadIntentPersistenceError(ctx)
	}
	if recovered {
		return nil
	}
	var current downloader.IntentState
	if err := s.db.Pool.QueryRow(ctx, getDownloadIntentStateSQL, intentID, strings.TrimSpace(username)).Scan(&current); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDownloadIntentNotFound
		}
		return ErrDownloadIntentPersistence
	}
	return ErrDownloadIntentStateConflict
}

func buildRecoverSubscriptionDownloadIntent(
	username string,
	intentID uuid.UUID,
	fromStates []downloader.IntentState,
	expectedUpdatedAt *time.Time,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || intentID == uuid.Nil || len(fromStates) == 0 {
		return "", nil, ErrInvalidDownloadIntent
	}
	seen := make(map[downloader.IntentState]struct{}, len(fromStates))
	normalized := make([]string, 0, len(fromStates))
	for _, state := range fromStates {
		// downloading 用于「任务已在下载器中消失」的失败回收，其余状态不允许回滚。
		if state != downloader.IntentPlanned && state != downloader.IntentSubmitting &&
			state != downloader.IntentDownloading && state != downloader.IntentFailed {
			return "", nil, ErrInvalidDownloadIntentState
		}
		if _, exists := seen[state]; exists {
			continue
		}
		seen[state] = struct{}{}
		normalized = append(normalized, string(state))
	}
	if len(normalized) == 0 || len(normalized) > 4 {
		return "", nil, ErrInvalidDownloadIntent
	}
	var expected any
	if expectedUpdatedAt != nil {
		if expectedUpdatedAt.IsZero() {
			return "", nil, ErrInvalidDownloadIntent
		}
		expected = expectedUpdatedAt.UTC()
	}
	v2State, err := subscription.V2State(subscription.PendingSearch)
	if err != nil {
		return "", nil, ErrInvalidDownloadIntentState
	}
	return recoverSubscriptionDownloadIntentSQL, []any{
		intentID,
		username,
		normalized,
		expected,
		subscription.PendingSearch,
		v2State,
		[]string{
			string(subscription.Acquired),
			string(subscription.Downloading),
			string(subscription.PendingIdentification),
		},
		subscription.PendingIdentification,
		"P",
	}, nil
}
