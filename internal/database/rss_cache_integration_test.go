//go:build integration

package database

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

func TestRSSCommitUsesObservedAtAndPreservesExistingPayloadPostgres(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	secretBox, err := auth.NewSecretBox("rss-cache-immutable-secret-master-key")
	if err != nil {
		t.Fatalf("创建 RSS 缓存 SecretBox: %v", err)
	}
	store := NewStore(baseStore.db, secretBox)
	siteID := insertRSSIntegrationSite(t, store, "rss-cache-immutable")
	const endpoint = "sha256:rss-cache-immutable"
	if _, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}

	firstObservedAt := time.Date(2026, time.August, 16, 8, 0, 0, 0, time.UTC)
	secondObservedAt := firstObservedAt.Add(5 * time.Minute)
	firstPublishedAt := firstObservedAt.Add(-time.Hour)
	secondPublishedAt := firstPublishedAt.Add(time.Hour)
	const itemKey = "sha256:rss-cache-immutable-item"
	first := RSSItemInput{
		ItemKey:        itemKey,
		GUID:           "immutable-guid",
		DownloadID:     "immutable-download-id",
		DetailURL:      "https://tracker.example/details/immutable",
		DownloadURL:    "https://tracker.example/download/immutable",
		ResourceSecret: "https://tracker.example/download/immutable?passkey=first-secret",
		Title:          "Immutable Release",
		PublishedAt:    &firstPublishedAt,
		Resource:       json.RawMessage(`{"seeders":11,"title":"Immutable Release"}`),
	}
	commitRSSCachePoll(t, store, siteID, endpoint, firstObservedAt, []RSSItemInput{first})
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE site_rss_items SET state='ignored', last_error='original decision'
		WHERE site_id=$1 AND item_key=$2`, siteID, itemKey); err != nil {
		t.Fatalf("prepare existing RSS decision: %v", err)
	}

	replayed := RSSItemInput{
		ItemKey:        itemKey,
		GUID:           "replacement-guid",
		DownloadID:     "replacement-download-id",
		DetailURL:      "https://tracker.example/details/replacement",
		DownloadURL:    "https://tracker.example/download/replacement",
		ResourceSecret: "https://tracker.example/download/replacement?passkey=second-secret",
		Title:          "Replacement Release",
		PublishedAt:    &secondPublishedAt,
		Resource:       json.RawMessage(`{"seeders":99,"title":"Replacement Release"}`),
	}
	commitRSSCachePoll(t, store, siteID, endpoint, secondObservedAt, []RSSItemInput{replayed})

	items, err := store.ListRSSCacheSnapshot(t.Context(), siteID, secondObservedAt)
	if err != nil {
		t.Fatalf("ListRSSCacheSnapshot() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("ListRSSCacheSnapshot() length = %d, want 1", len(items))
	}
	got := items[0]
	if !got.FirstSeenAt.Equal(firstObservedAt) || !got.LastSeenAt.Equal(secondObservedAt) {
		t.Fatalf("RSS seen timestamps = %s/%s, want %s/%s", got.FirstSeenAt, got.LastSeenAt, firstObservedAt, secondObservedAt)
	}
	if got.GUID != first.GUID || got.DownloadID != first.DownloadID || got.DetailURL != first.DetailURL ||
		got.DownloadURL != first.DownloadURL || got.ResourceSecret != first.ResourceSecret || got.Title != first.Title ||
		got.PublishedAt == nil || !got.PublishedAt.Equal(firstPublishedAt) || !rssCacheJSONEqual(got.Resource, first.Resource) {
		t.Fatalf("replayed RSS item overwrote immutable payload: %#v", got)
	}
	if got.State != "ignored" || got.LastError != "original decision" {
		t.Fatalf("replayed RSS item overwrote decision state: %#v", got)
	}
}

func TestRSSCacheSnapshotIncludesOldStatesAndHonorsCutoffPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-cache-snapshot")
	otherSiteID := insertRSSIntegrationSite(t, store, "rss-cache-snapshot-other")
	cutoff := time.Date(2026, time.August, 16, 9, 0, 0, 0, time.UTC)
	tiedPublishedAt := cutoff.Add(-2 * time.Hour)
	newestPublishedAt := cutoff.Add(-time.Hour)
	oldPublishedAt := cutoff.Add(-3 * time.Hour)
	firstSeenAt := cutoff.Add(-10 * time.Minute)

	insertRSSCacheItem(t, store, siteID, "snapshot-matched", "matched", &tiedPublishedAt, firstSeenAt, cutoff.Add(time.Hour))
	insertRSSCacheItem(t, store, siteID, "snapshot-processing", "processing", &tiedPublishedAt, firstSeenAt, cutoff.Add(time.Hour))
	insertRSSCacheItem(t, store, siteID, "snapshot-pending", "pending", &newestPublishedAt, firstSeenAt, firstSeenAt)
	insertRSSCacheItem(t, store, siteID, "snapshot-failed", "failed", &oldPublishedAt, firstSeenAt, firstSeenAt)
	insertRSSCacheItem(t, store, siteID, "snapshot-ignored", "ignored", nil, firstSeenAt, cutoff.Add(time.Hour))
	insertRSSCacheItem(t, store, siteID, "snapshot-after-cutoff", "pending", &newestPublishedAt, cutoff.Add(time.Second), cutoff.Add(time.Second))
	insertRSSCacheItem(t, store, otherSiteID, "snapshot-other-site", "pending", &newestPublishedAt, firstSeenAt, firstSeenAt)

	items, err := store.ListRSSCacheSnapshot(t.Context(), siteID, cutoff)
	if err != nil {
		t.Fatalf("ListRSSCacheSnapshot() error = %v", err)
	}
	wantKeys := []string{
		"snapshot-matched",
		"snapshot-processing",
		"snapshot-pending",
		"snapshot-failed",
		"snapshot-ignored",
	}
	if len(items) != len(wantKeys) {
		t.Fatalf("ListRSSCacheSnapshot() = %#v, want keys %v", items, wantKeys)
	}
	for i, wantKey := range wantKeys {
		if items[i].ItemKey != wantKey {
			t.Fatalf("snapshot[%d].ItemKey = %q, want %q; items = %#v", i, items[i].ItemKey, wantKey, items)
		}
	}
}

func TestRSSCacheSnapshotAndTrimPreferNewDiscoveryOverPublishedAtPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-cache-discovery-order")
	cutoff := time.Date(2026, time.August, 16, 10, 30, 0, 0, time.UTC)
	oldDiscovery := cutoff.Add(-time.Hour)
	newDiscovery := cutoff.Add(-time.Minute)
	newerPublishedAt := cutoff.Add(-time.Minute)
	oldPublishedAt := cutoff.Add(-365 * 24 * time.Hour)

	for index := range maxRSSCacheItems {
		insertRSSCacheItem(
			t, store, siteID, fmt.Sprintf("discovery-old-%03d", index), "pending",
			&newerPublishedAt, oldDiscovery.Add(time.Duration(index)*time.Millisecond), oldDiscovery,
		)
	}
	newOldDateID := insertRSSCacheItem(
		t, store, siteID, "discovery-new-old-date", "pending", &oldPublishedAt, newDiscovery, newDiscovery,
	)
	newNoDateID := insertRSSCacheItem(
		t, store, siteID, "discovery-new-no-date", "pending", nil, newDiscovery, newDiscovery,
	)

	items, err := store.ListRSSCacheSnapshot(t.Context(), siteID, cutoff)
	if err != nil {
		t.Fatalf("ListRSSCacheSnapshot() error = %v", err)
	}
	if len(items) != maxRSSCacheItems || items[0].ItemKey != "discovery-old-002" ||
		items[len(items)-2].ID != newOldDateID || items[len(items)-1].ID != newNoDateID {
		t.Fatalf("V2 append ordered snapshot boundary = first:%#v last:%#v", items[0], items[len(items)-2:])
	}

	deleted, err := store.TrimRSSCacheItems(t.Context(), siteID, maxRSSCacheItems)
	if err != nil || deleted != 2 {
		t.Fatalf("TrimRSSCacheItems() = %d, %v; want 2, nil", deleted, err)
	}
	if !rssBoundaryItemExists(t, store, newOldDateID) || !rssBoundaryItemExists(t, store, newNoDateID) {
		t.Fatal("trim removed newly discovered item because of old or empty published_at")
	}
}

func TestRSSPollRoundReadyRequiresEveryCursorToAdvanceAndReleaseLeasePostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	cutoff := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	readySiteID := insertRSSIntegrationSite(t, store, "rss-round-ready")
	dueSiteID := insertRSSIntegrationSite(t, store, "rss-round-due")
	leasedSiteID := insertRSSIntegrationSite(t, store, "rss-round-leased")
	fixtures := []struct {
		siteID         int64
		endpoint       string
		nextPollAt     time.Time
		leaseToken     *uuid.UUID
		leaseExpiresAt *time.Time
	}{
		{siteID: readySiteID, endpoint: "sha256:rss-round-ready", nextPollAt: cutoff.Add(time.Minute)},
		{siteID: dueSiteID, endpoint: "sha256:rss-round-due", nextPollAt: cutoff},
		{
			siteID: leasedSiteID, endpoint: "sha256:rss-round-leased", nextPollAt: cutoff.Add(time.Minute),
			leaseToken: rssBoundaryUUIDPtr(uuid.New()), leaseExpiresAt: rssBoundaryTimePtr(cutoff.Add(time.Hour)),
		},
	}
	for _, fixture := range fixtures {
		if _, err := store.EnsureRSSCursor(t.Context(), fixture.siteID, fixture.endpoint); err != nil {
			t.Fatalf("EnsureRSSCursor(%d) error = %v", fixture.siteID, err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE site_rss_cursors
			SET next_poll_at=$2, lease_token=$3, lease_expires_at=$4
			WHERE site_id=$1`, fixture.siteID, fixture.nextPollAt, fixture.leaseToken, fixture.leaseExpiresAt); err != nil {
			t.Fatalf("prepare RSS round cursor %d: %v", fixture.siteID, err)
		}
	}

	ready, err := store.RSSPollRoundReady(t.Context(), cutoff, nil)
	if err != nil || !ready {
		t.Fatalf("RSSPollRoundReady(empty) = %v, %v; want true, nil", ready, err)
	}
	expectedSiteIDs := []int64{leasedSiteID, readySiteID, dueSiteID, readySiteID}
	ready, err = store.RSSPollRoundReady(t.Context(), cutoff, expectedSiteIDs)
	if err != nil || ready {
		t.Fatalf("RSSPollRoundReady(incomplete) = %v, %v; want false, nil", ready, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE site_rss_cursors SET next_poll_at=$2 WHERE site_id=$1`, dueSiteID, cutoff.Add(time.Minute)); err != nil {
		t.Fatalf("advance due RSS cursor: %v", err)
	}
	ready, err = store.RSSPollRoundReady(t.Context(), cutoff, expectedSiteIDs)
	if err != nil || ready {
		t.Fatalf("RSSPollRoundReady(leased) = %v, %v; want false, nil", ready, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE site_rss_cursors SET lease_token=NULL, lease_expires_at=NULL WHERE site_id=$1`, leasedSiteID); err != nil {
		t.Fatalf("release leased RSS cursor: %v", err)
	}
	ready, err = store.RSSPollRoundReady(t.Context(), cutoff, expectedSiteIDs)
	if err != nil || !ready {
		t.Fatalf("RSSPollRoundReady(complete) = %v, %v; want true, nil", ready, err)
	}
	ready, err = store.RSSPollRoundReady(t.Context(), cutoff, append(expectedSiteIDs, 999999999))
	if err != nil || ready {
		t.Fatalf("RSSPollRoundReady(missing cursor) = %v, %v; want false, nil", ready, err)
	}
}

