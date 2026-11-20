package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

var (
	ErrInvalidSubscriptionTarget     = errors.New("订阅目标参数无效")
	ErrSubscriptionTargetConflict    = errors.New("订阅目标快照冲突")
	ErrSubscriptionTargetNotFound    = errors.New("订阅目标不存在")
	ErrSubscriptionTargetPersistence = errors.New("订阅目标持久化失败")
)

const reconcileSubscriptionTargetSQL = `
	WITH target AS MATERIALIZED (
		SELECT id, season, config, updated_at,
			jsonb_typeof(config)='object' AS config_object
		FROM subscriptions
		WHERE id=$1 AND username=$2 AND completed_at IS NULL AND updated_at=$3
		FOR UPDATE
	), requested AS MATERIALIZED (
		SELECT requested.season, requested.episode
		FROM unnest($4::integer[], $5::integer[]) AS requested(season, episode)
	), locked_episodes AS MATERIALIZED (
		SELECT se.id, se.season, se.episode, se.download_intent_id
		FROM subscription_episodes AS se
		JOIN target ON target.id=se.subscription_id
		ORDER BY se.id
		FOR UPDATE OF se
	), retained_coordinates AS MATERIALIZED (
		SELECT season, episode FROM locked_episodes WHERE download_intent_id IS NOT NULL
		UNION
		SELECT season, episode FROM requested
	), capacity AS MATERIALIZED (
		SELECT count(*) <= $8 AS allowed FROM retained_coordinates
	), changes AS MATERIALIZED (
		SELECT target.id,
			target.season IS DISTINCT FROM $6 OR
			target.config IS DISTINCT FROM target.config || $7::jsonb OR
			EXISTS (
				SELECT 1 FROM locked_episodes
				WHERE download_intent_id IS NULL AND NOT EXISTS (
					SELECT 1 FROM requested
					WHERE requested.season=locked_episodes.season
						AND requested.episode=locked_episodes.episode
				)
			) OR EXISTS (
				SELECT 1 FROM requested WHERE NOT EXISTS (
					SELECT 1 FROM locked_episodes
					WHERE locked_episodes.season=requested.season
						AND locked_episodes.episode=requested.episode
				)
			) AS required
		FROM target
	), patched AS MATERIALIZED (
		UPDATE subscriptions AS s
		SET season=$6, config=s.config || $7::jsonb, updated_at=clock_timestamp()
		FROM target, capacity, changes
		WHERE s.id=target.id AND target.config_object AND capacity.allowed AND changes.required
		RETURNING s.updated_at
	), authorized AS MATERIALIZED (
		SELECT target.id, COALESCE(patched.updated_at, target.updated_at) AS updated_at
		FROM target
		CROSS JOIN capacity
		LEFT JOIN patched ON TRUE
		WHERE target.config_object AND capacity.allowed
	), removed AS (
		DELETE FROM subscription_episodes AS se
		USING authorized
		WHERE se.id IN (
			SELECT id FROM locked_episodes WHERE download_intent_id IS NULL
		)
			AND NOT EXISTS (
				SELECT 1 FROM requested
				WHERE requested.season=se.season AND requested.episode=se.episode
			)
		RETURNING se.id
	), inserted AS (
		INSERT INTO subscription_episodes (subscription_id, season, episode, state)
		SELECT target.id, requested.season, requested.episode, 'missing'
		FROM target
		CROSS JOIN authorized
		CROSS JOIN requested
		ON CONFLICT (subscription_id, season, episode) DO NOTHING
		RETURNING id
	)
	SELECT (SELECT updated_at FROM authorized), capacity.allowed, target.config_object,
		(SELECT count(*) FROM inserted), (SELECT count(*) FROM removed)
	FROM target
	CROSS JOIN capacity`

const listSubscriptionEpisodesForTargetSQL = `
	WITH target AS MATERIALIZED (
		SELECT id
		FROM subscriptions
		WHERE id=$1 AND username=$2 AND completed_at IS NULL
	), requested AS MATERIALIZED (
		SELECT requested.season, requested.episode
		FROM unnest($3::integer[], $4::integer[]) AS requested(season, episode)
	)
	SELECT se.id, se.subscription_id, se.season, se.episode, se.state,
		se.download_intent_id, se.updated_at
	FROM subscription_episodes AS se
	JOIN target ON target.id=se.subscription_id
	JOIN requested ON requested.season=se.season AND requested.episode=se.episode
	WHERE se.state=ANY($5::text[])
	ORDER BY se.season, se.episode
	LIMIT $6`

const getActiveSubscriptionTargetVersionSQL = `
	SELECT updated_at
	FROM subscriptions
	WHERE id=$1 AND username=$2 AND completed_at IS NULL`

