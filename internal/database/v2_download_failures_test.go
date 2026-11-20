package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestV2DownloadFailuresMigrationContract(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/033_v2_download_failures.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.Join(strings.Fields(string(content)), " ")
	for _, required := range []string{
		"CREATE TABLE download_failures",
		"fingerprint CHAR(64) NOT NULL",
		"CONSTRAINT ux_download_failures_fingerprint UNIQUE (fingerprint)",
		"retry_count INTEGER NOT NULL DEFAULT 1",
		"next_retry_at TIMESTAMPTZ NOT NULL",
		"source VARCHAR(1000)",
		"error_message VARCHAR(1000)",
		"CREATE INDEX ix_download_failures_next_retry_at",
		"DROP TABLE IF EXISTS download_failures",
	} {
		if !strings.Contains(migration, required) {
			t.Fatalf("migration missing %q: %s", required, migration)
		}
	}
}

func TestNormalizeV2DownloadFailureFingerprints(t *testing.T) {
	t.Parallel()

	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	got := normalizeV2DownloadFailureFingerprints([]string{
		" " + strings.ToUpper(first) + " ", "invalid", first, second, "",
	})
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("normalized fingerprints = %#v", got)
	}
}

func TestRecordV2DownloadFailureValidationAndSanitizedErrors(t *testing.T) {
	t.Parallel()

	failure := subscription.V2DownloadFailure{
		Fingerprint: strings.Repeat("a", 64),
		Source:      "Subscribe|private",
	}
	var store *Store
	if err := store.RecordV2DownloadFailure(
		t.Context(), failure, time.Now(), subscription.V2DownloadFailureTransientTTL,
	); !errors.Is(err, ErrV2DownloadFailurePersistence) {
		t.Fatalf("nil store error = %v", err)
	}
	if _, err := store.ListActiveV2DownloadFailureFingerprints(
		t.Context(), []string{failure.Fingerprint}, time.Now(),
	); !errors.Is(err, ErrV2DownloadFailurePersistence) {
		t.Fatalf("nil store list error = %v", err)
	}

	invalid := []struct {
		name       string
		failure    subscription.V2DownloadFailure
		failedAt   time.Time
		retryAfter time.Duration
	}{
		{name: "invalid fingerprint", failure: subscription.V2DownloadFailure{Fingerprint: "invalid"}, failedAt: time.Now(), retryAfter: time.Hour},
		{name: "zero time", failure: failure, retryAfter: time.Hour},
		{name: "unsupported ttl", failure: failure, failedAt: time.Now(), retryAfter: 2 * time.Hour},
		{name: "source too long", failure: func() subscription.V2DownloadFailure {
			value := failure
			value.Source = strings.Repeat("界", 1_001)
			return value
		}(), failedAt: time.Now(), retryAfter: time.Hour},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := store.RecordV2DownloadFailure(
				t.Context(), tt.failure, tt.failedAt, tt.retryAfter,
			); !errors.Is(err, ErrInvalidV2DownloadFailure) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := v2DownloadFailurePersistenceError(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled persistence error = %v", err)
	}
}
