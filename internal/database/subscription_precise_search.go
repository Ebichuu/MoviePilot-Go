package database

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

var (
	// ErrInvalidSubscriptionPreciseSearch 表示精准搜索持久化参数无效。
	ErrInvalidSubscriptionPreciseSearch = errors.New("订阅精准搜索参数无效")
	// ErrSubscriptionPreciseSearchStateConflict 表示订阅已不再等待本次精准搜索结果。
	ErrSubscriptionPreciseSearchStateConflict = errors.New("订阅精准搜索状态冲突")
	// ErrSubscriptionPreciseSearchPersistence 表示数据库暂时无法安全完成精准搜索持久化。
	ErrSubscriptionPreciseSearchPersistence = errors.New("订阅精准搜索持久化失败")
	// ErrInvalidDownloadIntentSubscriptionBinding 表示下载意图与订阅绑定参数无效。
	ErrInvalidDownloadIntentSubscriptionBinding = errors.New("下载意图订阅绑定参数无效")
	// ErrDownloadIntentSubscriptionBindingNotFound 对不存在、已完成或不属于当前用户的绑定目标统一返回。
	ErrDownloadIntentSubscriptionBindingNotFound = errors.New("下载意图订阅绑定目标不存在")
	// ErrDownloadIntentSubscriptionBindingConflict 表示下载意图已经绑定到另一个订阅。
	ErrDownloadIntentSubscriptionBindingConflict = errors.New("下载意图订阅绑定冲突")
	// ErrDownloadIntentSubscriptionBindingPersistence 表示数据库暂时无法安全完成绑定。
	ErrDownloadIntentSubscriptionBindingPersistence = errors.New("下载意图订阅绑定失败")
)

const maxPreciseSearchSubscriptions = 100

const preciseSearchSubscriptionColumns = `id, username, title, media_type, year, season, media_source, media_id, internal_state, v2_state, config, completed_at, created_at, updated_at`

const listSubscriptionsForPreciseSearchSQL = `
	SELECT ` + preciseSearchSubscriptionColumns + `
	FROM subscriptions
	WHERE internal_state='pending_search' AND completed_at IS NULL
		AND media_type IS NOT NULL AND current_download_intent_id IS NULL
		AND updated_at <= $1
	ORDER BY updated_at, id
	LIMIT $2`

const listSubscriptionsForPreciseSearchAfterSQL = `
	SELECT ` + preciseSearchSubscriptionColumns + `
	FROM subscriptions
	WHERE internal_state='pending_search' AND completed_at IS NULL
		AND media_type IS NOT NULL AND current_download_intent_id IS NULL
		AND updated_at <= $1
		AND (updated_at, id) > ($2, $3)
	ORDER BY updated_at, id
	LIMIT $4`

const getActiveSubscriptionSQL = `
	SELECT ` + preciseSearchSubscriptionColumns + `
	FROM subscriptions
	WHERE id=$1 AND username=$2 AND completed_at IS NULL`

const getOwnedSubscriptionSQL = `
	SELECT ` + preciseSearchSubscriptionColumns + `
	FROM subscriptions
	WHERE id=$1 AND username=$2`

const completeSatisfiedBestVersionTVSubscriptionSQL = `
	UPDATE subscriptions
	SET internal_state=$3, v2_state=$4, completed_at=now(),
		config=config || $7::jsonb, updated_at=now()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL
		AND current_download_intent_id IS NULL
		AND jsonb_typeof(config)='object'
		AND internal_state=$5 AND v2_state=$12
		AND v2_state IN ('N', 'R')
		AND updated_at=$6
		AND LOWER(BTRIM(media_type)) IN ($8, $9)
		AND config=$10::jsonb AND season IS NOT DISTINCT FROM $11
		AND CASE jsonb_typeof(config->'best_version')
			WHEN 'boolean' THEN (config->>'best_version')::boolean
			WHEN 'number' THEN
				(config->>'best_version')::numeric = TRUNC((config->>'best_version')::numeric)
				AND (config->>'best_version')::numeric <> 0
			ELSE false
		END
	RETURNING id`

