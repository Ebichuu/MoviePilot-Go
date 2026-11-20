package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	maxRSSSubscriptionTargets = 100
)

const subscriptionRSSBaseSQL = `
		SELECT ` + preciseSearchSubscriptionColumns + `
		FROM subscriptions
		WHERE (
			(internal_state=$1 AND v2_state='P')
			OR (internal_state=ANY($2::text[]) AND v2_state='R')
		  )
		  AND completed_at IS NULL
		  AND media_type IS NOT NULL
		  AND current_download_intent_id IS NULL
		  AND updated_at <= $3`

// ListSubscriptionsForRSS returns a bounded, keyset-paginated snapshot of V2
// R and P targets that do not already own a download intent. V2 expands an RSS
// search for R to R,P: pending_search/N is excluded, while
// pending-identification/P remains searchable but cannot be completed.
//
// The method intentionally does not inspect the sites JSON configuration. RSS
// matching filters the site allow-list in memory after decoding the target;
// this keeps the SQL independent from legacy V2 config shapes.
func (s *Store) ListSubscriptionsForRSS(
	ctx context.Context,
	cutoff time.Time,
	afterUpdatedAt time.Time,
	afterID int64,
	limit int,
) ([]Subscription, error) {
	if ctx == nil {
		return nil, ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.db.Pool == nil || cutoff.IsZero() || limit < 1 || limit > maxRSSSubscriptionTargets {
		return nil, ErrInvalidSubscriptionPreciseSearch
	}
	zeroCursor := afterUpdatedAt.IsZero() && afterID == 0
	completeCursor := !afterUpdatedAt.IsZero() && afterID > 0
	if !zeroCursor && !completeCursor {
		return nil, ErrInvalidSubscriptionPreciseSearch
	}

	args := []any{
		subscription.PendingIdentification,
		[]string{
			string(subscription.Active),
			string(subscription.Acquired),
			string(subscription.Downloading),
			string(subscription.PendingOrganization),
			string(subscription.Organizing),
			string(subscription.PendingIndex),
			string(subscription.Indexed),
		},
		cutoff.UTC(),
	}
	query := subscriptionRSSBaseSQL
	if zeroCursor {
		query += ` ORDER BY updated_at, id LIMIT $4`
		args = append(args, limit)
	} else {
		query += ` AND (updated_at, id) > ($4, $5) ORDER BY updated_at, id LIMIT $6`
		args = append(args, afterUpdatedAt.UTC(), afterID, limit)
	}
	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, subscriptionPreciseSearchPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]Subscription, 0, limit)
	for rows.Next() {
		item, scanErr := scanSubscription(rows)
		if scanErr != nil {
			return nil, subscriptionPreciseSearchPersistenceError(ctx)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, subscriptionPreciseSearchPersistenceError(ctx)
	}
	return items, nil
}

// CompareAndSwapSubscriptionRSSStateTx atomically claims the scanned RSS
// target. Active/R moves to acquired/R; pending-identification/P stays P so it
// remains searchable but cannot cross V2's completion guard.
func (s *Store) CompareAndSwapSubscriptionRSSStateTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	expected subscription.State,
) error {
	username = strings.TrimSpace(username)
	if ctx == nil || tx == nil || s == nil || subscriptionID <= 0 || username == "" ||
		(expected != subscription.Active && expected != subscription.PendingIdentification) {
		return ErrInvalidSubscriptionPreciseSearch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := expected
	if expected == subscription.Active {
		var err error
		next, err = subscription.Transition(expected, subscription.Acquired)
		if err != nil {
			return ErrInvalidSubscriptionPreciseSearch
		}
	}
	projected, err := subscription.V2State(next)
	if err != nil {
		return ErrInvalidSubscriptionPreciseSearch
	}
	var id int64
	err = tx.QueryRow(ctx, `
		UPDATE subscriptions
		SET internal_state=$3, v2_state=$4, updated_at=now()
		WHERE id=$1 AND username=$2 AND completed_at IS NULL
		  AND internal_state=$5 AND v2_state=$4
		RETURNING id`, subscriptionID, username, next, projected, expected).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return subscriptionPreciseSearchPersistenceError(ctx)
	}
	var current subscription.State
	if err := tx.QueryRow(ctx, `
		SELECT internal_state FROM subscriptions
		WHERE id=$1 AND username=$2 AND completed_at IS NULL`, subscriptionID, username).Scan(&current); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return ErrSubscriptionPreciseSearchPersistence
	}
	return ErrSubscriptionPreciseSearchStateConflict
}
