//go:build integration

package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

func TestOrganizationPostgresLocksParentBeforeEpisodes(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "organization-lock-order-owner"
	insertSubscriptionIntegrationUsers(t, store, username)

	t.Run("claim due batches", func(t *testing.T) {
		fixture := prepareOrganizationLockOrderFixture(t, store, username, "claim-many")
		updated := assertOrganizationOperationLocksParentBeforeEpisodes(
			t, store, fixture.subscriptionID, "organization-lock-order-claim-many",
			func(ctx context.Context, racingStore *Store) (int64, error) {
				claimed, err := racingStore.ClaimOrganizationBatches(
					ctx, time.Now().UTC(), 1, time.Minute,
				)
				if err != nil {
					return 0, err
				}
				if len(claimed) != 1 || claimed[0].ID != fixture.batch.ID {
					return int64(len(claimed)), ErrOrganizationPersistence
				}
				return int64(len(claimed)), nil
			},
		)
		if updated != 1 {
			t.Fatalf("claim many result = %d, want 1", updated)
		}
		assertOrganizationSubscriptionState(
			t, store, fixture.subscriptionID, fixture.intentID, "organizing", "organizing", true,
		)
	})

	t.Run("claim one batch", func(t *testing.T) {
		fixture := prepareOrganizationLockOrderFixture(t, store, username, "claim")
		updated := assertOrganizationOperationLocksParentBeforeEpisodes(
			t, store, fixture.subscriptionID, "organization-lock-order-claim",
			func(ctx context.Context, racingStore *Store) (int64, error) {
				_, found, err := racingStore.ClaimOrganizationBatch(
					ctx, fixture.batch.ID, time.Now().UTC(), time.Minute,
				)
				if !found || err != nil {
					return 0, err
				}
				return 1, nil
			},
		)
		if updated != 1 {
			t.Fatalf("claim result = %d, want 1", updated)
		}
		assertOrganizationSubscriptionState(
			t, store, fixture.subscriptionID, fixture.intentID, "organizing", "organizing", true,
		)
	})

	t.Run("fail active batch", func(t *testing.T) {
		fixture := prepareOrganizationLockOrderFixture(t, store, username, "fail")
		claimed, found, err := store.ClaimOrganizationBatch(
			t.Context(), fixture.batch.ID, time.Now().UTC(), time.Minute,
		)
		if err != nil || !found {
			t.Fatalf("prepare active batch = %#v, found %v, %v", claimed, found, err)
		}
		updated := assertOrganizationOperationLocksParentBeforeEpisodes(
			t, store, fixture.subscriptionID, "organization-lock-order-fail",
			func(ctx context.Context, racingStore *Store) (int64, error) {
				if err := racingStore.FailOrganizationBatch(
					ctx, fixture.batch.ID, claimed.LeaseToken,
					time.Now().UTC().Add(time.Minute), "worker failed",
				); err != nil {
					return 0, err
				}
				return 1, nil
			},
		)
		if updated != 1 {
			t.Fatalf("fail result = %d, want 1", updated)
		}
		assertOrganizationSubscriptionState(
			t, store, fixture.subscriptionID, fixture.intentID,
			"pending_organization", "pending_organization", true,
		)
	})

	t.Run("recover expired batch", func(t *testing.T) {
		fixture := prepareOrganizationLockOrderFixture(t, store, username, "recover")
		now := time.Now().UTC()
		claimed, found, err := store.ClaimOrganizationBatch(
			t.Context(), fixture.batch.ID, now, time.Minute,
		)
		if err != nil || !found {
			t.Fatalf("prepare expired batch = %#v, found %v, %v", claimed, found, err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE organization_batches SET lease_expires_at=$2 WHERE id=$1`,
			fixture.batch.ID, now.Add(-time.Minute),
		); err != nil {
			t.Fatalf("expire organization batch: %v", err)
		}
		updated := assertOrganizationOperationLocksParentBeforeEpisodes(
			t, store, fixture.subscriptionID, "organization-lock-order-recover",
			func(ctx context.Context, racingStore *Store) (int64, error) {
				return racingStore.RecoverExpiredOrganizationLeases(ctx, now.Add(time.Second), 1)
			},
		)
		if updated != 1 {
			t.Fatalf("recovery result = %d, want 1", updated)
		}
		assertOrganizationSubscriptionState(
			t, store, fixture.subscriptionID, fixture.intentID,
			"pending_organization", "pending_organization", true,
		)
	})
}

type organizationLockOrderFixture struct {
	subscriptionID int64
	intentID       uuid.UUID
	batch          OrganizationBatch
}

func prepareOrganizationLockOrderFixture(
	t *testing.T,
	store *Store,
	username string,
	key string,
) organizationLockOrderFixture {
	t.Helper()
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "organization-lock-order-"+key)
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, subscription_id, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, $5, 'completed')`, intentID, username, subscriptionID,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatalf("create completed intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='pending_organization', v2_state='R'
		WHERE id=$2`, intentID, subscriptionID); err != nil {
		t.Fatalf("bind current intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO subscription_episodes (subscription_id, season, episode, state, download_intent_id)
		VALUES ($1, 1, 1, 'pending_organization', $2)`, subscriptionID, intentID); err != nil {
		t.Fatalf("create bound episode: %v", err)
	}
	root := t.TempDir()
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(
			filepath.Join(root, "downloads", "one.mkv"),
			filepath.Join(root, "library", "one.mkv"),
			organizer.Copy,
		),
	})
	if err != nil {
		t.Fatalf("build organization plan: %v", err)
	}
	snapshots := organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, plan)
	batch, err := store.CreateOrganizationBatch(
		t.Context(), username, intentID, filepath.Join(root, "downloads"), plan, snapshots,
	)
	if err != nil {
		t.Fatalf("create organization batch: %v", err)
	}
	return organizationLockOrderFixture{
		subscriptionID: subscriptionID,
		intentID:       intentID,
		batch:          batch,
	}
}

func assertOrganizationOperationLocksParentBeforeEpisodes(
	t *testing.T,
	store *Store,
	subscriptionID int64,
	applicationName string,
	operation func(context.Context, *Store) (int64, error),
) int64 {
	t.Helper()
	lockTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin parent lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var lockedID int64
	if err := lockTx.QueryRow(t.Context(), `
		SELECT id FROM subscriptions WHERE id=$1 FOR UPDATE`, subscriptionID,
	).Scan(&lockedID); err != nil {
		t.Fatalf("lock parent subscription: %v", err)
	}

	racingStore := newSubscriptionEpisodeRaceStore(t, store, applicationName)
	operationCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type operationResult struct {
		updated int64
		err     error
	}
	resultCh := make(chan operationResult, 1)
	go func() {
		updated, operationErr := operation(operationCtx, racingStore)
		resultCh <- operationResult{updated: updated, err: operationErr}
	}()

	waitForSubscriptionEpisodeLockWait(t, store, applicationName)
	var episodeID int64
	if err := lockTx.QueryRow(t.Context(), `
		SELECT id
		FROM subscription_episodes
		WHERE subscription_id=$1
		FOR UPDATE NOWAIT`, subscriptionID,
	).Scan(&episodeID); err != nil {
		t.Fatalf("operation locked episode before parent: %v", err)
	}
	if err := lockTx.Commit(t.Context()); err != nil {
		t.Fatalf("release parent and episode locks: %v", err)
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("organization operation after parent release: %v", result.err)
		}
		return result.updated
	case <-operationCtx.Done():
		t.Fatalf("organization operation did not finish: %v", operationCtx.Err())
		return 0
	}
}
