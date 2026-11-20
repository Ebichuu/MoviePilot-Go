package database

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// UpsertSubscriptionEpisodes 为 owner 的活动订阅幂等初始化缺失集，不覆盖已有进度。
func (s *Store) UpsertSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	episodes []subscription.Episode,
) error {
	query, args, err := buildUpsertSubscriptionEpisodes(username, subscriptionID, episodes)
	if err != nil {
		return err
	}
	if ctx == nil {
		return ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !subscriptionEpisodeStoreReady(s) {
		return ErrSubscriptionEpisodePersistence
	}

	var (
		targetExists bool
		capacityOK   bool
		inserted     int64
	)
	if err := s.db.Pool.QueryRow(ctx, query, args...).Scan(&targetExists, &capacityOK, &inserted); err != nil {
		return subscriptionEpisodePersistenceError(ctx)
	}
	if !targetExists {
		return ErrSubscriptionEpisodeNotFound
	}
	if !capacityOK {
		return ErrInvalidSubscriptionEpisode
	}
	return nil
}

// ListSubscriptionEpisodes 按状态有界读取 owner 的逐集状态。
func (s *Store) ListSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	states []subscription.SubscriptionEpisodeState,
	limit int,
) ([]subscription.SubscriptionEpisode, error) {
	query, args, err := buildListSubscriptionEpisodes(username, subscriptionID, states, limit)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !subscriptionEpisodeStoreReady(s) {
		return nil, ErrSubscriptionEpisodePersistence
	}

	rows, err := s.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, subscriptionEpisodePersistenceError(ctx)
	}
	defer rows.Close()

	items := make([]subscription.SubscriptionEpisode, 0, limit)
	for rows.Next() {
		item, scanErr := scanSubscriptionEpisode(rows)
		if scanErr != nil {
			return nil, subscriptionEpisodePersistenceError(ctx)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, subscriptionEpisodePersistenceError(ctx)
	}
	return items, nil
}

// CompareAndSwapSubscriptionEpisodeState 原子推进 owner 的单集状态。
func (s *Store) CompareAndSwapSubscriptionEpisodeState(
	ctx context.Context,
	username string,
	subscriptionID int64,
	episode subscription.Episode,
	current subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) error {
	query, args, err := buildCompareAndSwapSubscriptionEpisodeState(
		username, subscriptionID, episode, current, next,
	)
	if err != nil {
		return err
	}
	if ctx == nil {
		return ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !subscriptionEpisodeStoreReady(s) {
		return ErrSubscriptionEpisodePersistence
	}
	return compareAndSwapSubscriptionEpisodeState(ctx, s.db.Pool, query, args)
}

// CompareAndSwapSubscriptionEpisodeStateTx 在调用者事务内推进 owner 的单集状态。
func (s *Store) CompareAndSwapSubscriptionEpisodeStateTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	episode subscription.Episode,
	current subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) error {
	query, args, err := buildCompareAndSwapSubscriptionEpisodeState(
		username, subscriptionID, episode, current, next,
	)
	if err != nil {
		return err
	}
	if ctx == nil || s == nil || tx == nil {
		return ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return compareAndSwapSubscriptionEpisodeState(ctx, tx, query, args)
}

func compareAndSwapSubscriptionEpisodeState(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
) error {
	var id int64
	if err := querier.QueryRow(ctx, query, args...).Scan(&id); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return subscriptionEpisodePersistenceError(ctx)
	}

	var stored subscription.SubscriptionEpisodeState
	if err := querier.QueryRow(ctx, getActiveSubscriptionEpisodeStateSQL, args[:4]...).Scan(&stored); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubscriptionEpisodeNotFound
		}
		return ErrSubscriptionEpisodePersistence
	}
	if !validSubscriptionEpisodeState(stored) {
		return ErrSubscriptionEpisodePersistence
	}
	return ErrSubscriptionEpisodeStateConflict
}

