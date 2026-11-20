//go:build integration

package database

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

func TestRSSCursorLeaseAndItemsPostgres(t *testing.T) {
	baseStore := newSiteStatisticsIntegrationStore(t)
	secretBox, err := auth.NewSecretBox("rss-integration-secret-master-key")
	if err != nil {
		t.Fatalf("创建 RSS 集成测试 SecretBox: %v", err)
	}
	store := NewStore(baseStore.db, secretBox)
	siteID := insertRSSIntegrationSite(t, store, "rss-main")
	const endpoint = "https://tracker.example/rss"

	cursor, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint)
	if err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}
	if cursor.SiteID != siteID || cursor.Endpoint != endpoint || cursor.LeaseToken != uuid.Nil {
		t.Fatalf("EnsureRSSCursor() = %#v", cursor)
	}

	due, err := store.ListDueRSSCursors(t.Context(), time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ListDueRSSCursors() error = %v", err)
	}
	if len(due) != 1 || due[0].SiteID != siteID {
		t.Fatalf("ListDueRSSCursors() = %#v", due)
	}

	now := time.Now().UTC()
	leased, acquired, err := store.TryAcquireRSSLease(t.Context(), siteID, endpoint, now, time.Minute)
	if err != nil || !acquired || leased.LeaseToken == uuid.Nil || leased.LeaseExpiresAt == nil {
		t.Fatalf("TryAcquireRSSLease() = cursor %#v, acquired %v, error %v", leased, acquired, err)
	}
	preserved, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint)
	if err != nil {
		t.Fatalf("EnsureRSSCursor() preserving lease error = %v", err)
	}
	if preserved.LeaseToken != leased.LeaseToken {
		t.Fatalf("EnsureRSSCursor() lease token = %s, want %s", preserved.LeaseToken, leased.LeaseToken)
	}
	if _, acquired, err := store.TryAcquireRSSLease(t.Context(), siteID, endpoint, now, time.Minute); acquired || !errors.Is(err, ErrRSSLeaseConflict) {
		t.Fatalf("competing TryAcquireRSSLease() = acquired %v, error %v", acquired, err)
	}

	published := time.Date(2026, time.August, 12, 12, 30, 0, 0, time.UTC)
	const resourceSecret = "https://tracker.example/download/1?passkey=rss-fixture"
	item := RSSItemInput{
		ItemKey:        "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		GUID:           "fixture-1",
		DownloadID:     "https://tracker.example/download/1",
		DetailURL:      "https://tracker.example/details/1",
		DownloadURL:    "https://tracker.example/download/1",
		ResourceSecret: resourceSecret,
		Title:          "Fixture Release",
		PublishedAt:    &published,
		Resource:       json.RawMessage(`{"title":"Fixture Release","seeders":10}`),
	}
	nextPoll := now.Add(time.Minute)
	if err := store.CommitRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, RSSPollResult{
		ETag: `"rss-v1"`, LastModified: "Wed, 12 Aug 2026 12:30:00 GMT", ObservedAt: now, Items: []RSSItemInput{item},
	}, nextPoll); err != nil {
		t.Fatalf("CommitRSSPoll() error = %v", err)
	}
	afterCommit, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint)
	if err != nil {
		t.Fatalf("EnsureRSSCursor() after commit error = %v", err)
	}
	if afterCommit.LeaseToken != uuid.Nil || afterCommit.ETag != `"rss-v1"` || afterCommit.FailureCount != 0 || afterCommit.HighWaterPublishedAt == nil {
		t.Fatalf("cursor after commit = %#v", afterCommit)
	}
	if err := store.CommitRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, RSSPollResult{ObservedAt: now, Items: []RSSItemInput{item}}, nextPoll); !errors.Is(err, ErrRSSLeaseConflict) {
		t.Fatalf("stale CommitRSSPoll() error = %v, want ErrRSSLeaseConflict", err)
	}

	pending, err := store.ListPendingRSSItems(t.Context(), siteID, 10)
	if err != nil {
		t.Fatalf("ListPendingRSSItems() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ItemKey != item.ItemKey || pending[0].ResourceSecret != resourceSecret || pending[0].LeaseToken != uuid.Nil {
		t.Fatalf("ListPendingRSSItems() = %#v", pending)
	}
	var storedSecret string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT download_secret FROM site_rss_items WHERE site_id=$1 AND item_key=$2`,
		siteID, item.ItemKey,
	).Scan(&storedSecret); err != nil {
		t.Fatalf("查询 RSS locator 密文: %v", err)
	}
	if storedSecret == resourceSecret || !strings.HasPrefix(storedSecret, "v1:") || strings.Contains(storedSecret, "rss-fixture") {
		t.Fatalf("RSS locator 未安全持久化: %q", storedSecret)
	}
	claimed, err := store.ClaimRSSItems(t.Context(), siteID, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimRSSItems() error = %v", err)
	}
	if len(claimed) != 1 || claimed[0].State != "processing" || claimed[0].ResourceSecret != resourceSecret || claimed[0].LeaseToken == uuid.Nil || claimed[0].AttemptCount != 1 {
		t.Fatalf("ClaimRSSItems() = %#v", claimed)
	}
	if second, err := store.ClaimRSSItems(t.Context(), siteID, 10, time.Minute); err != nil || len(second) != 0 {
		t.Fatalf("second ClaimRSSItems() = %#v, error %v", second, err)
	}
	if err := store.CompleteRSSItem(t.Context(), claimed[0].ID, claimed[0].LeaseToken, "matched", ""); err != nil {
		t.Fatalf("CompleteRSSItem() error = %v", err)
	}
	if err := store.CompleteRSSItem(t.Context(), claimed[0].ID, claimed[0].LeaseToken, "matched", ""); !errors.Is(err, ErrRSSLeaseConflict) {
		t.Fatalf("stale CompleteRSSItem() error = %v, want ErrRSSLeaseConflict", err)
	}

	setRSSCursorDue(t, store, siteID)
	now = time.Now().UTC()
	leased, acquired, err = store.TryAcquireRSSLease(t.Context(), siteID, endpoint, now, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("reacquire RSS cursor = acquired %v, error %v", acquired, err)
	}
	if err := store.CommitRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, RSSPollResult{
		NotModified: true, ObservedAt: now,
	}, now.Add(time.Minute)); err != nil {
		t.Fatalf("304 CommitRSSPoll() error = %v", err)
	}
	afterNotModified, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint)
	if err != nil {
		t.Fatalf("EnsureRSSCursor() after 304 error = %v", err)
	}
	if afterNotModified.ETag != `"rss-v1"` {
		t.Fatalf("304 cleared ETag: got %q", afterNotModified.ETag)
	}

	var count int
	var state string
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT count(*), min(state) FROM site_rss_items WHERE site_id=$1 AND item_key=$2`, siteID, item.ItemKey).Scan(&count, &state); err != nil {
		t.Fatalf("query RSS item after replay: %v", err)
	}
	if count != 1 || state != "matched" {
		t.Fatalf("RSS replay count/state = %d/%q, want 1/matched", count, state)
	}
}

func TestRSSFailAndExpiredItemLeasePostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-recovery")
	const endpoint = "https://tracker.example/recovery-rss"
	if _, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}
	now := time.Now().UTC()
	leased, acquired, err := store.TryAcquireRSSLease(t.Context(), siteID, endpoint, now, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("TryAcquireRSSLease() = acquired %v, error %v", acquired, err)
	}
	if err := store.FailRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, now.Add(time.Minute), "temporary failure"); err != nil {
		t.Fatalf("FailRSSPoll() error = %v", err)
	}
	if err := store.FailRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, now.Add(time.Minute), "stale"); !errors.Is(err, ErrRSSLeaseConflict) {
		t.Fatalf("stale FailRSSPoll() error = %v, want ErrRSSLeaseConflict", err)
	}
	cursor, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint)
	if err != nil {
		t.Fatalf("EnsureRSSCursor() after failure error = %v", err)
	}
	if cursor.FailureCount != 1 || cursor.LastError != "temporary failure" || cursor.LeaseToken != uuid.Nil {
		t.Fatalf("cursor after failure = %#v", cursor)
	}

	insertRSSPendingIntegrationItem(t, store, siteID, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	claimed, err := store.ClaimRSSItems(t.Context(), siteID, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimRSSItems() = %#v, error %v", claimed, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE site_rss_items SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, claimed[0].ID); err != nil {
		t.Fatalf("expire item lease: %v", err)
	}
	recovered, err := store.RecoverExpiredRSSLeases(t.Context(), time.Now().UTC(), 10)
	if err != nil || recovered != 1 {
		t.Fatalf("RecoverExpiredRSSLeases() = %d, error %v", recovered, err)
	}
	reclaimed, err := store.ClaimRSSItems(t.Context(), siteID, 1, time.Minute)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].AttemptCount != 2 || reclaimed[0].LeaseToken == claimed[0].LeaseToken {
		t.Fatalf("reclaimed RSS items = %#v, error %v", reclaimed, err)
	}
}

