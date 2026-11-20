package scrapeflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestServiceHandle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 19, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		scrapeErr error
		wantDone  bool
		wantRetry bool
		wantErr   error
	}{
		{name: "success", wantDone: true},
		{name: "retryable failure is persisted", scrapeErr: errors.New("tmdb secret"), wantRetry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id := uuid.New()
			store := &scrapeStoreFake{lease: Lease{Token: uuid.New(), Request: Request{
				ID: id, Kind: KindMovie, MediaPath: testAbsolutePath(t, "movie.mkv"), TMDBID: 42,
				State: StateWorking, AttemptCount: 1,
			}}, claimed: true}
			service, err := NewService(store, scrapeFake{err: tt.scrapeErr})
			if err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return now }
			err = service.Handle(t.Context(), id)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Handle() error = %v, want %v", err, tt.wantErr)
			}
			if store.completed != tt.wantDone || store.rescheduled != tt.wantRetry {
				t.Fatalf("completed=%v rescheduled=%v", store.completed, store.rescheduled)
			}
			if store.stableErr != "" && store.stableErr != "scrape_failed" {
				t.Fatalf("stable error = %q", store.stableErr)
			}
		})
	}
}

func TestSweepHandlerRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	handler, err := NewSweepHandler(&scrapeSweepStoreFake{ids: []uuid.UUID{id, id}}, &scrapeEnqueuerFake{})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Handle(t.Context()); !errors.Is(err, ErrPersistence) {
		t.Fatalf("Handle() error = %v, want %v", err, ErrPersistence)
	}
}

func testAbsolutePath(t *testing.T, name string) string {
	t.Helper()
	return t.TempDir() + string([]rune{filepathSeparator()}) + name
}

func filepathSeparator() rune {
	return []rune("\\")[0]
}

type scrapeFake struct{ err error }

func (s scrapeFake) ScrapeMovieNFO(context.Context, int, string) error             { return s.err }
func (s scrapeFake) ScrapeTVShowNFO(context.Context, int, string) error            { return s.err }
func (s scrapeFake) ScrapeEpisodeNFO(context.Context, int, int, int, string) error { return s.err }

type scrapeStoreFake struct {
	lease       Lease
	claimed     bool
	completed   bool
	rescheduled bool
	stableErr   string
}

func (s *scrapeStoreFake) ClaimScrape(context.Context, uuid.UUID, time.Time, time.Duration) (Lease, bool, error) {
	return s.lease, s.claimed, nil
}
func (s *scrapeStoreFake) CompleteScrape(context.Context, uuid.UUID, uuid.UUID, time.Time) error {
	s.completed = true
	return nil
}
func (s *scrapeStoreFake) RescheduleScrape(_ context.Context, _, _ uuid.UUID, _ time.Time, stable string) error {
	s.rescheduled = true
	s.stableErr = stable
	return nil
}

type scrapeSweepStoreFake struct{ ids []uuid.UUID }

func (s *scrapeSweepStoreFake) RecoverExpiredScrapes(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}
func (s *scrapeSweepStoreFake) ListDueScrapes(context.Context, time.Time, int) ([]uuid.UUID, error) {
	return append([]uuid.UUID(nil), s.ids...), nil
}

type scrapeEnqueuerFake struct{}

func (*scrapeEnqueuerFake) EnqueueScrape(context.Context, uuid.UUID) error { return nil }