func TestRSSCacheSnapshotAndTrimUseStableHundredItemBoundaryPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-cache-trim")
	otherSiteID := insertRSSIntegrationSite(t, store, "rss-cache-trim-other")
	cutoff := time.Date(2026, time.August, 16, 11, 0, 0, 0, time.UTC)
	publishedAt := cutoff.Add(-time.Hour)
	firstSeenAt := cutoff.Add(-time.Minute)
	itemIDs := make([]int64, maxRSSCacheItems+2)
	for i := range itemIDs {
		itemIDs[i] = insertRSSCacheItem(
			t, store, siteID, fmt.Sprintf("trim-item-%03d", i), "pending", &publishedAt, firstSeenAt, firstSeenAt,
		)
	}
	otherItemID := insertRSSCacheItem(t, store, otherSiteID, "trim-other-site", "pending", &publishedAt, firstSeenAt, firstSeenAt)

	items, err := store.ListRSSCacheSnapshot(t.Context(), siteID, cutoff)
	if err != nil {
		t.Fatalf("ListRSSCacheSnapshot() error = %v", err)
	}
	if len(items) != maxRSSCacheItems || items[0].ItemKey != "trim-item-002" || items[len(items)-1].ItemKey != "trim-item-101" {
		t.Fatalf("snapshot boundary = len %d, first %q, last %q", len(items), items[0].ItemKey, items[len(items)-1].ItemKey)
	}

	deleted, err := store.TrimRSSCacheItems(t.Context(), siteID, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("first TrimRSSCacheItems() = %d, %v; want 1, nil", deleted, err)
	}
	if rssBoundaryItemExists(t, store, itemIDs[0]) || !rssBoundaryItemExists(t, store, itemIDs[1]) {
		t.Fatal("first trim did not delete only the oldest stable tie")
	}
	deleted, err = store.TrimRSSCacheItems(t.Context(), siteID, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("second TrimRSSCacheItems() = %d, %v; want 1, nil", deleted, err)
	}
	deleted, err = store.TrimRSSCacheItems(t.Context(), siteID, 1)
	if err != nil || deleted != 0 {
		t.Fatalf("final TrimRSSCacheItems() = %d, %v; want 0, nil", deleted, err)
	}
	if rssBoundaryItemExists(t, store, itemIDs[1]) || !rssBoundaryItemExists(t, store, itemIDs[2]) || !rssBoundaryItemExists(t, store, otherItemID) {
		t.Fatal("trim crossed the stable 100-item or site boundary")
	}
	if count := countRSSCacheItems(t, store, siteID); count != maxRSSCacheItems {
		t.Fatalf("trimmed RSS cache count = %d, want %d", count, maxRSSCacheItems)
	}
}

