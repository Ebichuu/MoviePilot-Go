//go:build integration

package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRSSEndpointChangeResetsCursorStatePostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-endpoint-reset")
	const oldEndpoint = "https://tracker.example/rss/old"
	const newEndpoint = "https://tracker.example/rss/new"

	if _, err := store.EnsureRSSCursor(t.Context(), siteID, oldEndpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() old endpoint error = %v", err)
	}
	highWater := time.Date(2026, time.August, 12, 12, 30, 0, 0, time.UTC)
	lastAttempt := highWater.Add(-2 * time.Minute)
	lastSuccess := highWater.Add(-time.Minute)
	leaseToken := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE site_rss_cursors
		SET etag='"old-etag"', last_modified='Wed, 12 Aug 2026 12:30:00 GMT',
			high_water_published_at=$2, high_water_item_key='sha256:old-item',
			next_poll_at=now()+interval '1 hour', failure_count=3,
			last_error='temporary failure', last_attempt_at=$3, last_success_at=$4,
			lease_token=$5,
			lease_expires_at=now()+interval '1 hour'
		WHERE site_id=$1`, siteID, highWater, lastAttempt, lastSuccess, leaseToken); err != nil {
		t.Fatalf("prepare RSS cursor state: %v", err)
	}

	cursor, err := store.EnsureRSSCursor(t.Context(), siteID, newEndpoint)
	if err != nil {
		t.Fatalf("EnsureRSSCursor() new endpoint error = %v", err)
	}
	if cursor.Endpoint != newEndpoint || cursor.ETag != "" || cursor.LastModified != "" {
		t.Fatalf("cursor validators after endpoint change = %#v", cursor)
	}
	if cursor.HighWaterPublishedAt != nil || cursor.HighWaterItemKey != "" {
		t.Fatalf("cursor high water after endpoint change = %#v", cursor)
	}
	if cursor.FailureCount != 0 || cursor.LastError != "" {
		t.Fatalf("cursor failure state after endpoint change = %#v", cursor)
	}
	if cursor.LastAttemptAt != nil || cursor.LastSuccessAt != nil {
		t.Fatalf("cursor poll history after endpoint change = %#v", cursor)
	}
	if cursor.LeaseToken != uuid.Nil || cursor.LeaseExpiresAt != nil {
		t.Fatalf("cursor lease after endpoint change = %#v", cursor)
	}

	due, err := store.ListDueRSSCursors(t.Context(), time.Now().UTC().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListDueRSSCursors() after endpoint change error = %v", err)
	}
	if len(due) != 1 || due[0].SiteID != siteID || due[0].Endpoint != newEndpoint {
		t.Fatalf("due cursors after endpoint change = %#v", due)
	}
}

func TestRSSListDueCursorsOrdersLimitsAndFiltersPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	snapshot := time.Now().UTC()
	early := snapshot.Add(-3 * time.Hour)
	tied := snapshot.Add(-2 * time.Hour)

	earlySite := insertRSSIntegrationSite(t, store, "rss-due-early")
	tiedFirstSite := insertRSSIntegrationSite(t, store, "rss-due-tied-first")
	tiedSecondSite := insertRSSIntegrationSite(t, store, "rss-due-tied-second")
	leasedSite := insertRSSIntegrationSite(t, store, "rss-due-leased")
	futureSite := insertRSSIntegrationSite(t, store, "rss-due-future")
	disabledSite := insertRSSIntegrationSite(t, store, "rss-due-disabled")

	fixtures := []struct {
		siteID       int64
		endpoint     string
		nextPollAt   time.Time
		leaseToken   *uuid.UUID
		leaseExpires *time.Time
	}{
		{siteID: earlySite, endpoint: "sha256:due-early", nextPollAt: early},
		{siteID: tiedFirstSite, endpoint: "sha256:due-tied-first", nextPollAt: tied},
		{siteID: tiedSecondSite, endpoint: "sha256:due-tied-second", nextPollAt: tied, leaseToken: rssBoundaryUUIDPtr(uuid.New()), leaseExpires: rssBoundaryTimePtr(snapshot.Add(-time.Minute))},
		{siteID: leasedSite, endpoint: "sha256:due-leased", nextPollAt: early, leaseToken: rssBoundaryUUIDPtr(uuid.New()), leaseExpires: rssBoundaryTimePtr(snapshot.Add(time.Hour))},
		{siteID: futureSite, endpoint: "sha256:due-future", nextPollAt: snapshot.Add(time.Hour)},
		{siteID: disabledSite, endpoint: "sha256:due-disabled", nextPollAt: early},
	}
	for _, fixture := range fixtures {
		if _, err := store.EnsureRSSCursor(t.Context(), fixture.siteID, fixture.endpoint); err != nil {
			t.Fatalf("EnsureRSSCursor(%q) error = %v", fixture.endpoint, err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE site_rss_cursors
			SET next_poll_at=$2, lease_token=$3, lease_expires_at=$4
			WHERE site_id=$1`, fixture.siteID, fixture.nextPollAt, fixture.leaseToken, fixture.leaseExpires); err != nil {
			t.Fatalf("prepare due cursor %q: %v", fixture.endpoint, err)
		}
	}
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE sites SET enabled=FALSE WHERE id=$1`, disabledSite); err != nil {
		t.Fatalf("disable RSS site: %v", err)
	}

	limited, err := store.ListDueRSSCursors(t.Context(), snapshot, 2)
	if err != nil {
		t.Fatalf("ListDueRSSCursors() limited error = %v", err)
	}
	if len(limited) != 2 || limited[0].SiteID != earlySite || limited[1].SiteID != tiedFirstSite {
		t.Fatalf("limited due cursors = %#v", limited)
	}

	all, err := store.ListDueRSSCursors(t.Context(), snapshot, 10)
	if err != nil {
		t.Fatalf("ListDueRSSCursors() all error = %v", err)
	}
	wantSiteIDs := []int64{earlySite, tiedFirstSite, tiedSecondSite}
	if len(all) != len(wantSiteIDs) {
		t.Fatalf("all due cursors = %#v, want site IDs %v", all, wantSiteIDs)
	}
	for i, wantSiteID := range wantSiteIDs {
		if all[i].SiteID != wantSiteID {
			t.Fatalf("all due cursors[%d].SiteID = %d, want %d; cursors = %#v", i, all[i].SiteID, wantSiteID, all)
		}
	}
}

func TestRSSHighWaterMarkTieBreaksAndNeverRegressesPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-high-water")
	const endpoint = "sha256:high-water"
	if _, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}

	published := time.Date(2026, time.August, 12, 12, 30, 0, 0, time.UTC)
	cursor := commitRSSBoundaryPoll(t, store, siteID, endpoint, []RSSItemInput{
		rssBoundaryPollItem("sha256:boundary-b", published),
		rssBoundaryPollItem("sha256:boundary-a", published),
	})
	assertRSSBoundaryHighWater(t, cursor, published, "sha256:boundary-b")

	cursor = commitRSSBoundaryPoll(t, store, siteID, endpoint, []RSSItemInput{
		rssBoundaryPollItem("sha256:boundary-c", published),
	})
	assertRSSBoundaryHighWater(t, cursor, published, "sha256:boundary-c")

	cursor = commitRSSBoundaryPoll(t, store, siteID, endpoint, []RSSItemInput{
		rssBoundaryPollItem("sha256:boundary-b2", published),
		rssBoundaryPollItem("sha256:boundary-z-old", published.Add(-time.Hour)),
	})
	assertRSSBoundaryHighWater(t, cursor, published, "sha256:boundary-c")
}

func TestRSSCleanupProcessedItemsFiltersAndLimitsPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-cleanup")
	otherSiteID := insertRSSIntegrationSite(t, store, "rss-cleanup-other")
	before := time.Date(2026, time.August, 12, 12, 30, 0, 0, time.UTC)
	old := before.Add(-time.Hour)
	recent := before.Add(time.Hour)

	matchedOldID := insertRSSBoundaryItem(t, store, siteID, "cleanup-matched-old", "matched", recent, &old)
	ignoredOldID := insertRSSBoundaryItem(t, store, siteID, "cleanup-ignored-old", "ignored", old, nil)
	insertRSSBoundaryItem(t, store, siteID, "cleanup-matched-boundary", "matched", old, &before)
	insertRSSBoundaryItem(t, store, siteID, "cleanup-matched-recent", "matched", old, &recent)
	insertRSSBoundaryItem(t, store, siteID, "cleanup-ignored-recent", "ignored", recent, nil)
	insertRSSBoundaryItem(t, store, siteID, "cleanup-pending-old", "pending", old, nil)
	insertRSSBoundaryItem(t, store, siteID, "cleanup-processing-old", "processing", old, nil)
	insertRSSBoundaryItem(t, store, siteID, "cleanup-failed-old", "failed", old, nil)
	otherSiteItemID := insertRSSBoundaryItem(t, store, otherSiteID, "cleanup-other-matched-old", "matched", old, &old)

	deleted, err := store.CleanupProcessedRSSItems(t.Context(), siteID, before, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("first CleanupProcessedRSSItems() = %d, error %v", deleted, err)
	}
	if rssBoundaryItemExists(t, store, matchedOldID) || !rssBoundaryItemExists(t, store, ignoredOldID) {
		t.Fatalf("first cleanup did not honor ID order and limit")
	}

	deleted, err = store.CleanupProcessedRSSItems(t.Context(), siteID, before, 10)
	if err != nil || deleted != 1 {
		t.Fatalf("second CleanupProcessedRSSItems() = %d, error %v", deleted, err)
	}
	if rssBoundaryItemExists(t, store, ignoredOldID) {
		t.Fatal("second cleanup retained eligible ignored item")
	}

	deleted, err = store.CleanupProcessedRSSItems(t.Context(), siteID, before, 10)
	if err != nil || deleted != 0 {
		t.Fatalf("final CleanupProcessedRSSItems() = %d, error %v", deleted, err)
	}
	for _, itemID := range []int64{otherSiteItemID} {
		if !rssBoundaryItemExists(t, store, itemID) {
			t.Fatalf("cleanup deleted protected item %d", itemID)
		}
	}

	var remainingTargetItems int
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM site_rss_items WHERE site_id=$1`, siteID).Scan(&remainingTargetItems); err != nil {
		t.Fatalf("count remaining RSS items: %v", err)
	}
	if remainingTargetItems != 6 {
		t.Fatalf("remaining target RSS items = %d, want 6", remainingTargetItems)
	}
}

