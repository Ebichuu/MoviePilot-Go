//go:build integration

package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionEpisodesPostgresCurrentIntentSwitchWinsBlockedOperations(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "episode-current-intent-race-owner"
	insertSubscriptionIntegrationUsers(t, store, username)

	tests := []struct {
		name               string
		operation          string
		requiresOldBinding bool
	}{
		{name: "binding", operation: "bind"},
		{name: "exact replay", operation: "verify", requiresOldBinding: true},
		{name: "state transition", operation: "transition", requiresOldBinding: true},
		{name: "single episode cas", operation: "cas", requiresOldBinding: true},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subscriptionID := createSubscriptionIntegrationFixture(
				t, store, username, "episode-current-intent-race-"+tt.operation,
			)
			episodes := []subscription.Episode{
				{Season: 8 + index, Episode: 1},
				{Season: 8 + index, Episode: 2},
			}
			if err := store.UpsertSubscriptionEpisodes(
				t.Context(), username, subscriptionID, episodes,
			); err != nil {
				t.Fatalf("initialize episodes: %v", err)
			}

			oldIntentID := createBoundSubscriptionEpisodeIntent(
				t, store, username, subscriptionID,
				"episode-current-intent-race-old-"+tt.operation,
			)
			if tt.requiresOldBinding {
				if err := store.BindSubscriptionEpisodes(
					t.Context(), username, subscriptionID, oldIntentID, episodes,
				); err != nil {
					t.Fatalf("bind old current intent episodes: %v", err)
				}
				if err := store.CompareAndSwapSubscriptionPreciseSearchState(
					t.Context(), username, subscriptionID, subscription.Acquired,
				); err != nil {
					t.Fatalf("advance subscription for old current intent: %v", err)
				}
			}
			newIntentID := createHistoricalSubscriptionEpisodeIntent(
				t, store, username, subscriptionID,
				"episode-current-intent-race-new-"+tt.operation,
			)

			applicationName := "moviepilot-episode-race-" + uuid.NewString()
			racingStore := newSubscriptionEpisodeRaceStore(t, store, applicationName)
			lockTx, err := store.db.Pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("begin current intent switch transaction: %v", err)
			}
			defer func() { _ = lockTx.Rollback(context.Background()) }()
			var lockedID int64
			if err := lockTx.QueryRow(t.Context(), `
				SELECT id
				FROM subscriptions
				WHERE id=$1 AND username=$2
				FOR UPDATE`, subscriptionID, username,
			).Scan(&lockedID); err != nil {
				t.Fatalf("lock subscription before current intent switch: %v", err)
			}
			if lockedID != subscriptionID {
				t.Fatalf("locked subscription = %d, want %d", lockedID, subscriptionID)
			}

			type operationResult struct {
				updated int64
				err     error
			}
			operationCtx, cancelOperation := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancelOperation()
			resultCh := make(chan operationResult, 1)
			go func() {
				var result operationResult
				switch tt.operation {
				case "bind":
					result.err = racingStore.BindSubscriptionEpisodes(
						operationCtx, username, subscriptionID, oldIntentID, episodes,
					)
				case "verify":
					tx, beginErr := racingStore.db.Pool.Begin(operationCtx)
					if beginErr != nil {
						result.err = beginErr
						break
					}
					defer func() { _ = tx.Rollback(context.Background()) }()
					result.err = racingStore.VerifySubscriptionEpisodesBoundToIntentTx(
						operationCtx, tx, username, subscriptionID, oldIntentID, episodes,
					)
				case "transition":
					result.updated, result.err = racingStore.TransitionSubscriptionEpisodesByIntent(
						operationCtx, username, oldIntentID,
						[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
						SubscriptionEpisodeDownloading,
					)
				case "cas":
					result.err = racingStore.CompareAndSwapSubscriptionEpisodeState(
						operationCtx, username, subscriptionID, episodes[0],
						SubscriptionEpisodeAcquired, SubscriptionEpisodeDownloading,
					)
				}
				resultCh <- result
			}()

			waitForSubscriptionEpisodeLockWait(t, store, applicationName)
			result, err := lockTx.Exec(t.Context(), `
				UPDATE subscriptions
				SET current_download_intent_id=$1, updated_at=now()
				WHERE id=$2 AND username=$3`, newIntentID, subscriptionID, username)
			if err != nil {
				t.Fatalf("switch current intent while old operation waits: %v", err)
			}
			if result.RowsAffected() != 1 {
				t.Fatalf("current intent switch rows = %d, want 1", result.RowsAffected())
			}
			if err := lockTx.Commit(t.Context()); err != nil {
				t.Fatalf("commit current intent switch: %v", err)
			}

			var operation operationResult
			select {
			case operation = <-resultCh:
			case <-operationCtx.Done():
				t.Fatalf("old current intent operation did not finish: %v", operationCtx.Err())
			}
			if operation.updated != 0 || !errors.Is(operation.err, ErrSubscriptionEpisodeNotFound) {
				t.Fatalf(
					"old current intent %s = (%d, %v), want (0, ErrSubscriptionEpisodeNotFound)",
					tt.operation, operation.updated, operation.err,
				)
			}

			assertSubscriptionCurrentIntent(t, store, username, subscriptionID, newIntentID)
			if tt.requiresOldBinding {
				assertSubscriptionEpisodeIntentIDs(
					t, store, username, subscriptionID,
					map[int]uuid.UUID{
						episodes[0].Episode: oldIntentID,
						episodes[1].Episode: oldIntentID,
					},
				)
				for _, episode := range episodes {
					assertSubscriptionEpisodeState(
						t, store, username, subscriptionID, episode, SubscriptionEpisodeAcquired,
					)
				}
				return
			}
			assertSubscriptionEpisodeIntentIDs(
				t, store, username, subscriptionID, map[int]uuid.UUID{},
			)
			for _, episode := range episodes {
				assertSubscriptionEpisodeState(
					t, store, username, subscriptionID, episode, SubscriptionEpisodeMissing,
				)
			}
		})
	}
}

func newSubscriptionEpisodeRaceStore(
	t *testing.T,
	base *Store,
	applicationName string,
) *Store {
	t.Helper()
	config := base.db.Pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("create isolated race connection pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("ping isolated race connection pool: %v", err)
	}
	return NewStore(&DB{Pool: pool}, nil)
}

func waitForSubscriptionEpisodeLockWait(
	t *testing.T,
	store *Store,
	applicationName string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		var waiting bool
		err := store.db.Pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE application_name=$1
					AND wait_event_type='Lock'
					AND cardinality(pg_blocking_pids(pid)) > 0
			)`, applicationName).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe blocked episode operation: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("episode operation did not wait on the subscription lock: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}