func TestRSSMediaRecognitionFieldsPersistAcrossPollReplayPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-media-recognition")
	endpoint := "sha256:rss-media-recognition"
	if _, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	item := RSSItemInput{
		ItemKey: "sha256:rss-media-recognition-item", Title: "Release.S01E01",
		DownloadID: "download-rss-media-recognition", Resource: json.RawMessage(`{"title":"Release.S01E01"}`),
		MediaRecognizeFailCount: 1,
	}
	commitRSSCachePoll(t, store, siteID, endpoint, now, []RSSItemInput{item})
	var failCount int
	var title string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT media_recognize_fail_count, recognized_media_title
		FROM site_rss_items WHERE site_id=$1 AND item_key=$2`, siteID, item.ItemKey).Scan(&failCount, &title); err != nil {
		t.Fatalf("read initial recognition fields: %v", err)
	}
	if failCount != 1 || title != "" {
		t.Fatalf("initial recognition fields = (%d, %q), want (1, empty)", failCount, title)
	}
	// Re-seeing the same feed item must not consume another recognition
	// attempt; only match-time retries advance the durable counter.
	commitRSSCachePoll(t, store, siteID, endpoint, now.Add(time.Minute), []RSSItemInput{item})
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT media_recognize_fail_count, recognized_media_title
		FROM site_rss_items WHERE site_id=$1 AND item_key=$2`, siteID, item.ItemKey).Scan(&failCount, &title); err != nil {
		t.Fatalf("read duplicate recognition fields: %v", err)
	}
	if failCount != 1 || title != "" {
		t.Fatalf("duplicate recognition fields = (%d, %q), want (1, empty)", failCount, title)
	}
	var itemID int64
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT id FROM site_rss_items WHERE site_id=$1 AND item_key=$2`, siteID, item.ItemKey).Scan(&itemID); err != nil {
		t.Fatalf("read RSS item id: %v", err)
	}
	if err := store.UpdateRSSItemMediaRecognition(t.Context(), itemID, "Canonical Show"); err != nil {
		t.Fatalf("UpdateRSSItemMediaRecognition(success) error = %v", err)
	}
	commitRSSCachePoll(t, store, siteID, endpoint, now.Add(2*time.Minute), []RSSItemInput{item})
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT media_recognize_fail_count, recognized_media_title
		FROM site_rss_items WHERE site_id=$1 AND item_key=$2`, siteID, item.ItemKey).Scan(&failCount, &title); err != nil {
		t.Fatalf("read replay recognition fields: %v", err)
	}
	if failCount != 0 || title != "Canonical Show" {
		t.Fatalf("replay recognition fields = (%d, %q), want (0, Canonical Show)", failCount, title)
	}
}

