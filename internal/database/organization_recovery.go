package database

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FailOrganizationBatch 记录批次失败、释放租约，并让下一轮在 retryAt 后恢复。
func (s *Store) FailOrganizationBatch(
	ctx context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	retryAt time.Time,
	stableErr string,
) (returnErr error) {
	if batchID == uuid.Nil || token == uuid.Nil || retryAt.IsZero() {
		return ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return err
	}
	stableErr = sanitizeOrganizationError(stableErr)
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) && returnErr == nil {
			returnErr = organizationPersistenceError(ctx)
		}
	}()
	var intentID uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE organization_batches
		SET state='failed', last_error=$4, next_attempt_at=$3,
			lease_token=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND state='organizing' AND lease_token=$2
			AND lease_expires_at > now()
		RETURNING intent_id`, batchID, token, retryAt.UTC(), stableErr).Scan(&intentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return organizationLeaseFailure(ctx, s, batchID)
	}
	if err != nil {
		return organizationPersistenceError(ctx)
	}
	subscriptionID, locked, err := lockOrganizationSubscriptionByIntent(ctx, tx, intentID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE organization_files
		SET state='failed', last_error=$2, completed_at=NULL, updated_at=now()
		WHERE batch_id=$1 AND state='organizing'`, batchID, stableErr); err != nil {
		return organizationPersistenceError(ctx)
	}
	if locked {
		if err := rollbackOrganizationSubscription(ctx, tx, subscriptionID, intentID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return organizationPersistenceError(ctx)
	}
	return nil
}

// RecoverExpiredOrganizationLeases 有界回收过期批次和执行中的文件。
func (s *Store) RecoverExpiredOrganizationLeases(ctx context.Context, now time.Time, limit int) (int64, error) {
	if now.IsZero() || limit < 1 || limit > maxOrganizationClaimSize {
		return 0, ErrInvalidOrganizationInput
	}
	if err := validateOrganizationStore(s, ctx); err != nil {
		return 0, err
	}
	now = now.UTC()
	var recovered int64
	err := s.db.Pool.QueryRow(ctx, `
		WITH candidates AS MATERIALIZED (
			SELECT id, intent_id
			FROM organization_batches
			WHERE state='organizing' AND lease_expires_at <= $1
			ORDER BY lease_expires_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		), locked_parents AS MATERIALIZED (
			SELECT s.id AS subscription_id, candidates.intent_id
			FROM subscriptions AS s
			JOIN download_intents AS di
				ON di.subscription_id=s.id AND di.username=s.username
			JOIN candidates ON candidates.intent_id=di.id
			WHERE s.completed_at IS NULL AND s.current_download_intent_id=di.id
				AND s.internal_state='organizing'
			FOR UPDATE OF s
		), parent_gate AS MATERIALIZED (
			SELECT count(*) FROM locked_parents
		), files AS (
			UPDATE organization_files AS f
			SET state='failed', last_error='lease expired', completed_at=NULL, updated_at=$1
			FROM candidates
			CROSS JOIN parent_gate
			WHERE f.batch_id=candidates.id AND f.state='organizing'
			RETURNING f.id
		), episodes AS (
			UPDATE subscription_episodes AS se
			SET state='pending_organization', updated_at=$1
			FROM locked_parents
			WHERE se.subscription_id=locked_parents.subscription_id
				AND se.download_intent_id=locked_parents.intent_id
				AND se.state='organizing'
			RETURNING se.id
		), parents AS (
			UPDATE subscriptions AS s
			SET internal_state='pending_organization', v2_state='R', updated_at=$1
			FROM locked_parents
			WHERE s.id=locked_parents.subscription_id
				AND s.current_download_intent_id=locked_parents.intent_id
				AND s.completed_at IS NULL AND s.internal_state='organizing'
			RETURNING s.id
		), batches AS (
			UPDATE organization_batches AS b
			SET state='failed', last_error='lease expired', next_attempt_at=$1,
				lease_token=NULL, lease_expires_at=NULL, updated_at=$1
			FROM candidates
			CROSS JOIN parent_gate
			WHERE b.id=candidates.id
			RETURNING b.id
		)
		SELECT count(*) FROM batches`, now, limit).Scan(&recovered)
	if err != nil {
		return 0, organizationPersistenceError(ctx)
	}
	return recovered, nil
}

func rollbackOrganizationSubscription(
	ctx context.Context,
	tx pgx.Tx,
	subscriptionID int64,
	intentID uuid.UUID,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE subscription_episodes
		SET state='pending_organization', updated_at=now()
		WHERE subscription_id=$1 AND download_intent_id=$2
			AND state='organizing'`, subscriptionID, intentID); err != nil {
		return organizationPersistenceError(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE subscriptions
		SET internal_state='pending_organization', v2_state='R', updated_at=now()
		WHERE id=$1 AND current_download_intent_id=$2
			AND completed_at IS NULL AND internal_state='organizing'`, subscriptionID, intentID); err != nil {
		return organizationPersistenceError(ctx)
	}
	return nil
}

func lockOrganizationSubscriptionByIntent(
	ctx context.Context,
	tx pgx.Tx,
	intentID uuid.UUID,
) (int64, bool, error) {
	var subscriptionID int64
	err := tx.QueryRow(ctx, `
		SELECT s.id
		FROM subscriptions AS s
		JOIN download_intents AS di
			ON di.subscription_id=s.id AND di.username=s.username
		WHERE di.id=$1 AND s.completed_at IS NULL
			AND s.current_download_intent_id=di.id
			AND s.internal_state='organizing'
		FOR UPDATE OF s`, intentID).Scan(&subscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, organizationPersistenceError(ctx)
	}
	return subscriptionID, true, nil
}