const verifySubscriptionTargetVersionSQL = `
	SELECT id
	FROM subscriptions
	WHERE id=$1 AND username=$2 AND completed_at IS NULL AND updated_at=$3
	FOR UPDATE`

// SubscriptionTargetReconcile atomically applies one previously observed TV
// target snapshot. Episodes already bound to any download intent are immutable
// history: coordinates, state and intent ID are never rewritten or deleted.
type SubscriptionTargetReconcile struct {
	Username          string
	SubscriptionID    int64
	ExpectedUpdatedAt time.Time
	Target            subscription.TVTargetResolution
}

// ReconcileSubscriptionTarget applies config and episode membership under the
// subscription row lock. It returns the next observation version.
func (s *Store) ReconcileSubscriptionTarget(
	ctx context.Context,
	input SubscriptionTargetReconcile,
) (time.Time, error) {
	_, _, err := buildReconcileSubscriptionTarget(input)
	if err != nil {
		return time.Time{}, err
	}
	if ctx == nil || !subscriptionEpisodeStoreReady(s) {
		return time.Time{}, ErrSubscriptionTargetPersistence
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return time.Time{}, subscriptionTargetPersistenceError(ctx)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	next, err := reconcileSubscriptionTargetTx(ctx, tx, input)
	if err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, subscriptionTargetPersistenceError(ctx)
	}
	return next, nil
}

func reconcileSubscriptionTargetTx(
	ctx context.Context,
	tx pgx.Tx,
	input SubscriptionTargetReconcile,
) (time.Time, error) {
	query, args, err := buildReconcileSubscriptionTarget(input)
	if err != nil {
		return time.Time{}, err
	}
	var next *time.Time
	var capacityOK, configObject bool
	var inserted, removed int64
	err = tx.QueryRow(ctx, query, args...).Scan(&next, &capacityOK, &configObject, &inserted, &removed)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, classifySubscriptionTargetMiss(ctx, tx, input)
	}
	if err != nil {
		return time.Time{}, subscriptionTargetPersistenceError(ctx)
	}
	if !configObject {
		return time.Time{}, ErrSubscriptionTargetPersistence
	}
	if !capacityOK {
		return time.Time{}, ErrInvalidSubscriptionTarget
	}
	if next == nil {
		return time.Time{}, ErrSubscriptionTargetPersistence
	}
	return next.UTC(), nil
}

// VerifySubscriptionTargetVersionTx locks one active subscription only when
// the candidate was prepared from its current target snapshot.
func (s *Store) VerifySubscriptionTargetVersionTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	expectedUpdatedAt time.Time,
) error {
	username = strings.TrimSpace(username)
	if ctx == nil || tx == nil || username == "" || subscriptionID <= 0 || expectedUpdatedAt.IsZero() {
		return ErrInvalidSubscriptionTarget
	}
	var id int64
	err := tx.QueryRow(ctx, verifySubscriptionTargetVersionSQL,
		subscriptionID, username, expectedUpdatedAt.UTC()).Scan(&id)
	if err == nil && id == subscriptionID {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubscriptionTargetConflict
	}
	return ErrSubscriptionTargetPersistence
}