const compareAndSwapSubscriptionPreciseSearchStateSQL = `
	UPDATE subscriptions
	SET internal_state=$3, v2_state=$4, updated_at=now()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL AND internal_state=$5
	RETURNING id`

const compareAndSwapSubscriptionPreciseSearchStateAtSQL = `
	UPDATE subscriptions
	SET internal_state=$3, v2_state=$4, updated_at=now()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL AND internal_state=$5 AND updated_at=$6
	RETURNING id`

const completeSatisfiedSubscriptionSQL = `
	UPDATE subscriptions
	SET internal_state=$3, v2_state=$4, completed_at=now(),
		config=config || $7::jsonb, updated_at=now()
	WHERE id=$1 AND username=$2 AND completed_at IS NULL
		AND current_download_intent_id IS NULL
		AND jsonb_typeof(config)='object'
		AND internal_state=$5
		AND updated_at=$6
		AND LOWER(BTRIM(media_type)) IN ($8, $9)
		AND NOT CASE jsonb_typeof(config->'best_version')
			WHEN 'boolean' THEN (config->>'best_version')::boolean
			WHEN 'number' THEN
				(config->>'best_version')::numeric = TRUNC((config->>'best_version')::numeric)
				AND (config->>'best_version')::numeric <> 0
			ELSE false
		END
	RETURNING id`

const getActiveSubscriptionStateSQL = `
	SELECT internal_state
	FROM subscriptions
	WHERE id=$1 AND username=$2 AND completed_at IS NULL`

const bindDownloadIntentSubscriptionSQL = `
	WITH target AS MATERIALIZED (
		SELECT di.id AS intent_id, s.id AS subscription_id,
			di.subscription_id AS previous_subscription_id,
			s.current_download_intent_id AS previous_current_intent_id
		FROM download_intents AS di
		JOIN subscriptions AS s ON s.id=$3 AND s.username=$2 AND s.completed_at IS NULL
		WHERE di.id=$1 AND di.username=$2
			AND (di.subscription_id IS NULL OR di.subscription_id=s.id)
			AND (s.current_download_intent_id IS NULL OR s.current_download_intent_id=di.id)
		FOR UPDATE OF di, s
	), bound_subscription AS (
		UPDATE subscriptions AS s
		SET current_download_intent_id=target.intent_id,
			updated_at=CASE
				WHEN target.previous_current_intent_id IS NULL THEN now()
				ELSE s.updated_at
			END
		FROM target
		WHERE s.id=target.subscription_id
		RETURNING s.id
	)
	UPDATE download_intents AS di
	SET subscription_id=target.subscription_id,
		updated_at=CASE
			WHEN target.previous_subscription_id IS NULL THEN now()
			ELSE di.updated_at
		END
	FROM target, bound_subscription
	WHERE di.id=target.intent_id AND bound_subscription.id=target.subscription_id
	RETURNING di.subscription_id`

const getDownloadIntentSubscriptionBindingSQL = `
	SELECT di.subscription_id,
		EXISTS (
			SELECT 1
			FROM subscriptions AS s
			WHERE s.id=$3 AND s.username=$2 AND s.completed_at IS NULL
		),
		(
			SELECT s.current_download_intent_id
			FROM subscriptions AS s
			WHERE s.id=$3 AND s.username=$2 AND s.completed_at IS NULL
		)
	FROM download_intents AS di
	WHERE di.id=$1 AND di.username=$2`

