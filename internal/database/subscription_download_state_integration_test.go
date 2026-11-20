//go:build integration

package database

import (
	"errors"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestSubscriptionDownloadStatePostgresLifecycleAndOwnership(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertSubscriptionIntegrationUsers(t, store, "download-state-owner", "download-state-other")
	subscriptionID := createSubscriptionIntegrationFixture(t, store, "download-state-owner", "download-state")
	intentID := createBoundSubscriptionEpisodeIntent(
		t,
		store,
		"download-state-owner",
		subscriptionID,
		"download-state-intent",
	)
	if err := store.CompareAndSwapSubscriptionPreciseSearchState(
		t.Context(), "download-state-owner", subscriptionID, subscription.Acquired,
	); err != nil {
		t.Fatalf("prepare acquired subscription: %v", err)
	}

	if err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(), "download-state-owner", intentID, subscription.Acquired, subscription.Downloading,
	); err != nil {
		t.Fatalf("advance parent to downloading: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(), "download-state-owner", intentID, subscription.Acquired, subscription.Downloading,
	); err != nil {
		t.Fatalf("idempotent downloading replay: %v", err)
	}
	if got := mustActiveSubscription(t, store, "download-state-owner", subscriptionID).InternalState; got != subscription.Downloading {
		t.Fatalf("parent state = %q, want downloading", got)
	}

	if err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(), "download-state-other", intentID, subscription.Downloading, subscription.PendingOrganization,
	); !errors.Is(err, ErrSubscriptionDownloadStateNotFound) {
		t.Fatalf("other owner error = %v", err)
	}
	if err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(), "download-state-owner", intentID, subscription.Downloading, subscription.PendingOrganization,
	); err != nil {
		t.Fatalf("advance parent to pending organization: %v", err)
	}
	if got := mustActiveSubscription(t, store, "download-state-owner", subscriptionID).InternalState; got != subscription.PendingOrganization {
		t.Fatalf("parent state = %q, want pending_organization", got)
	}
}
