package database

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const recordRecoveredTVSubscriptionProgressSQL = `
	UPDATE subscriptions
	SET config=config || $6::jsonb, internal_state=$7, v2_state=$8,
		completed_at=CASE WHEN $7='completed' THEN now() ELSE completed_at END,
		updated_at=now()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL
		AND current_download_intent_id IS NULL
		AND jsonb_typeof(config)='object'
		AND internal_state=$3 AND v2_state=$4 AND updated_at=$5
		AND LOWER(BTRIM(media_type)) IN ($9, $10)
		AND CASE jsonb_typeof(config->'total_episode')
			WHEN 'number' THEN (config->>'total_episode')::numeric=0
			ELSE false
		END
		AND NOT CASE jsonb_typeof(config->'best_version')
			WHEN 'boolean' THEN (config->>'best_version')::boolean
			WHEN 'number' THEN
				(config->>'best_version')::numeric=TRUNC((config->>'best_version')::numeric)
				AND (config->>'best_version')::numeric<>0
			ELSE false
		END
	RETURNING id`

// RecordRecoveredTVSubscriptionProgress persists the outer loop's lefts,
// independently of the subscription's unchanged zero configured total.
func (s *Store) RecordRecoveredTVSubscriptionProgress(
	ctx context.Context, username string, id int64, expectedState subscription.State,
	expectedUpdatedAt time.Time, lack int,
) error {
	query, args, err := buildRecordRecoveredTVSubscriptionProgress(username, id, expectedState, expectedUpdatedAt, lack)
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

func buildRecordRecoveredTVSubscriptionProgress(
	username string, id int64, expectedState subscription.State, expectedUpdatedAt time.Time, lack int,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || id <= 0 || expectedUpdatedAt.IsZero() || lack < 0 || lack > maxSubscriptionEpisodes ||
		(expectedState != subscription.PendingSearch && expectedState != subscription.Active && expectedState != subscription.PendingIdentification) {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	next := expectedState
	if expectedState != subscription.PendingIdentification {
		if lack == 0 {
			next = subscription.Completed
		} else if expectedState == subscription.PendingSearch {
			next = subscription.Active
		}
	}
	projected, _ := subscription.V2State(expectedState)
	nextProjected, _ := subscription.V2State(next)
	patch, _ := json.Marshal(map[string]any{"lack_episode": lack})
	return recordRecoveredTVSubscriptionProgressSQL, []any{
		id, username, expectedState, projected, expectedUpdatedAt.UTC(), patch, next, nextProjected,
		strings.ToLower(string(media.TypeTV)), "tv",
	}, nil
}
