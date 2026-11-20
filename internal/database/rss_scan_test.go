package database

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestScanRSSCursor(t *testing.T) {
	t.Parallel()

	zone := time.FixedZone("UTC+8", 8*60*60)
	highWater := time.Date(2026, time.August, 12, 12, 30, 0, 0, zone)
	nextPoll := highWater.Add(time.Minute)
	lastAttempt := highWater.Add(-time.Minute)
	lastSuccess := highWater.Add(-time.Hour)
	leaseExpires := highWater.Add(time.Hour)
	updated := highWater.Add(2 * time.Minute)
	leaseToken := uuidFromRSSFixture(t)

	row := rssCursorRowFunc(func(dest ...any) error {
		if len(dest) != 14 {
			return errors.New("unexpected RSS cursor column count")
		}
		*dest[0].(*int64) = 7
		*dest[1].(*string) = "sha256:cursor"
		*dest[2].(*string) = `"fixture-etag"`
		*dest[3].(*string) = "Wed, 12 Aug 2026 04:30:00 GMT"
		*dest[4].(**time.Time) = &highWater
		*dest[5].(*string) = "sha256:item"
		*dest[6].(*time.Time) = nextPoll
		*dest[7].(**time.Time) = &lastAttempt
		*dest[8].(**time.Time) = &lastSuccess
		*dest[9].(*int) = 2
		*dest[10].(*string) = "temporary failure"
		*dest[11].(**uuid.UUID) = &leaseToken
		*dest[12].(**time.Time) = &leaseExpires
		*dest[13].(*time.Time) = updated
		return nil
	})

	cursor, err := scanRSSCursor(row)
	if err != nil {
		t.Fatalf("scanRSSCursor() error = %v", err)
	}
	if cursor.SiteID != 7 || cursor.Endpoint != "sha256:cursor" || cursor.LeaseToken != leaseToken {
		t.Fatalf("scanRSSCursor() identity = %#v", cursor)
	}
	if cursor.HighWaterPublishedAt != &highWater || cursor.LastAttemptAt != &lastAttempt || cursor.LastSuccessAt != &lastSuccess || cursor.LeaseExpiresAt != &leaseExpires {
		t.Fatalf("scanRSSCursor() nullable timestamps = %#v", cursor)
	}
	if cursor.NextPollAt.Location() != time.UTC || cursor.UpdatedAt.Location() != time.UTC {
		t.Fatalf("scanRSSCursor() UTC fields = next %v, updated %v", cursor.NextPollAt.Location(), cursor.UpdatedAt.Location())
	}
}

func TestScanRSSCursorHandlesNullableLeaseAndScanError(t *testing.T) {
	t.Parallel()

	row := rssCursorRowFunc(func(dest ...any) error {
		now := time.Now()
		*dest[6].(*time.Time) = now
		*dest[11].(**uuid.UUID) = nil
		*dest[13].(*time.Time) = now
		return nil
	})
	cursor, err := scanRSSCursor(row)
	if err != nil {
		t.Fatalf("scanRSSCursor() nil lease error = %v", err)
	}
	if cursor.LeaseToken != uuid.Nil {
		t.Fatalf("scanRSSCursor() lease token = %s, want nil UUID", cursor.LeaseToken)
	}

	wantErr := errors.New("scan failed")
	if _, err := scanRSSCursor(rssCursorRowFunc(func(...any) error { return wantErr })); !errors.Is(err, wantErr) {
		t.Fatalf("scanRSSCursor() error = %v, want %v", err, wantErr)
	}
}

type rssCursorRowFunc func(...any) error

func (scan rssCursorRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}
