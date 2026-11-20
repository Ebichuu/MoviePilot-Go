//go:build integration

package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestListSubscriptionsForRSSPostgresReturnsV2ResolvedAndPendingTargets(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "rss-target-owner")

	pendingID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-pending")
	pausedID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-paused")
	inconsistentRID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-inconsistent-r")
	inconsistentPID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-inconsistent-p")
	busyActiveID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-busy-active")
	busyPendingID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-busy-pending")
	eligibleStates := []struct {
		state   subscription.State
		v2State string
	}{
		{state: subscription.PendingIdentification, v2State: "P"},
		{state: subscription.Active, v2State: "R"},
		{state: subscription.Acquired, v2State: "R"},
		{state: subscription.Downloading, v2State: "R"},
		{state: subscription.PendingOrganization, v2State: "R"},
		{state: subscription.Organizing, v2State: "R"},
		{state: subscription.PendingIndex, v2State: "R"},
		{state: subscription.Indexed, v2State: "R"},
	}
	eligibleIDs := make([]int64, 0, len(eligibleStates))
	for _, eligible := range eligibleStates {
		id := createSubscriptionIntegrationFixture(
			t, store, "rss-target-owner", "rss-target-eligible-"+string(eligible.state),
		)
		eligibleIDs = append(eligibleIDs, id)
	}
	afterCutoffID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-after-cutoff")
	completedID := createSubscriptionIntegrationFixture(t, store, "rss-target-owner", "rss-target-completed")

	base := time.Date(2026, time.August, 16, 1, 0, 0, 0, time.UTC)
	cutoff := base.Add(time.Hour)
	updates := []struct {
		id          int64
		state       subscription.State
		v2State     string
		updatedAt   time.Time
		completedAt *time.Time
	}{
		{id: pendingID, state: subscription.PendingSearch, v2State: "N", updatedAt: base},
		{id: pausedID, state: subscription.Paused, v2State: "S", updatedAt: base},
		{id: inconsistentRID, state: subscription.Active, v2State: "N", updatedAt: base},
		{id: inconsistentPID, state: subscription.PendingIdentification, v2State: "R", updatedAt: base},
		{id: busyActiveID, state: subscription.Active, v2State: "R", updatedAt: base},
		{id: busyPendingID, state: subscription.PendingIdentification, v2State: "P", updatedAt: base},
		{id: afterCutoffID, state: subscription.Active, v2State: "R", updatedAt: cutoff.Add(time.Millisecond)},
		{id: completedID, state: subscription.Completed, v2State: "", updatedAt: base, completedAt: &base},
	}
	for index, eligible := range eligibleStates {
		updates = append(updates, struct {
			id          int64
			state       subscription.State
			v2State     string
			updatedAt   time.Time
			completedAt *time.Time
		}{
			id: eligibleIDs[index], state: eligible.state, v2State: eligible.v2State, updatedAt: base,
		})
	}
	for _, update := range updates {
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE subscriptions
			SET internal_state=$1, v2_state=$2, updated_at=$3, completed_at=$4
			WHERE id=$5`, update.state, update.v2State, update.updatedAt, update.completedAt, update.id); err != nil {
			t.Fatalf("prepare RSS subscription id=%d: %v", update.id, err)
		}
	}
	for index, id := range []int64{busyActiveID, busyPendingID} {
		intent := mustDatabaseDownloadIntent(t, "rss-target-owner", "rss-target-busy-"+string(rune('a'+index)))
		if _, err := store.CreateDownloadIntent(t.Context(), "rss-target-owner", intent); err != nil {
			t.Fatalf("create busy RSS intent: %v", err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE subscriptions SET current_download_intent_id=$1 WHERE id=$2`, intent.ID, id); err != nil {
			t.Fatalf("bind busy RSS subscription id=%d: %v", id, err)
		}
	}

	all, err := store.ListSubscriptionsForRSS(t.Context(), cutoff, time.Time{}, 0, 100)
	if err != nil {
		t.Fatalf("ListSubscriptionsForRSS() error = %v", err)
	}
	assertOrderedSubscriptionIDs(t, all, eligibleIDs)

	firstPage, err := store.ListSubscriptionsForRSS(t.Context(), cutoff, time.Time{}, 0, 3)
	if err != nil {
		t.Fatalf("ListSubscriptionsForRSS() first page error = %v", err)
	}
	assertOrderedSubscriptionIDs(t, firstPage, eligibleIDs[:3])
	secondPage, err := store.ListSubscriptionsForRSS(
		t.Context(), cutoff, firstPage[2].UpdatedAt, firstPage[2].ID, 10,
	)
	if err != nil {
		t.Fatalf("ListSubscriptionsForRSS() second page error = %v", err)
	}
	assertOrderedSubscriptionIDs(t, secondPage, eligibleIDs[3:])
}