// ListSubscriptionEpisodesForTarget returns only rows in the caller's current
// target snapshot. The version post-check makes the returned rows linearizable
// with target reconciliation, including an explicitly empty target.
func (s *Store) ListSubscriptionEpisodesForTarget(
	ctx context.Context,
	username string,
	subscriptionID int64,
	expectedUpdatedAt time.Time,
	targetEpisodes []subscription.Episode,
	states []subscription.SubscriptionEpisodeState,
	limit int,
) ([]subscription.SubscriptionEpisode, error) {
	seasons, episodes, err := normalizeSubscriptionTargetCoordinates(targetEpisodes)
	if err != nil || strings.TrimSpace(username) == "" || subscriptionID <= 0 || expectedUpdatedAt.IsZero() ||
		len(states) == 0 || len(states) > 7 || limit < 1 || limit > maxSubscriptionEpisodes {
		return nil, ErrInvalidSubscriptionTarget
	}
	normalizedStates := make([]string, 0, len(states))
	seenStates := make(map[subscription.SubscriptionEpisodeState]struct{}, len(states))
	for _, state := range states {
		if !validSubscriptionEpisodeState(state) {
			return nil, ErrInvalidSubscriptionTarget
		}
		if _, exists := seenStates[state]; exists {
			continue
		}
		seenStates[state] = struct{}{}
		normalizedStates = append(normalizedStates, string(state))
	}
	if ctx == nil || !subscriptionEpisodeStoreReady(s) {
		return nil, ErrSubscriptionTargetPersistence
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.Pool.Query(ctx, listSubscriptionEpisodesForTargetSQL,
		subscriptionID, strings.TrimSpace(username), seasons, episodes, normalizedStates, limit)
	if err != nil {
		return nil, subscriptionTargetPersistenceError(ctx)
	}
	defer rows.Close()
	items := make([]subscription.SubscriptionEpisode, 0)
	for rows.Next() {
		item, scanErr := scanSubscriptionEpisode(rows)
		if scanErr != nil {
			return nil, subscriptionTargetPersistenceError(ctx)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, subscriptionTargetPersistenceError(ctx)
	}
	var current time.Time
	err = s.db.Pool.QueryRow(ctx, getActiveSubscriptionTargetVersionSQL,
		subscriptionID, strings.TrimSpace(username)).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubscriptionTargetNotFound
	}
	if err != nil {
		return nil, subscriptionTargetPersistenceError(ctx)
	}
	if !current.Equal(expectedUpdatedAt) {
		return nil, ErrSubscriptionTargetConflict
	}
	return items, nil
}

func buildReconcileSubscriptionTarget(input SubscriptionTargetReconcile) (string, []any, error) {
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" || input.SubscriptionID <= 0 || input.ExpectedUpdatedAt.IsZero() {
		return "", nil, ErrInvalidSubscriptionTarget
	}
	canonical, err := subscription.DescribeTVTarget(input.Target.Snapshot)
	if err != nil || canonical.Knowledge != input.Target.Knowledge ||
		!reflect.DeepEqual(canonical.TargetEpisodes, input.Target.TargetEpisodes) {
		return "", nil, ErrInvalidSubscriptionTarget
	}
	patch := map[string]any{
		"total_episode":        input.Target.Snapshot.TotalEpisode,
		"start_episode":        input.Target.Snapshot.StartEpisode,
		"manual_total_episode": input.Target.Snapshot.ManualTotalEpisode,
		"lack_episode":         input.Target.Snapshot.LackEpisode,
		"note":                 input.Target.Snapshot.Note,
		"current_priority":     input.Target.Snapshot.CurrentPriority,
		"episode_priority":     input.Target.Snapshot.EpisodePriority,
	}
	encoded, err := json.Marshal(patch)
	if err != nil {
		return "", nil, ErrInvalidSubscriptionTarget
	}
	seasons, episodes, err := normalizeSubscriptionTargetCoordinates(input.Target.TargetEpisodes)
	if err != nil {
		return "", nil, err
	}
	return reconcileSubscriptionTargetSQL, []any{
		input.SubscriptionID, input.Username, input.ExpectedUpdatedAt.UTC(),
		seasons, episodes, input.Target.Snapshot.Season, encoded, maxSubscriptionEpisodes,
	}, nil
}

func normalizeSubscriptionTargetCoordinates(items []subscription.Episode) ([]int32, []int32, error) {
	if len(items) > maxSubscriptionEpisodes {
		return nil, nil, ErrInvalidSubscriptionTarget
	}
	unique := make(map[subscription.Episode]struct{}, len(items))
	for _, item := range items {
		if item.Season < 0 || item.Season > maxSubscriptionEpisodeSeason ||
			item.Episode < 1 || item.Episode > maxSubscriptionEpisodeNumber {
			return nil, nil, ErrInvalidSubscriptionTarget
		}
		unique[item] = struct{}{}
	}
	normalized := make([]subscription.Episode, 0, len(unique))
	for item := range unique {
		normalized = append(normalized, item)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Season == normalized[j].Season {
			return normalized[i].Episode < normalized[j].Episode
		}
		return normalized[i].Season < normalized[j].Season
	})
	seasons := make([]int32, 0, len(normalized))
	episodes := make([]int32, 0, len(normalized))
	for _, item := range normalized {
		seasons = append(seasons, int32(item.Season))
		episodes = append(episodes, int32(item.Episode))
	}
	return seasons, episodes, nil
}

func classifySubscriptionTargetMiss(
	ctx context.Context,
	tx pgx.Tx,
	input SubscriptionTargetReconcile,
) error {
	var current time.Time
	err := tx.QueryRow(ctx, getActiveSubscriptionTargetVersionSQL,
		input.SubscriptionID, strings.TrimSpace(input.Username)).Scan(&current)
	if err == nil {
		return ErrSubscriptionTargetConflict
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubscriptionTargetNotFound
	}
	return ErrSubscriptionTargetPersistence
}

func subscriptionTargetPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrSubscriptionTargetPersistence
}