func TestRSSCommitPostgresRollsBackItemsWhenCursorCASExpires(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	siteID := insertRSSIntegrationSite(t, store, "rss-rollback")
	const endpoint = "https://tracker.example/rollback-rss"
	if _, err := store.EnsureRSSCursor(t.Context(), siteID, endpoint); err != nil {
		t.Fatalf("EnsureRSSCursor() error = %v", err)
	}
	leased, acquired, err := store.TryAcquireRSSLease(t.Context(), siteID, endpoint, time.Now().UTC(), time.Minute)
	if err != nil || !acquired {
		t.Fatalf("TryAcquireRSSLease() = acquired %v, error %v", acquired, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		CREATE FUNCTION expire_rss_cursor_lease() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			UPDATE site_rss_cursors SET lease_expires_at=now()-interval '1 second' WHERE site_id=NEW.site_id;
			RETURN NEW;
		END $$;
		CREATE TRIGGER expire_rss_cursor_lease_trigger
		AFTER INSERT ON site_rss_items
		FOR EACH ROW EXECUTE FUNCTION expire_rss_cursor_lease()`); err != nil {
		t.Fatalf("create RSS rollback trigger: %v", err)
	}
	itemKey := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	observedAt := time.Now().UTC()
	err = store.CommitRSSPoll(t.Context(), siteID, endpoint, leased.LeaseToken, RSSPollResult{ObservedAt: observedAt, Items: []RSSItemInput{{
		ItemKey:  itemKey,
		Title:    "Rollback Fixture",
		Resource: json.RawMessage(`{"title":"Rollback Fixture"}`),
	}}}, observedAt.Add(time.Minute))
	if !errors.Is(err, ErrRSSLeaseConflict) {
		t.Fatalf("CommitRSSPoll() error = %v, want ErrRSSLeaseConflict", err)
	}
	var itemCount int
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM site_rss_items WHERE site_id=$1 AND item_key=$2`, siteID, itemKey).Scan(&itemCount); err != nil {
		t.Fatalf("query rolled back RSS item: %v", err)
	}
	if itemCount != 0 {
		t.Fatalf("rolled back RSS item count = %d, want 0", itemCount)
	}
	var token *uuid.UUID
	var leaseValid bool
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT lease_token, lease_expires_at > now() FROM site_rss_cursors WHERE site_id=$1`, siteID).Scan(&token, &leaseValid); err != nil {
		t.Fatalf("query cursor after rollback: %v", err)
	}
	if token == nil || *token != leased.LeaseToken || !leaseValid {
		t.Fatalf("cursor lease did not roll back: token=%v valid=%v", token, leaseValid)
	}
}

func insertRSSIntegrationSite(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	var siteID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, url, adapter, enabled, config)
		VALUES ($1, 'https://tracker.example', 'declarative', TRUE, '{"format":"rss"}'::jsonb)
		RETURNING id`, name).Scan(&siteID); err != nil {
		t.Fatalf("insert RSS integration site: %v", err)
	}
	return siteID
}

func insertRSSPendingIntegrationItem(t *testing.T, store *Store, siteID int64, itemKey string) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO site_rss_items (site_id, item_key, title, resource)
		VALUES ($1, $2, 'Recovery Fixture', '{"title":"Recovery Fixture"}'::jsonb)`, siteID, itemKey); err != nil {
		t.Fatalf("insert pending RSS integration item: %v", err)
	}
}

func setRSSCursorDue(t *testing.T, store *Store, siteID int64) {
	t.Helper()
	if _, err := store.db.Pool.Exec(t.Context(), `UPDATE site_rss_cursors SET next_poll_at=now()-interval '1 second' WHERE site_id=$1`, siteID); err != nil {
		t.Fatalf("set RSS cursor due: %v", err)
	}
}
