package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

func TestScrapeRequestMigrationContract(t *testing.T) {
	t.Parallel()
	content, err := migrationFS.ReadFile("migrations/023_scrape_notification_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.Join(strings.Fields(string(content)), " ")
	for _, fragment := range []string{
		"CREATE TABLE scrape_requests",
		"CHECK (octet_length(media_path) BETWEEN 1 AND 4096)",
		"attempt_count BETWEEN 0 AND 5",
		"state IN ('pending', 'working', 'failed', 'completed', 'skipped')",
		"CREATE TRIGGER scrape_request_plan_immutable",
		"WHERE state IN ('pending', 'failed') AND attempt_count < 5",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing %q", fragment)
		}
	}
	if strings.Contains(sql, "chr(0)") {
		t.Fatal("migration must not evaluate chr(0); PostgreSQL text rejects NUL before constraints run")
	}
	identityFix, err := migrationFS.ReadFile("migrations/024_scrape_skipped_episode_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	identitySQL := strings.Join(strings.Fields(string(identityFix)), " ")
	for _, fragment := range []string{
		"DROP CONSTRAINT IF EXISTS scrape_requests_identity_check",
		"kind='episode' AND (episode>0 OR state='skipped')",
		"UPDATE scrape_requests SET episode=1, updated_at=now() WHERE kind='episode' AND state='skipped' AND episode=0",
	} {
		if !strings.Contains(identitySQL, fragment) {
			t.Fatalf("identity migration missing %q", fragment)
		}
	}
}

func TestScrapeRequestStoreValidation(t *testing.T) {
	t.Parallel()
	var store *Store
	requestID := uuid.New()
	now := time.Now().UTC()
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{name: "nil context", call: func() error {
			_, _, err := store.ClaimScrape(nil, requestID, now, time.Minute)
			return err
		}, want: scrapeflow.ErrPersistence},
		{name: "empty request", call: func() error {
			_, _, err := store.ClaimScrape(context.Background(), uuid.Nil, now, time.Minute)
			return err
		}, want: scrapeflow.ErrInvalidPlan},
		{name: "invalid list limit", call: func() error {
			_, err := store.ListDueScrapes(context.Background(), now, scrapeflow.SweepPageSize+1)
			return err
		}, want: scrapeflow.ErrInvalidPlan},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}
