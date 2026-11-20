//go:build integration

package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type episodeOrganizationLockOrderResult struct {
	updated int64
	err     error
}

func TestSubscriptionEpisodeTransitionPostgresDoesNotDeadlockOrganizationRollback(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	const username = "episode-organization-lock-order-owner"
	insertSubscriptionIntegrationUsers(t, store, username)

	tests := []struct {
		name     string
		rollback func(context.Context, *Store, organizationLockOrderFixture, OrganizationBatch) (int64, error)
	}{
		{
			name: "fail active batch",
			rollback: func(
				ctx context.Context,
				racingStore *Store,
				fixture organizationLockOrderFixture,
				claimed OrganizationBatch,
			) (int64, error) {
				err := racingStore.FailOrganizationBatch(
					ctx,
					fixture.batch.ID,
					claimed.LeaseToken,
					time.Now().UTC().Add(time.Minute),
					"worker failed",
				)
				if err != nil {
					return 0, err
				}
				return 1, nil
			},
		},
		{
			name: "recover expired lease",
			rollback: func(
				ctx context.Context,
				racingStore *Store,
				_ organizationLockOrderFixture,
				_ OrganizationBatch,
			) (int64, error) {
				return racingStore.RecoverExpiredOrganizationLeases(ctx, time.Now().UTC(), 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := prepareOrganizationLockOrderFixture(t, store, username, uuid.NewString())
			claimed, found, err := store.ClaimOrganizationBatch(
				t.Context(), fixture.batch.ID, time.Now().UTC(), 5*time.Minute,
			)
			if err != nil || !found {
				t.Fatalf("prepare organizing batch = %#v, found %v, %v", claimed, found, err)
			}
			if tt.name == "recover expired lease" {
				if _, err := store.db.Pool.Exec(t.Context(), `
					UPDATE organization_batches
					SET lease_expires_at=now()-interval '1 minute'
					WHERE id=$1`, fixture.batch.ID); err != nil {
					t.Fatalf("expire organization lease: %v", err)
				}
			}

			episodeLock, err := store.db.Pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("begin episode lock transaction: %v", err)
			}
			defer func() { _ = episodeLock.Rollback(context.Background()) }()
			var episodeID int64
			if err := episodeLock.QueryRow(t.Context(), `
				SELECT id
				FROM subscription_episodes
				WHERE subscription_id=$1 AND download_intent_id=$2
				FOR UPDATE`, fixture.subscriptionID, fixture.intentID).Scan(&episodeID); err != nil {
				t.Fatalf("lock organizing episode: %v", err)
			}

			transitionName := "episode-transition-" + uuid.NewString()
			transitionStore := newSubscriptionEpisodeRaceStore(t, store, transitionName)
			operationCtx, cancelOperations := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancelOperations()
			transitionResult := make(chan episodeOrganizationLockOrderResult, 1)
			go func() {
				updated, transitionErr := transitionStore.TransitionSubscriptionEpisodesByIntent(
					operationCtx,
					username,
					fixture.intentID,
					[]SubscriptionEpisodeState{SubscriptionEpisodePendingOrganization},
					SubscriptionEpisodeOrganizing,
				)
				transitionResult <- episodeOrganizationLockOrderResult{updated: updated, err: transitionErr}
			}()
			waitForSubscriptionEpisodeLockWait(t, store, transitionName)

			rollbackName := "organization-rollback-" + uuid.NewString()
			rollbackStore := newSubscriptionEpisodeRaceStore(t, store, rollbackName)
			rollbackResult := make(chan episodeOrganizationLockOrderResult, 1)
			go func() {
				updated, rollbackErr := tt.rollback(operationCtx, rollbackStore, fixture, claimed)
				rollbackResult <- episodeOrganizationLockOrderResult{updated: updated, err: rollbackErr}
			}()
			waitForSubscriptionEpisodeLockWait(t, store, rollbackName)

			if err := episodeLock.Commit(t.Context()); err != nil {
				t.Fatalf("release episode lock: %v", err)
			}

			transition := receiveOrganizationLockOrderResult(
				t, operationCtx, transitionResult, "episode transition",
			)
			rollback := receiveOrganizationLockOrderResult(
				t, operationCtx, rollbackResult, "organization rollback",
			)
			if transition.err != nil {
				t.Fatalf("episode transition returned an error (possible PostgreSQL 40P01): %v", transition.err)
			}
			if transition.updated != 0 {
				t.Fatalf("idempotent episode transition updated = %d, want 0", transition.updated)
			}
			if rollback.err != nil {
				t.Fatalf("organization rollback returned an error (possible PostgreSQL 40P01): %v", rollback.err)
			}
			if rollback.updated != 1 {
				t.Fatalf("organization rollback updated = %d, want 1", rollback.updated)
			}

			assertOrganizationSubscriptionState(
				t,
				store,
				fixture.subscriptionID,
				fixture.intentID,
				"pending_organization",
				"pending_organization",
				true,
			)
			assertOrganizationRollbackLeaseReleased(t, store, fixture.batch.ID)
		})
	}
}

func receiveOrganizationLockOrderResult(
	t *testing.T,
	ctx context.Context,
	result <-chan episodeOrganizationLockOrderResult,
	operation string,
) episodeOrganizationLockOrderResult {
	t.Helper()
	select {
	case received := <-result:
		return received
	case <-ctx.Done():
		t.Fatalf("%s did not finish: %v", operation, ctx.Err())
		return episodeOrganizationLockOrderResult{}
	}
}

func assertOrganizationRollbackLeaseReleased(t *testing.T, store *Store, batchID uuid.UUID) {
	t.Helper()
	var (
		state          OrganizationBatchState
		lastError      string
		leaseToken     *uuid.UUID
		leaseExpiresAt *time.Time
		completedAt    *time.Time
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state, last_error, lease_token, lease_expires_at, completed_at
		FROM organization_batches
		WHERE id=$1`, batchID).Scan(
		&state, &lastError, &leaseToken, &leaseExpiresAt, &completedAt,
	); err != nil {
		t.Fatalf("read rolled back organization batch: %v", err)
	}
	if state != OrganizationBatchFailed || lastError == "" || leaseToken != nil ||
		leaseExpiresAt != nil || completedAt != nil {
		t.Fatalf(
			"rolled back organization batch = state %q error %q lease %v/%v completed %v",
			state, lastError, leaseToken, leaseExpiresAt, completedAt,
		)
	}
}