func TestRSSReplaceCachePreservesEmptyAndProcessingSemanticsPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-replace-cache")
	endpoint := "sha256:rss-replace-cache"
	if _, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 20, 13, 0, 0, 0, time.UTC)
	old := RSSItemInput{ItemKey: "sha256:rss-replace-old", Title: "Old", DownloadID: "old", Resource: json.RawMessage(`{"title":"Old"}`)}
	commitRSSCachePoll(t, store, siteID, endpoint, now, []RSSItemInput{old})
	processingID := insertRSSCacheItem(t, store, siteID, "sha256:rss-replace-processing-old", "processing", nil, now, now)
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE site_rss_items SET lease_token=$2, lease_expires_at=now()+interval '1 minute' WHERE id=$1`, processingID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	fresh := RSSItemInput{ItemKey: "sha256:rss-replace-processing", Title: "Fresh", DownloadID: "fresh", Resource: json.RawMessage(`{"title":"Fresh"}`)}
	commitRSSCachePoll(t, store, siteID, endpoint, now.Add(time.Minute), []RSSItemInput{fresh}, true)
	var oldCount, freshCount, processingCount int
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE item_key=$2), count(*) FILTER (WHERE item_key=$3), count(*) FILTER (WHERE item_key=$4) FROM site_rss_items WHERE site_id=$1`, siteID, old.ItemKey, fresh.ItemKey, "sha256:rss-replace-processing-old").Scan(&oldCount, &freshCount, &processingCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 0 || freshCount != 1 || processingCount != 0 {
		t.Fatalf("replace counts old/fresh/processing = %d/%d/%d", oldCount, freshCount, processingCount)
	}
	var title, state string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT title, state FROM site_rss_items
		WHERE site_id=$1 AND item_key=$2`, siteID, fresh.ItemKey).Scan(&title, &state); err != nil {
		t.Fatal(err)
	}
	if title != "Fresh" || state != "pending" {
		t.Fatalf("processing replacement payload = (%q, %q), want (Fresh, pending)", title, state)
	}
	commitRSSCachePoll(t, store, siteID, endpoint, now.Add(2*time.Minute), nil, false)
	if countRSSCacheItems(t, store, siteID) != 1 {
		t.Fatal("empty RSS response unexpectedly replaced cache")
	}
}

func commitRSSCachePoll(
	t *testing.T,
	store *Store,
	siteID int64,
	endpoint string,
	observedAt time.Time,
	items []RSSItemInput,
	replace ...bool,
) {
	t.Helper()
	setRSSCursorDue(t, store, siteID)
	now := time.Now().UTC()
	leased, acquired, err := store.TryAcquireRSSLease(t.Context(), siteID, endpoint, now, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("TryAcquireRSSLease() = acquired %v, error %v", acquired, err)
	}
	nextPollAt := now.Add(time.Minute)
	if !nextPollAt.After(observedAt) {
		nextPollAt = observedAt.Add(time.Minute)
	}
	if err := store.CommitRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, RSSPollResult{
		ObservedAt:   observedAt,
		ReplaceCache: len(replace) > 0 && replace[0],
		Items:        items,
	}, nextPollAt); err != nil {
		t.Fatalf("CommitRSSPoll() error = %v", err)
	}
}

func insertRSSCacheItem(
	t *testing.T,
	store *Store,
	siteID int64,
	itemKey string,
	state string,
	publishedAt *time.Time,
	firstSeenAt time.Time,
	lastSeenAt time.Time,
) int64 {
	t.Helper()
	var itemID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO site_rss_items
			(site_id, item_key, title, published_at, resource, first_seen_at, last_seen_at, state)
		VALUES ($1, $2, $2, $3, '{}'::jsonb, $4, $5, $6)
		RETURNING id`, siteID, itemKey, publishedAt, firstSeenAt, lastSeenAt, state).Scan(&itemID); err != nil {
		t.Fatalf("insert RSS cache item %q: %v", itemKey, err)
	}
	return itemID
}

func countRSSCacheItems(t *testing.T, store *Store, siteID int64) int {
	t.Helper()
	var count int
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM site_rss_items WHERE site_id=$1`, siteID).Scan(&count); err != nil {
		t.Fatalf("count RSS cache items for site %d: %v", siteID, err)
	}
	return count
}

func rssCacheJSONEqual(left json.RawMessage, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
