package database

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestScanSubscriptionEpisode(t *testing.T) {
	t.Parallel()

	updatedAt := time.Date(2026, time.August, 13, 1, 0, 0, 0, time.UTC)
	intentID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	valid := SubscriptionEpisode{
		ID:               71,
		SubscriptionID:   29,
		Season:           1,
		Episode:          3,
		State:            SubscriptionEpisodeDownloading,
		DownloadIntentID: &intentID,
		UpdatedAt:        updatedAt,
	}

	got, err := scanSubscriptionEpisode(subscriptionEpisodeRow(valid))
	if err != nil {
		t.Fatalf("scanSubscriptionEpisode() error = %v", err)
	}
	if got.ID != valid.ID || got.SubscriptionID != valid.SubscriptionID ||
		got.Season != valid.Season || got.Episode != valid.Episode || got.State != valid.State ||
		got.DownloadIntentID == nil || *got.DownloadIntentID != intentID || !got.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("scanSubscriptionEpisode() = %#v, want %#v", got, valid)
	}

	tests := []struct {
		name   string
		mutate func(*SubscriptionEpisode)
	}{
		{name: "非法记录 ID", mutate: func(item *SubscriptionEpisode) { item.ID = 0 }},
		{name: "非法订阅 ID", mutate: func(item *SubscriptionEpisode) { item.SubscriptionID = 0 }},
		{name: "非法季号", mutate: func(item *SubscriptionEpisode) { item.Season = maxSubscriptionEpisodeSeason + 1 }},
		{name: "非法集号", mutate: func(item *SubscriptionEpisode) { item.Episode = 0 }},
		{name: "非法状态", mutate: func(item *SubscriptionEpisode) { item.State = SubscriptionEpisodeState("corrupt") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stored := valid
			tt.mutate(&stored)
			if _, scanErr := scanSubscriptionEpisode(subscriptionEpisodeRow(stored)); !errors.Is(scanErr, ErrSubscriptionEpisodePersistence) {
				t.Fatalf("scanSubscriptionEpisode() error = %v, want ErrSubscriptionEpisodePersistence", scanErr)
			}
		})
	}
}

func TestScanSubscriptionEpisodePropagatesScanError(t *testing.T) {
	t.Parallel()

	want := errors.New("fixture scan failure")
	if _, err := scanSubscriptionEpisode(subscriptionEpisodeRowFunc(func(...any) error { return want })); !errors.Is(err, want) {
		t.Fatalf("scanSubscriptionEpisode() error = %v, want %v", err, want)
	}
}

func subscriptionEpisodeRow(item SubscriptionEpisode) rowScanner {
	return subscriptionEpisodeRowFunc(func(dest ...any) error {
		*dest[0].(*int64) = item.ID
		*dest[1].(*int64) = item.SubscriptionID
		*dest[2].(*int) = item.Season
		*dest[3].(*int) = item.Episode
		*dest[4].(*SubscriptionEpisodeState) = item.State
		*dest[5].(**uuid.UUID) = item.DownloadIntentID
		*dest[6].(*time.Time) = item.UpdatedAt
		return nil
	})
}

type subscriptionEpisodeRowFunc func(...any) error

func (scan subscriptionEpisodeRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}
