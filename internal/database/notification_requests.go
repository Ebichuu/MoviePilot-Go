package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
)

const (
	maxNotificationLease = time.Hour
	minNotificationLease = time.Second
)

// ClaimNotification atomically leases one due channel delivery.
func (s *Store) ClaimNotification(
	ctx context.Context,
	requestID uuid.UUID,
	now time.Time,
	leaseDuration time.Duration,
) (notificationflow.Lease, bool, error) {
	if requestID == uuid.Nil || now.IsZero() || leaseDuration < minNotificationLease || leaseDuration > maxNotificationLease {
		return notificationflow.Lease{}, false, notificationflow.ErrInvalidPlan
	}
	if err := validateNotificationStore(s, ctx); err != nil {
		return notificationflow.Lease{}, false, err
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return notificationflow.Lease{}, false, notificationPersistenceError(ctx)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	token := uuid.New()
	var lease notificationflow.Lease
	var payload []byte
	var batchID *uuid.UUID
	var eventID *int64
	var eventBatchID *uuid.UUID
	var expiresAt *time.Time
	err = tx.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id
			FROM notifications
			WHERE id=$1
			  AND attempt_count < $2
			  AND (
				(state IN ('pending', 'failed') AND next_attempt_at <= $3)
				OR (state='working' AND lease_expires_at <= $3)
			  )
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE notifications AS n
			SET state='working', attempt_count=n.attempt_count+1, last_error='',
				lease_token=$4, lease_expires_at=$5, updated_at=$3
			FROM candidate
			WHERE n.id=candidate.id
			RETURNING n.id, n.channel, n.source, n.payload, n.state, n.attempt_count,
				n.lease_token, n.lease_expires_at, n.batch_id, n.event_id
		)
		SELECT c.id, c.channel, c.source, c.payload, c.state, c.attempt_count,
			c.lease_token, c.lease_expires_at, c.batch_id, c.event_id, e.batch_id
		FROM claimed AS c
		LEFT JOIN notification_events AS e ON e.id=c.event_id`,
		requestID, notificationflow.MaxAttempts, now.UTC(), token, now.UTC().Add(leaseDuration)).Scan(
		&lease.Request.ID,
		&lease.Request.Channel,
		&lease.Request.Source,
		&payload,
		&lease.Request.State,
		&lease.Request.AttemptCount,
		&lease.Token,
		&expiresAt,
		&batchID,
		&eventID,
		&eventBatchID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return notificationflow.Lease{}, false, nil
	}
	eventLinkValid := false
	if eventID == nil {
		eventLinkValid = batchID != nil && *batchID != uuid.Nil
	} else if *eventID > 0 {
		organizationEvent := batchID != nil && *batchID != uuid.Nil && eventBatchID != nil && *eventBatchID == *batchID
		businessEvent := batchID == nil && eventBatchID == nil
		eventLinkValid = organizationEvent || businessEvent
	}
	if err != nil || (batchID != nil && *batchID == uuid.Nil) || expiresAt == nil || !eventLinkValid ||
		json.Unmarshal(payload, &lease.Request.Message) != nil ||
		!lease.Valid(requestID) {
		return notificationflow.Lease{}, false, notificationPersistenceError(ctx)
	}
	if err := tx.Commit(ctx); err != nil {
		return notificationflow.Lease{}, false, notificationPersistenceError(ctx)
	}
	return lease, true, nil
}

// CompleteNotification marks one leased channel delivery sent.
func (s *Store) CompleteNotification(ctx context.Context, requestID, token uuid.UUID, sentAt time.Time) error {
	if requestID == uuid.Nil || token == uuid.Nil || sentAt.IsZero() {
		return notificationflow.ErrInvalidPlan
	}
	if err := validateNotificationStore(s, ctx); err != nil {
		return err
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE notifications
		SET state='sent', sent_at=COALESCE(sent_at, $3), last_error='',
			lease_token=NULL, lease_expires_at=NULL, updated_at=$3
		WHERE id=$1 AND state='working' AND lease_token=$2
		  AND lease_expires_at > now()`, requestID, token, sentAt.UTC())
	if err != nil || result.RowsAffected() != 1 {
		return notificationPersistenceError(ctx)
	}
	return nil
}

// RescheduleNotification persists a bounded retry or the terminal failed state.
func (s *Store) RescheduleNotification(
	ctx context.Context,
	requestID, token uuid.UUID,
	retryAt time.Time,
	stableErr string,
) error {
	stableErr = normalizeSideEffectError(stableErr)
	if requestID == uuid.Nil || token == uuid.Nil || retryAt.IsZero() || stableErr == "" {
		return notificationflow.ErrInvalidPlan
	}
	if err := validateNotificationStore(s, ctx); err != nil {
		return err
	}
	result, err := s.db.Pool.Exec(ctx, `
		UPDATE notifications
		SET state='failed', next_attempt_at=$3, last_error=$5,
			lease_token=NULL, lease_expires_at=NULL, updated_at=now()
		WHERE id=$1 AND state='working' AND lease_token=$2
		  AND lease_expires_at > now()
		  AND attempt_count <= $4`, requestID, token, retryAt.UTC(), notificationflow.MaxAttempts, stableErr)
	if err != nil || result.RowsAffected() != 1 {
		return notificationPersistenceError(ctx)
	}
	return nil
}

// RecoverExpiredNotifications releases a bounded page of expired leases.
func (s *Store) RecoverExpiredNotifications(ctx context.Context, now time.Time, limit int) (int64, error) {
	if now.IsZero() || limit < 1 || limit > notificationflow.SweepPageSize {
		return 0, notificationflow.ErrInvalidPlan
	}
	if err := validateNotificationStore(s, ctx); err != nil {
		return 0, err
	}
	result, err := s.db.Pool.Exec(ctx, `
		WITH candidates AS (
			SELECT id FROM notifications
			WHERE state='working' AND lease_expires_at <= $1
			ORDER BY lease_expires_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE notifications AS n
		SET state='failed', next_attempt_at=$1, last_error='lease_expired',
			lease_token=NULL, lease_expires_at=NULL, updated_at=$1
		FROM candidates WHERE n.id=candidates.id`, now.UTC(), limit)
	if err != nil {
		return 0, notificationPersistenceError(ctx)
	}
	return result.RowsAffected(), nil
}

// ListDueNotifications returns one bounded page of retryable request IDs.
func (s *Store) ListDueNotifications(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	if now.IsZero() || limit < 1 || limit > notificationflow.SweepPageSize {
		return nil, notificationflow.ErrInvalidPlan
	}
	if err := validateNotificationStore(s, ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, `
		SELECT id FROM notifications
		WHERE state IN ('pending', 'failed') AND attempt_count < $3
		  AND next_attempt_at <= $1
		ORDER BY next_attempt_at, id
		LIMIT $2`, now.UTC(), limit, notificationflow.MaxAttempts)
	if err != nil {
		return nil, notificationPersistenceError(ctx)
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil || id == uuid.Nil {
			return nil, notificationPersistenceError(ctx)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, notificationPersistenceError(ctx)
	}
	return ids, nil
}

func validateNotificationStore(s *Store, ctx context.Context) error {
	if ctx == nil || s == nil || s.db == nil || s.db.Pool == nil {
		return notificationflow.ErrPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func notificationPersistenceError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return notificationflow.ErrPersistence
}