func TestCompareAndSwapSubscriptionRSSStateTxPostgresPreservesV2Pending(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "rss-cas-owner", "rss-cas-other")

	pendingID := createSubscriptionIntegrationFixture(t, store, "rss-cas-owner", "rss-cas-pending")
	activeID := createSubscriptionIntegrationFixture(t, store, "rss-cas-owner", "rss-cas-active")
	v2PendingID := createSubscriptionIntegrationFixture(t, store, "rss-cas-owner", "rss-cas-v2-pending")
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R', updated_at=now()
		WHERE id=$2`, subscription.Active, activeID); err != nil {
		t.Fatalf("prepare active RSS subscription: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='P', updated_at=now()
		WHERE id=$2`, subscription.PendingIdentification, v2PendingID); err != nil {
		t.Fatalf("prepare pending-identification RSS subscription: %v", err)
	}

	if err := runRSSSubscriptionCASTx(t, store, "rss-cas-owner", pendingID, subscription.Active); !errors.Is(err, ErrSubscriptionPreciseSearchStateConflict) {
		t.Fatalf("pending_search RSS CAS error = %v, want state conflict", err)
	}
	if err := runRSSSubscriptionCASTx(t, store, "rss-cas-other", activeID, subscription.Active); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("other owner RSS CAS error = %v, want not found", err)
	}
	if err := runRSSSubscriptionCASTx(t, store, "rss-cas-owner", activeID, subscription.Active); err != nil {
		t.Fatalf("active RSS CAS error = %v", err)
	}
	if err := runRSSSubscriptionCASTx(
		t, store, "rss-cas-owner", v2PendingID, subscription.PendingIdentification,
	); err != nil {
		t.Fatalf("pending-identification RSS CAS error = %v", err)
	}

	acquired := mustActiveSubscription(t, store, "rss-cas-owner", activeID)
	if acquired.InternalState != subscription.Acquired || acquired.V2State != "R" {
		t.Fatalf("RSS CAS state = (%q, %q), want (%q, R)", acquired.InternalState, acquired.V2State, subscription.Acquired)
	}
	preserved := mustActiveSubscription(t, store, "rss-cas-owner", v2PendingID)
	if preserved.InternalState != subscription.PendingIdentification || preserved.V2State != "P" {
		t.Fatalf(
			"pending-identification RSS CAS state = (%q, %q), want (%q, P)",
			preserved.InternalState, preserved.V2State, subscription.PendingIdentification,
		)
	}

	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin invalid expected-state transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	err = store.CompareAndSwapSubscriptionRSSStateTx(
		t.Context(), tx, "rss-cas-owner", pendingID,
		subscription.PendingSearch,
	)
	if !errors.Is(err, ErrInvalidSubscriptionPreciseSearch) {
		t.Fatalf("pending_search expected state error = %v, want invalid argument", err)
	}
}

func runRSSSubscriptionCASTx(
	t *testing.T,
	store *Store,
	username string,
	subscriptionID int64,
	expected subscription.State,
) error {
	t.Helper()
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin RSS CAS transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.CompareAndSwapSubscriptionRSSStateTx(
		t.Context(), tx, username, subscriptionID,
		expected,
	); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}