// ListSubscriptionsForPreciseSearch 返回固定截止时间前仍等待精准搜索的有界批次。
func (s *Store) ListSubscriptionsForPreciseSearch(
	ctx context.Context,
	cutoff time.Time,
	afterUpdatedAt time.Time,
	afterID int64,
	limit int,
) ([]Subscription, error) {
	query, args, err := buildListSubscriptionsForPreciseSearch(cutoff, afterUpdatedAt, afterID, limit)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !subscriptionPreciseSearchStoreReady(s) {
		return nil, ErrSubscriptionPreciseSearchPersistence
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

// GetActiveSubscription 按 ID 和 owner 读取尚未进入完成历史的订阅。
func (s *Store) GetActiveSubscription(
	ctx context.Context,
	username string,
	id int64,
) (Subscription, error) {
	query, args, err := buildGetActiveSubscription(username, id)
	if err != nil {
		return Subscription{}, err
	}
	if ctx == nil {
		return Subscription{}, ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return Subscription{}, err
	}
	if !subscriptionPreciseSearchStoreReady(s) {
		return Subscription{}, ErrSubscriptionPreciseSearchPersistence
	}
	return getActiveSubscription(ctx, s.db.Pool, query, args)
}

// GetOwnedSubscription includes completed history so callers can distinguish
// a confirmed completion from a deleted or foreign-owner target.
func (s *Store) GetOwnedSubscription(
	ctx context.Context,
	username string,
	id int64,
) (Subscription, error) {
	query, args, err := buildGetOwnedSubscription(username, id)
	if err != nil {
		return Subscription{}, err
	}
	if ctx == nil || !subscriptionPreciseSearchStoreReady(s) {
		return Subscription{}, ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return Subscription{}, err
	}
	return getActiveSubscription(ctx, s.db.Pool, query, args)
}

func getActiveSubscription(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
) (Subscription, error) {
	item, err := scanSubscription(querier.QueryRow(ctx, query, args...))
	if err == nil {
		return item, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return Subscription{}, contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrSubscriptionNotFound
	}
	return Subscription{}, ErrSubscriptionPreciseSearchPersistence
}

// CompareAndSwapSubscriptionPreciseSearchState 原子推进 pending_search 到 active 或 acquired。
func (s *Store) CompareAndSwapSubscriptionPreciseSearchState(
	ctx context.Context,
	username string,
	id int64,
	next subscription.State,
) error {
	query, args, err := buildCompareAndSwapSubscriptionPreciseSearchState(username, id, next)
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
	return compareAndSwapSubscriptionPreciseSearchState(ctx, s.db.Pool, query, args)
}

// CompareAndSwapSubscriptionPreciseSearchStateAt also requires the target
// snapshot version used by the caller's search result.
func (s *Store) CompareAndSwapSubscriptionPreciseSearchStateAt(
	ctx context.Context,
	username string,
	id int64,
	next subscription.State,
	expectedUpdatedAt time.Time,
) error {
	query, args, err := buildCompareAndSwapSubscriptionPreciseSearchStateAt(username, id, next, expectedUpdatedAt)
	if err != nil {
		return err
	}
	if ctx == nil || !subscriptionPreciseSearchStoreReady(s) {
		return ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return compareAndSwapSubscriptionPreciseSearchState(ctx, s.db.Pool, query, args)
}

// CompleteSatisfiedTVSubscription moves a normal TV subscription directly to
// history when the caller's bounded target snapshot is already satisfied.
func (s *Store) CompleteSatisfiedTVSubscription(
	ctx context.Context,
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) error {
	query, args, err := buildCompleteSatisfiedTVSubscription(
		username,
		id,
		expectedState,
		expectedUpdatedAt,
	)
	if err != nil {
		return err
	}
	if ctx == nil || !subscriptionPreciseSearchStoreReady(s) {
		return ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var completedID int64
	err = s.db.Pool.QueryRow(ctx, query, args...).Scan(&completedID)
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		var current subscription.State
		stateErr := s.db.Pool.QueryRow(
			ctx,
			getActiveSubscriptionStateSQL,
			id,
			strings.TrimSpace(username),
		).Scan(&current)
		if stateErr == nil {
			return ErrSubscriptionPreciseSearchStateConflict
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(stateErr, pgx.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return ErrSubscriptionPreciseSearchPersistence
	}
	return ErrSubscriptionPreciseSearchPersistence
}

// CompleteSatisfiedBestVersionTVSubscription applies V2's terminal quality
// predicate. Config/season CAS protects the facts checked
// in Go even if another writer changes quality or mode without changing time.
// The ordinary media-library completion predicate remains separate.
func (s *Store) CompleteSatisfiedBestVersionTVSubscription(
	ctx context.Context,
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) error {
	if _, _, err := buildCompleteSatisfiedTVSubscription(username, id, expectedState, expectedUpdatedAt); err != nil {
		return err
	}
	if ctx == nil || !subscriptionPreciseSearchStoreReady(s) {
		return ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	observed, err := s.GetActiveSubscription(ctx, username, id)
	if err != nil {
		return err
	}
	query, args, err := prepareBestVersionTVCompletion(observed, username, id, expectedState, expectedUpdatedAt)
	if err != nil {
		return err
	}
	var completedID int64
	if err := s.db.Pool.QueryRow(ctx, query, args...).Scan(&completedID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return subscriptionPreciseSearchPersistenceError(ctx)
	}
	if _, err := s.GetActiveSubscription(ctx, username, id); err != nil {
		return err
	}
	return ErrSubscriptionPreciseSearchStateConflict
}

// CompleteSatisfiedMovieSubscription completes a movie whose bounded snapshot
// is satisfied by the ordinary library check or terminal best-version quality.
func (s *Store) CompleteSatisfiedMovieSubscription(
	ctx context.Context,
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) error {
	query, args, err := buildCompleteSatisfiedMovieSubscription(
		username,
		id,
		expectedState,
		expectedUpdatedAt,
	)
	if err != nil {
		return err
	}
	if ctx == nil || !subscriptionPreciseSearchStoreReady(s) {
		return ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var completedID int64
	err = s.db.Pool.QueryRow(ctx, query, args...).Scan(&completedID)
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, pgx.ErrNoRows) {
		var current subscription.State
		stateErr := s.db.Pool.QueryRow(
			ctx,
			getActiveSubscriptionStateSQL,
			id,
			strings.TrimSpace(username),
		).Scan(&current)
		if stateErr == nil {
			return ErrSubscriptionPreciseSearchStateConflict
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(stateErr, pgx.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return ErrSubscriptionPreciseSearchPersistence
	}
	return ErrSubscriptionPreciseSearchPersistence
}

// CompareAndSwapSubscriptionPreciseSearchStateTx 在调用者事务内推进精准搜索状态。
func (s *Store) CompareAndSwapSubscriptionPreciseSearchStateTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	id int64,
	next subscription.State,
) error {
	query, args, err := buildCompareAndSwapSubscriptionPreciseSearchState(username, id, next)
	if err != nil {
		return err
	}
	if ctx == nil || s == nil || tx == nil {
		return ErrSubscriptionPreciseSearchPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return compareAndSwapSubscriptionPreciseSearchState(ctx, tx, query, args)
}

func compareAndSwapSubscriptionPreciseSearchState(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
) error {
	var id int64
	if err := querier.QueryRow(ctx, query, args...).Scan(&id); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return subscriptionPreciseSearchPersistenceError(ctx)
	}

	var current subscription.State
	if err := querier.QueryRow(ctx, getActiveSubscriptionStateSQL, args[0], args[1]).Scan(&current); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return ErrSubscriptionPreciseSearchPersistence
	}
	if _, err := subscription.V2State(current); err != nil || current == subscription.Completed {
		return ErrSubscriptionPreciseSearchPersistence
	}
	return ErrSubscriptionPreciseSearchStateConflict
}

// BindDownloadIntentSubscriptionTx 在事务内将 owner 的下载意图绑定到 owner 的活动订阅。
// 空绑定和相同绑定可幂等重放，已有的不同绑定不会被覆盖。
func (s *Store) BindDownloadIntentSubscriptionTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	intentID uuid.UUID,
	subscriptionID int64,
) error {
	query, args, err := buildBindDownloadIntentSubscription(username, intentID, subscriptionID)
	if err != nil {
		return err
	}
	if ctx == nil || s == nil || tx == nil {
		return ErrDownloadIntentSubscriptionBindingPersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var storedSubscriptionID int64
	if err := tx.QueryRow(ctx, query, args...).Scan(&storedSubscriptionID); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return downloadIntentSubscriptionBindingPersistenceError(ctx)
	}

	var (
		currentSubscriptionID *int64
		targetExists          bool
		currentIntentID       *uuid.UUID
	)
	err = tx.QueryRow(ctx, getDownloadIntentSubscriptionBindingSQL, args...).Scan(
		&currentSubscriptionID,
		&targetExists,
		&currentIntentID,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDownloadIntentSubscriptionBindingNotFound
		}
		return ErrDownloadIntentSubscriptionBindingPersistence
	}
	if !targetExists {
		return ErrDownloadIntentSubscriptionBindingNotFound
	}
	if (currentSubscriptionID != nil && *currentSubscriptionID != subscriptionID) ||
		(currentIntentID != nil && *currentIntentID != intentID) {
		return ErrDownloadIntentSubscriptionBindingConflict
	}
	return ErrDownloadIntentSubscriptionBindingPersistence
}

func buildListSubscriptionsForPreciseSearch(
	cutoff time.Time,
	afterUpdatedAt time.Time,
	afterID int64,
	limit int,
) (string, []any, error) {
	zeroCursor := afterUpdatedAt.IsZero() && afterID == 0
	completeCursor := !afterUpdatedAt.IsZero() && afterID > 0
	if cutoff.IsZero() || (!zeroCursor && !completeCursor) || limit < 1 || limit > maxPreciseSearchSubscriptions {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	args := []any{cutoff.UTC()}
	if zeroCursor {
		return listSubscriptionsForPreciseSearchSQL, append(args, limit), nil
	}
	return listSubscriptionsForPreciseSearchAfterSQL, append(
		args,
		afterUpdatedAt.UTC(),
		afterID,
		limit,
	), nil
}

func buildGetActiveSubscription(username string, id int64) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || id <= 0 {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	return getActiveSubscriptionSQL, []any{id, username}, nil
}

func buildGetOwnedSubscription(username string, id int64) (string, []any, error) {
	_, args, err := buildGetActiveSubscription(username, id)
	if err != nil {
		return "", nil, err
	}
	return getOwnedSubscriptionSQL, args, nil
}

func prepareBestVersionTVCompletion(
	observed Subscription,
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) (string, []any, error) {
	_, args, err := buildCompleteSatisfiedTVSubscription(username, id, expectedState, expectedUpdatedAt)
	if err != nil {
		return "", nil, err
	}
	if observed.ID != id || observed.Username != args[1] || observed.CompletedAt != nil {
		return "", nil, ErrSubscriptionNotFound
	}
	expectedV2, _ := subscription.V2State(expectedState)
	if observed.InternalState != expectedState || observed.V2State != expectedV2 ||
		!observed.UpdatedAt.Equal(expectedUpdatedAt) ||
		(strings.ToLower(strings.TrimSpace(observed.MediaType)) != "tv" &&
			strings.TrimSpace(observed.MediaType) != string(media.TypeTV)) {
		return "", nil, ErrSubscriptionPreciseSearchStateConflict
	}
	resolution, _, err := downloadIntentAcceptanceTVTarget(observed.Season, observed.Config)
	if err != nil {
		return "", nil, ErrSubscriptionPreciseSearchPersistence
	}
	snapshot := resolution.Snapshot
	facts, err := subscription.DescribeTVBestVersion(snapshot)
	if err != nil {
		return "", nil, ErrSubscriptionPreciseSearchPersistence
	}
	if !snapshot.BestVersion || !facts.Complete {
		return "", nil, ErrSubscriptionPreciseSearchStateConflict
	}
	patch := map[string]any{"lack_episode": len(resolution.TargetEpisodes) - len(facts.DownloadedEpisodes)}
	if !snapshot.BestVersionFull {
		patch["current_priority"] = facts.CurrentPriority
	}
	encodedPatch, err := json.Marshal(patch)
	if err != nil {
		return "", nil, ErrSubscriptionPreciseSearchPersistence
	}
	encodedConfig, err := json.Marshal(observed.Config)
	if err != nil {
		return "", nil, ErrSubscriptionPreciseSearchPersistence
	}
	args[6] = string(encodedPatch)
	return completeSatisfiedBestVersionTVSubscriptionSQL,
		append(args, string(encodedConfig), observed.Season, expectedV2), nil
}

func buildCompareAndSwapSubscriptionPreciseSearchState(
	username string,
	id int64,
	next subscription.State,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || id <= 0 ||
		(next != subscription.Active && next != subscription.Acquired && next != subscription.Paused) {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	projected, err := subscription.V2State(next)
	if err != nil {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	return compareAndSwapSubscriptionPreciseSearchStateSQL, []any{
		id,
		username,
		next,
		projected,
		subscription.PendingSearch,
	}, nil
}

func buildCompareAndSwapSubscriptionPreciseSearchStateAt(
	username string,
	id int64,
	next subscription.State,
	expectedUpdatedAt time.Time,
) (string, []any, error) {
	_, args, err := buildCompareAndSwapSubscriptionPreciseSearchState(username, id, next)
	if err != nil || expectedUpdatedAt.IsZero() {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	return compareAndSwapSubscriptionPreciseSearchStateAtSQL,
		append(args, expectedUpdatedAt.UTC()), nil
}

func buildCompleteSatisfiedTVSubscription(
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	expectedV2, err := subscription.V2State(expectedState)
	if username == "" || id <= 0 || expectedUpdatedAt.IsZero() || err != nil ||
		(expectedV2 != "N" && expectedV2 != "R") {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	projected, err := subscription.V2State(subscription.Completed)
	if err != nil {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	return completeSatisfiedSubscriptionSQL, []any{
		id,
		username,
		subscription.Completed,
		projected,
		expectedState,
		expectedUpdatedAt.UTC(),
		`{"lack_episode":0}`,
		strings.ToLower(string(media.TypeTV)),
		"tv",
	}, nil
}

func buildCompleteSatisfiedMovieSubscription(
	username string,
	id int64,
	expectedState subscription.State,
	expectedUpdatedAt time.Time,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	expectedV2, err := subscription.V2State(expectedState)
	if username == "" || id <= 0 || expectedUpdatedAt.IsZero() || err != nil ||
		(expectedV2 != "N" && expectedV2 != "R") {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	projected, err := subscription.V2State(subscription.Completed)
	if err != nil {
		return "", nil, ErrInvalidSubscriptionPreciseSearch
	}
	return completeSatisfiedMovieSubscriptionSQL, []any{
		id,
		username,
		subscription.Completed,
		projected,
		expectedState,
		expectedUpdatedAt.UTC(),
		`{}`,
		strings.ToLower(string(media.TypeMovie)),
		"movie",
	}, nil
}

func buildBindDownloadIntentSubscription(
	username string,
	intentID uuid.UUID,
	subscriptionID int64,
) (string, []any, error) {
	username = strings.TrimSpace(username)
	if username == "" || intentID == uuid.Nil || subscriptionID <= 0 {
		return "", nil, ErrInvalidDownloadIntentSubscriptionBinding
	}
	return bindDownloadIntentSubscriptionSQL, []any{intentID, username, subscriptionID}, nil
}

func subscriptionPreciseSearchStoreReady(store *Store) bool {
	return store != nil && store.db != nil && store.db.Pool != nil
}

func subscriptionPreciseSearchPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrSubscriptionPreciseSearchPersistence
}

func downloadIntentSubscriptionBindingPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ErrDownloadIntentSubscriptionBindingPersistence
}
