//go:build integration

package database

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestV2DownloadFailuresPostgresUpsertAndStrictExpiry(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	failure := v2DownloadFailureIntegrationFixture("a")
	failedAt := time.Date(2026, 8, 16, 8, 9, 10, 999_000_000, time.UTC)

	if err := store.RecordV2DownloadFailure(
		t.Context(), failure, failedAt, subscription.V2DownloadFailureTransientTTL,
	); err != nil {
		t.Fatalf("record first failure: %v", err)
	}
	assertV2DownloadFailureActive(t, store, failure.Fingerprint, failedAt, true)
	assertV2DownloadFailureActive(
		t, store, failure.Fingerprint, failedAt.Truncate(time.Second).Add(time.Hour), false,
	)

	failure.Episodes = "E02-E04"
	failure.ErrorMessage = "下载种子内容为空"
	secondAt := failedAt.Add(10 * time.Minute)
	if err := store.RecordV2DownloadFailure(
		t.Context(), failure, secondAt, subscription.V2DownloadFailureResourceTTL,
	); err != nil {
		t.Fatalf("record repeated failure: %v", err)
	}

	var (
		retryCount                         int
		episodes, errorMessage             string
		firstFailedAt, lastFailedAt, retry time.Time
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT retry_count, episodes, error_message,
		       first_failed_at, last_failed_at, next_retry_at
		FROM download_failures
		WHERE fingerprint=$1`, failure.Fingerprint).Scan(
		&retryCount, &episodes, &errorMessage, &firstFailedAt, &lastFailedAt, &retry,
	); err != nil {
		t.Fatalf("read repeated failure: %v", err)
	}
	if retryCount != 2 || episodes != failure.Episodes || errorMessage != failure.ErrorMessage {
		t.Fatalf("updated failure = retry:%d episodes:%q error:%q", retryCount, episodes, errorMessage)
	}
	if !firstFailedAt.Equal(failedAt.Truncate(time.Second)) ||
		!lastFailedAt.Equal(secondAt.Truncate(time.Second)) ||
		!retry.Equal(secondAt.Truncate(time.Second).Add(24*time.Hour)) {
		t.Fatalf("failure timestamps = first:%v last:%v retry:%v", firstFailedAt, lastFailedAt, retry)
	}

	active, err := store.ListActiveV2DownloadFailureFingerprints(
		t.Context(),
		[]string{failure.Fingerprint, failure.Fingerprint, strings.Repeat("b", 64)},
		secondAt,
	)
	if err != nil || len(active) != 1 {
		t.Fatalf("global deduplicated active set = %#v, %v", active, err)
	}
}

func TestV2DownloadFailuresPostgresConcurrentUpsert(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	failure := v2DownloadFailureIntegrationFixture("c")
	failedAt := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)

	const total = 24
	start := make(chan struct{})
	var group errgroup.Group
	for range total {
		group.Go(func() error {
			<-start
			return store.RecordV2DownloadFailure(
				t.Context(), failure, failedAt, subscription.V2DownloadFailureTransientTTL,
			)
		})
	}
	close(start)
	if err := group.Wait(); err != nil {
		t.Fatalf("concurrent record failure: %v", err)
	}

	var count, retries int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT COUNT(*), MAX(retry_count)
		FROM download_failures
		WHERE fingerprint=$1`, failure.Fingerprint).Scan(&count, &retries); err != nil {
		t.Fatalf("read concurrent failure: %v", err)
	}
	if count != 1 || retries != total {
		t.Fatalf("concurrent failure rows/retries = %d/%d, want 1/%d", count, retries, total)
	}
}

func TestV2DownloadFailuresPostgresErrorsAreSanitized(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	failure := v2DownloadFailureIntegrationFixture("d")
	failure.Source = "Subscribe|private-source-must-not-appear"
	failure.ErrorMessage = "private-error-must-not-appear"

	if _, err := store.db.Pool.Exec(t.Context(), `ALTER TABLE download_failures RENAME TO download_failures_fault_injected`); err != nil {
		t.Fatalf("inject download failure table fault: %v", err)
	}
	err := store.RecordV2DownloadFailure(
		t.Context(), failure, time.Now(), subscription.V2DownloadFailureTransientTTL,
	)
	if !errors.Is(err, ErrV2DownloadFailurePersistence) {
		t.Fatalf("record persistence error = %v", err)
	}
	if strings.Contains(err.Error(), failure.Source) || strings.Contains(err.Error(), failure.ErrorMessage) ||
		strings.Contains(err.Error(), "download_failures") {
		t.Fatalf("record error leaked persistence details: %v", err)
	}
	_, err = store.ListActiveV2DownloadFailureFingerprints(
		t.Context(), []string{failure.Fingerprint}, time.Now(),
	)
	if !errors.Is(err, ErrV2DownloadFailurePersistence) {
		t.Fatalf("list persistence error = %v", err)
	}
}

func v2DownloadFailureIntegrationFixture(character string) subscription.V2DownloadFailure {
	return subscription.V2DownloadFailure{
		Fingerprint:  strings.Repeat(character, 64),
		MediaType:    "电视剧",
		Title:        "Fixture Show",
		Year:         "2026",
		MediaSource:  "themoviedb",
		MediaID:      "42",
		Season:       "S01",
		Episodes:     "E01",
		SiteID:       7,
		SiteName:     "Fixture",
		ResourceKey:  "torrent-42",
		TorrentName:  "Fixture.Show.S01E01",
		TorrentSize:  1024,
		Downloader:   "qB",
		Source:       "Subscribe|fixture",
		ErrorMessage: "temporary failure",
	}
}

func assertV2DownloadFailureActive(
	t *testing.T,
	store *Store,
	fingerprint string,
	at time.Time,
	want bool,
) {
	t.Helper()
	active, err := store.ListActiveV2DownloadFailureFingerprints(
		t.Context(), []string{fingerprint}, at,
	)
	if err != nil {
		t.Fatalf("list active failures: %v", err)
	}
	_, got := active[fingerprint]
	if got != want {
		t.Fatalf("active at %v = %t, want %t", at, got, want)
	}
}
