package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const recordEmptyTVSubscriptionProgressSQL = `
	UPDATE subscriptions
	SET config=config || $6::jsonb, updated_at=now()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL
		AND current_download_intent_id IS NULL
		AND jsonb_typeof(config)='object'
		AND internal_state=$3 AND v2_state=$4
		AND updated_at=$5
		AND LOWER(BTRIM(media_type)) IN ($7, $8)
		AND NOT CASE jsonb_typeof(config->'best_version')
			WHEN 'boolean' THEN (config->>'best_version')::boolean
			WHEN 'number' THEN
				(config->>'best_version')::numeric = TRUNC((config->>'best_version')::numeric)
				AND (config->>'best_version')::numeric <> 0
			ELSE false
		END
	RETURNING id`

// RecordEmptyTVSubscriptionProgress records V2's empty missing-container
// progress for an ordinary P subscription without completing identification.
func (s *Store) RecordEmptyTVSubscriptionProgress(
	ctx context.Context,
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) error {
	query, args, err := buildRecordEmptyTVSubscriptionProgress(username, id, expectedState, expectedUpdatedAt)
	if err != nil {
		return err
	}
	if ctx == nil {
		return ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !subscriptionPreciseSearchStoreReady(s) {
		return ErrSubscriptionPreciseSearchPersistence
	}

	var recordedID int64
	err = s.db.Pool.QueryRow(ctx, query, args...).Scan(&recordedID)
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ErrSubscriptionPreciseSearchPersistence
	}
	var current subscription.State
	err = s.db.Pool.QueryRow(ctx, getActiveSubscriptionStateSQL, id, strings.TrimSpace(username)).Scan(&current)
	if err == nil {
		return ErrSubscriptionPreciseSearchStateConflict
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubscriptionNotFound
	}
	return ErrSubscriptionPreciseSearchPersistence
}

func buildRecordEmptyTVSubscriptionProgress(
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || id <= 0 || expectedUpdatedAt.IsZero() || expectedState != subscription.PendingIdentification {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	return recordEmptyTVSubscriptionProgressSQL, []any{
		id, username, expectedState, "P", expectedUpdatedAt.UTC(), `{"lack_episode":0}`,
		strings.ToLower(string(media.TypeTV)), "tv",
	}, nil
}