// BindSubscriptionEpisodes 将一批逐集记录原子绑定到订阅的当前下载意图。
func (s *Store) BindSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) error {
	query, args, expected, err := buildBindSubscriptionEpisodes(
		username, subscriptionID, intentID, episodes,
	)
	if err != nil {
		return err
	}
	if ctx == nil {
		return ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !subscriptionEpisodeStoreReady(s) {
		return ErrSubscriptionEpisodePersistence
	}
	return bindSubscriptionEpisodes(ctx, s.db.Pool, query, args, expected)
}

// BindSubscriptionEpisodesTx 在调用者事务内原子绑定一批逐集记录到当前下载意图。
func (s *Store) BindSubscriptionEpisodesTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) error {
	query, args, expected, err := buildBindSubscriptionEpisodes(
		username, subscriptionID, intentID, episodes,
	)
	if err != nil {
		return err
	}
	if ctx == nil || s == nil || tx == nil {
		return ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return bindSubscriptionEpisodes(ctx, tx, query, args, expected)
}

// VerifySubscriptionEpisodesBoundToIntentTx 验证一次逐集下载提交是当前意图的精确幂等重放。
func (s *Store) VerifySubscriptionEpisodesBoundToIntentTx(
	ctx context.Context,
	tx pgx.Tx,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) error {
	query, args, expected, err := buildVerifySubscriptionEpisodesBoundToIntent(
		username, subscriptionID, intentID, episodes,
	)
	if err != nil {
		return err
	}
	if ctx == nil || s == nil || tx == nil {
		return ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var (
		targetExists bool
		requested    int64
		matched      int64
		bound        int64
	)
	if err := tx.QueryRow(ctx, query, args...).Scan(&targetExists, &requested, &matched, &bound); err != nil {
		return subscriptionEpisodePersistenceError(ctx)
	}
	if !targetExists {
		return ErrSubscriptionEpisodeNotFound
	}
	if requested != expected || matched != expected || bound != expected {
		return ErrSubscriptionEpisodeBindingConflict
	}
	return nil
}

func bindSubscriptionEpisodes(
	ctx context.Context,
	querier pgxQueryRower,
	query string,
	args []any,
	expected int64,
) error {
	var (
		targetExists bool
		intentExists bool
		requested    int64
		matched      int64
		eligible     int64
		updated      int64
	)
	if err := querier.QueryRow(ctx, query, args...).Scan(
		&targetExists,
		&intentExists,
		&requested,
		&matched,
		&eligible,
		&updated,
	); err != nil {
		return subscriptionEpisodePersistenceError(ctx)
	}
	if !targetExists || !intentExists || requested != expected || matched != expected {
		return ErrSubscriptionEpisodeNotFound
	}
	if eligible != expected {
		return ErrSubscriptionEpisodeBindingConflict
	}
	if updated != expected {
		return ErrSubscriptionEpisodePersistence
	}
	return nil
}

// TransitionSubscriptionEpisodesByIntent 原子推进 owner 当前下载意图绑定的所有匹配集。
// 返回值是实际更新的逐集数量；当前 intent 没有绑定逐集，或绑定逐集均已处于
// 目标状态及其后继状态时，返回 (0, nil) 作为合法幂等重放结果。
func (s *Store) TransitionSubscriptionEpisodesByIntent(
	ctx context.Context,
	username string,
	intentID uuid.UUID,
	fromStates []subscription.SubscriptionEpisodeState,
	next subscription.SubscriptionEpisodeState,
) (int64, error) {
	query, args, err := buildTransitionSubscriptionEpisodesByIntent(username, intentID, fromStates, next)
	if err != nil {
		return 0, err
	}
	if ctx == nil {
		return 0, ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !subscriptionEpisodeStoreReady(s) {
		return 0, ErrSubscriptionEpisodePersistence
	}

	var (
		targetExists       bool
		stateSetCompatible bool
		updated            int64
	)
	if err := s.db.Pool.QueryRow(ctx, query, args...).Scan(
		&targetExists,
		&stateSetCompatible,
		&updated,
	); err != nil {
		return 0, subscriptionEpisodePersistenceError(ctx)
	}
	if !targetExists {
		return 0, ErrSubscriptionEpisodeNotFound
	}
	if !stateSetCompatible {
		return 0, ErrSubscriptionEpisodeStateConflict
	}
	return updated, nil
}

// SummarizeSubscriptionEpisodes 返回 owner 的单订阅逐集状态汇总。
func (s *Store) SummarizeSubscriptionEpisodes(
	ctx context.Context,
	username string,
	subscriptionID int64,
) (subscription.SubscriptionEpisodeSummary, error) {
	query, args, err := buildSummarizeSubscriptionEpisodes(username, subscriptionID)
	if err != nil {
		return subscription.SubscriptionEpisodeSummary{}, err
	}
	if ctx == nil {
		return subscription.SubscriptionEpisodeSummary{}, ErrSubscriptionEpisodePersistence
	}
	if err := ctx.Err(); err != nil {
		return subscription.SubscriptionEpisodeSummary{}, err
	}
	if !subscriptionEpisodeStoreReady(s) {
		return subscription.SubscriptionEpisodeSummary{}, ErrSubscriptionEpisodePersistence
	}

	summary, err := scanSubscriptionEpisodeSummary(s.db.Pool.QueryRow(ctx, query, args...))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return subscription.SubscriptionEpisodeSummary{}, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return subscription.SubscriptionEpisodeSummary{}, ErrSubscriptionEpisodeNotFound
		}
		return subscription.SubscriptionEpisodeSummary{}, ErrSubscriptionEpisodePersistence
	}
	return summary, nil
}
