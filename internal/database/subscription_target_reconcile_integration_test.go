//go:build integration

package database

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionTargetReconcilePostgresPreservesBoundHistoryAndFiltersCurrentTarget(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "target-reconcile-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "target-reconcile")
	all := []subscription.Episode{
		{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
		{Season: 1, Episode: 3}, {Season: 1, Episode: 4},
	}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, all); err != nil {
		t.Fatalf("initialize target episodes: %v", err)
	}
	intentID := createBoundSubscriptionEpisodeIntent(t, store, username, subscriptionID, "target-reconcile-bound")
	if err := store.BindSubscriptionEpisodes(t.Context(), username, subscriptionID, intentID, all[3:]); err != nil {
		t.Fatalf("bind immutable historical target: %v", err)
	}

	beforeVersion, beforePayload := readSubscriptionTargetIntegrationFacts(t, store, username, subscriptionID, intentID)
	current := all[:2]
	currentTarget := mustSubscriptionTargetResolution(t, 1, 1, 2)
	nextVersion, err := store.ReconcileSubscriptionTarget(t.Context(), SubscriptionTargetReconcile{
		Username: username, SubscriptionID: subscriptionID, ExpectedUpdatedAt: beforeVersion,
		Target: currentTarget,
	})
	if err != nil {
		t.Fatalf("reconcile target: %v", err)
	}
	if !nextVersion.After(beforeVersion) {
		t.Fatalf("next version=%v, before=%v", nextVersion, beforeVersion)
	}

	items, err := store.ListSubscriptionEpisodesForTarget(
		t.Context(), username, subscriptionID, nextVersion, current,
		[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing, subscription.SubscriptionEpisodeAcquired},
		maxSubscriptionEpisodes,
	)
	if err != nil {
		t.Fatalf("list current target: %v", err)
	}
	assertSubscriptionEpisodeCoordinates(t, items, current)
	if _, err := store.ListSubscriptionEpisodesForTarget(
		t.Context(), username, subscriptionID, beforeVersion, current,
		[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing}, maxSubscriptionEpisodes,
	); !errors.Is(err, ErrSubscriptionTargetConflict) {
		t.Fatalf("stale target list error=%v", err)
	}

	var unboundOutside, boundOutside int
	var storedIntent uuid.UUID
	err = store.db.Pool.QueryRow(t.Context(), `
		SELECT
			count(*) FILTER (WHERE episode=3 AND download_intent_id IS NULL),
			count(*) FILTER (WHERE episode=4 AND download_intent_id IS NOT NULL),
			max(download_intent_id::text)::uuid
		FROM subscription_episodes
		WHERE subscription_id=$1 AND season=1`, subscriptionID,
	).Scan(&unboundOutside, &boundOutside, &storedIntent)
	if err != nil {
		t.Fatalf("read retained history: %v", err)
	}
	if unboundOutside != 0 || boundOutside != 1 || storedIntent != intentID {
		t.Fatalf("outside rows=(unbound=%d bound=%d intent=%s)", unboundOutside, boundOutside, storedIntent)
	}
	_, afterPayload := readSubscriptionTargetIntegrationFacts(t, store, username, subscriptionID, intentID)
	if beforePayload != afterPayload {
		t.Fatalf("download intent payload changed: before=%s after=%s", beforePayload, afterPayload)
	}
	var config map[string]any
	var rawConfig []byte
	var storedSeason int
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT season, config FROM subscriptions WHERE id=$1`, subscriptionID).Scan(&storedSeason, &rawConfig); err != nil {
		t.Fatalf("read reconciled config: %v", err)
	}
	if err := json.Unmarshal(rawConfig, &config); err != nil || storedSeason != 1 || config["total_episode"] != float64(2) || config["lack_episode"] != float64(2) {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	idempotentVersion, err := store.ReconcileSubscriptionTarget(t.Context(), SubscriptionTargetReconcile{
		Username: username, SubscriptionID: subscriptionID, ExpectedUpdatedAt: nextVersion, Target: currentTarget,
	})
	if err != nil || !idempotentVersion.Equal(nextVersion) {
		t.Fatalf("idempotent reconcile=(%v,%v), want unchanged %v", idempotentVersion, err, nextVersion)
	}

	if _, err := store.ReconcileSubscriptionTarget(t.Context(), SubscriptionTargetReconcile{
		Username: username, SubscriptionID: subscriptionID, ExpectedUpdatedAt: beforeVersion,
		Target: mustSubscriptionTargetResolution(t, 1, 1, 3),
	}); !errors.Is(err, ErrSubscriptionTargetConflict) {
		t.Fatalf("stale CAS error=%v", err)
	}
	if _, err := store.ReconcileSubscriptionTarget(t.Context(), SubscriptionTargetReconcile{
		Username: "another-owner", SubscriptionID: subscriptionID, ExpectedUpdatedAt: nextVersion,
		Target: mustSubscriptionTargetResolution(t, 1, 1, 3),
	}); !errors.Is(err, ErrSubscriptionTargetNotFound) {
		t.Fatalf("other owner error=%v", err)
	}
}

func TestSubscriptionTargetReconcilePostgresCountsRetainedHistoryAgainstCapacity(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "target-reconcile-capacity-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "target-reconcile-capacity")
	intentID := createHistoricalSubscriptionEpisodeIntent(t, store, username, subscriptionID, "target-reconcile-capacity-history")
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO subscription_episodes (subscription_id, season, episode, state, download_intent_id)
		SELECT $1, (coordinate-1)/5000, ((coordinate-1)%5000)+1, 'acquired', $2
		FROM generate_series(1, $3) AS coordinate`, subscriptionID, intentID, maxSubscriptionEpisodes); err != nil {
		t.Fatalf("insert retained history at capacity: %v", err)
	}
	var beforeVersion time.Time
	var beforeSeason *int
	var beforeConfig string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT updated_at, season, config::text FROM subscriptions WHERE id=$1`, subscriptionID,
	).Scan(&beforeVersion, &beforeSeason, &beforeConfig); err != nil {
		t.Fatalf("read capacity baseline: %v", err)
	}
	_, err := store.ReconcileSubscriptionTarget(t.Context(), SubscriptionTargetReconcile{
		Username: username, SubscriptionID: subscriptionID, ExpectedUpdatedAt: beforeVersion,
		Target: mustSubscriptionTargetResolution(t, 2, 1, 1),
	})
	if !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("capacity error=%v", err)
	}
	var afterVersion time.Time
	var afterSeason *int
	var afterConfig string
	var episodeCount int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT updated_at, season, config::text,
			(SELECT count(*) FROM subscription_episodes WHERE subscription_id=$1)
		FROM subscriptions WHERE id=$1`, subscriptionID,
	).Scan(&afterVersion, &afterSeason, &afterConfig, &episodeCount); err != nil {
		t.Fatalf("read capacity result: %v", err)
	}
	if !afterVersion.Equal(beforeVersion) || !equalOptionalInt(afterSeason, beforeSeason) ||
		afterConfig != beforeConfig || episodeCount != maxSubscriptionEpisodes {
		t.Fatalf("capacity rejection mutated target: version=%v season=%v config=%s count=%d", afterVersion, afterSeason, afterConfig, episodeCount)
	}
}

