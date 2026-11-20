//go:build integration

package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionEpisodeTransitionPostgresLocksIntentBeforeParentAndEpisodes(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "episode-acceptance-lock-order-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)

	episodes := []subscription.Episode{{Season: 9, Episode: 1}}
	fixture := prepareDownloadIntentAcceptanceFixture(
		t, store, username, "transition-lock-order", "电视剧", episodes, episodes, "",
	)

	lockTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始 intent lock 事务: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var intentID uuid.UUID
	if err := lockTx.QueryRow(t.Context(), `
		SELECT id FROM download_intents WHERE id=$1 FOR UPDATE`, fixture.intent.ID,
	).Scan(&intentID); err != nil {
		t.Fatalf("锁定 intent: %v", err)
	}

	applicationName := "ep-accept-lock-" + uuid.NewString()
	racingStore := newSubscriptionEpisodeRaceStore(t, store, applicationName)
	operationCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type transitionResult struct {
		updated int64
		err     error
	}
	result := make(chan transitionResult, 1)
	go func() {
		updated, transitionErr := racingStore.TransitionSubscriptionEpisodesByIntent(
			operationCtx,
			username,
			fixture.intent.ID,
			[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
			SubscriptionEpisodeDownloading,
		)
		result <- transitionResult{updated: updated, err: transitionErr}
	}()
	waitForSubscriptionEpisodeLockWait(t, store, applicationName)

	var subscriptionID int64
	if err := lockTx.QueryRow(t.Context(), `
		SELECT id FROM subscriptions WHERE id=$1 FOR UPDATE NOWAIT`, fixture.subscriptionID,
	).Scan(&subscriptionID); err != nil {
		t.Fatalf("逐集推进在 intent 之前锁了 parent: %v", err)
	}
	var episodeID int64
	if err := lockTx.QueryRow(t.Context(), `
		SELECT id FROM subscription_episodes
		WHERE subscription_id=$1 FOR UPDATE NOWAIT`, fixture.subscriptionID,
	).Scan(&episodeID); err != nil {
		t.Fatalf("逐集推进在 parent 之前锁了 episode: %v", err)
	}
	if err := lockTx.Commit(t.Context()); err != nil {
		t.Fatalf("释放 intent lock: %v", err)
	}

	select {
	case received := <-result:
		if received.err != nil || received.updated != 1 {
			t.Fatalf("锁释放后逐集推进 = (%d, %v), want (1, nil)", received.updated, received.err)
		}
	case <-operationCtx.Done():
		t.Fatalf("锁释放后逐集推进未完成: %v", operationCtx.Err())
	}
}