func commitRSSBoundaryPoll(t *testing.T, store *Store, siteID int64, endpoint string, items []RSSItemInput) RSSCursor {
	t.Helper()
	setRSSCursorDue(t, store, siteID)
	now := time.Now().UTC()
	leased, acquired, err := store.TryAcquireRSSLease(t.Context(), siteID, endpoint, now, 5*time.Minute)
	if err != nil || !acquired {
		t.Fatalf("TryAcquireRSSLease() = acquired %v, error %v", acquired, err)
	}
	if err := store.CommitRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, RSSPollResult{ObservedAt: now, Items: items}, now.Add(time.Minute)); err != nil {
		t.Fatalf("CommitRSSPoll() error = %v", err)
	}
	cursor, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint)
	if err != nil {
		t.Fatalf("EnsureRSSCursor() after commit error = %v", err)
	}
	return cursor
}

func rssBoundaryPollItem(itemKey string, published time.Time) RSSItemInput {
	return RSSItemInput{
		ItemKey:     itemKey,
		Title:       itemKey,
		PublishedAt: &published,
		Resource:    json.RawMessage(`{}`),
	}
}

func assertRSSBoundaryHighWater(t *testing.T, cursor RSSCursor, published time.Time, itemKey string) {
	t.Helper()
	if cursor.HighWaterPublishedAt == nil || !cursor.HighWaterPublishedAt.Equal(published) || cursor.HighWaterItemKey != itemKey {
		t.Fatalf("cursor high water = (%v, %q), want (%v, %q)", cursor.HighWaterPublishedAt, cursor.HighWaterItemKey, published, itemKey)
	}
}

func insertRSSBoundaryItem(t *testing.T, store *Store, siteID int64, itemKey, state string, lastSeen time.Time, matchedAt *time.Time) int64 {
	t.Helper()
	var itemID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO site_rss_items (site_id, item_key, title, state, last_seen_at, matched_at)
		VALUES ($1, $2, $2, $3, $4, $5)
		RETURNING id`, siteID, itemKey, state, lastSeen, matchedAt).Scan(&itemID); err != nil {
		t.Fatalf("insert RSS boundary item %q: %v", itemKey, err)
	}
	return itemID
}

func rssBoundaryItemExists(t *testing.T, store *Store, itemID int64) bool {
	t.Helper()
	var exists bool
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM site_rss_items WHERE id=$1)`, itemID).Scan(&exists); err != nil {
		t.Fatalf("query RSS boundary item %d: %v", itemID, err)
	}
	return exists
}

func rssBoundaryUUIDPtr(value uuid.UUID) *uuid.UUID {
	return &value
}

func rssBoundaryTimePtr(value time.Time) *time.Time {
	return &value
}