func TestSubscriptionTargetReconcilePostgresCurrentIntentSwitchInvalidatesSnapshot(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "target-reconcile-race-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "target-reconcile-race")
	initial := []subscription.Episode{{Season: 1, Episode: 1}}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, initial); err != nil {
		t.Fatalf("initialize race target: %v", err)
	}
	oldIntentID := createBoundSubscriptionEpisodeIntent(t, store, username, subscriptionID, "target-reconcile-race-old")
	if err := store.BindSubscriptionEpisodes(t.Context(), username, subscriptionID, oldIntentID, initial); err != nil {
		t.Fatalf("bind old current intent: %v", err)
	}
	newIntentID := createHistoricalSubscriptionEpisodeIntent(t, store, username, subscriptionID, "target-reconcile-race-new")
	expected, _ := readSubscriptionTargetIntegrationFacts(t, store, username, subscriptionID, oldIntentID)

	// application_name 超过 63 字符会被 PostgreSQL 截断，观察端就永远匹配不到，故用短前缀。
	applicationName := "mp-race-" + uuid.NewString()
	racingStore := newSubscriptionEpisodeRaceStore(t, store, applicationName)
	lockTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin intent switch: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var lockedID int64
	if err := lockTx.QueryRow(t.Context(), `
		SELECT id FROM subscriptions WHERE id=$1 AND username=$2 FOR UPDATE`,
		subscriptionID, username).Scan(&lockedID); err != nil {
		t.Fatalf("lock subscription: %v", err)
	}

	type reconcileResult struct{ err error }
	resultCh := make(chan reconcileResult, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		_, reconcileErr := racingStore.ReconcileSubscriptionTarget(ctx, SubscriptionTargetReconcile{
			Username: username, SubscriptionID: subscriptionID, ExpectedUpdatedAt: expected,
			Target: mustSubscriptionTargetResolution(t, 1, 1, 2),
		})
		resultCh <- reconcileResult{err: reconcileErr}
	}()
	waitForSubscriptionEpisodeLockWait(t, store, applicationName)
	if _, err := lockTx.Exec(t.Context(), `
		UPDATE subscriptions SET current_download_intent_id=$1, updated_at=clock_timestamp()
		WHERE id=$2 AND username=$3`, newIntentID, subscriptionID, username); err != nil {
		t.Fatalf("switch current intent: %v", err)
	}
	if err := lockTx.Commit(t.Context()); err != nil {
		t.Fatalf("commit current intent switch: %v", err)
	}
	select {
	case result := <-resultCh:
		if !errors.Is(result.err, ErrSubscriptionTargetConflict) {
			t.Fatalf("blocked reconciliation error=%v", result.err)
		}
	case <-ctx.Done():
		t.Fatalf("blocked reconciliation did not finish: %v", ctx.Err())
	}
	assertSubscriptionCurrentIntent(t, store, username, subscriptionID, newIntentID)
	var episodeTwoCount int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM subscription_episodes
		WHERE subscription_id=$1 AND season=1 AND episode=2`, subscriptionID).Scan(&episodeTwoCount); err != nil {
		t.Fatalf("read stale inserted target: %v", err)
	}
	if episodeTwoCount != 0 {
		t.Fatalf("stale reconciliation inserted %d rows", episodeTwoCount)
	}
}

func mustSubscriptionTargetResolution(t *testing.T, season, start, total int) subscription.TVTargetResolution {
	t.Helper()
	target, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{
		Season: season, StartEpisode: start, TotalEpisode: total, LackEpisode: total,
	})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	return target
}

func equalOptionalInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func readSubscriptionTargetIntegrationFacts(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	intentID uuid.UUID,
) (time.Time, string) {
	t.Helper()
	var version time.Time
	var payload string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT s.updated_at, di.payload::text
		FROM subscriptions AS s
		JOIN download_intents AS di ON di.id=$3 AND di.subscription_id=s.id
		WHERE s.id=$1 AND s.username=$2`, subscriptionID, username, intentID).Scan(&version, &payload); err != nil {
		t.Fatalf("read target and intent facts: %v", err)
	}
	return version.UTC(), payload
}
