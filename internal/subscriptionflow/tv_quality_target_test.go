package subscriptionflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type tvQualityTargetStore struct {
	*subscriptionFlowStore
	qualityCompleteCalls int
	qualityCompleteErr   error
	targetReadCalls      int
}

func (s *tvQualityTargetStore) ListSubscriptionEpisodesForTarget(
	context.Context, string, int64, time.Time, []subscription.Episode,
	[]subscription.SubscriptionEpisodeState, int,
) ([]subscription.SubscriptionEpisode, error) {
	s.targetReadCalls++
	return nil, errors.New("quality need must not consume ordinary indexed state")
}

func (s *tvQualityTargetStore) CompleteSatisfiedBestVersionTVSubscription(
	_ context.Context, owner string, id int64, state subscription.State, version time.Time,
) error {
	s.qualityCompleteCalls++
	s.completeOwner, s.completeID, s.completeState, s.completeTime = owner, id, state, version
	return s.qualityCompleteErr
}

func TestTVQualityTargetIgnoresIndexedAndLibraryProgress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		snapshot    subscription.TVTargetSnapshot
		missing     []int
		completable bool
		matchable   bool
	}{
		{"indexed lower quality still pending", subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, StartEpisode: 2, BestVersion: true, Note: []int{2, 3}, EpisodePriority: map[int]int{2: 90, 3: 100}}, []int{2}, false, true},
		{"legacy low quality still pending", subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, BestVersion: true, CurrentPriority: intPointer(90)}, []int{1, 2, 3}, false, true},
		{"per episode terminal facts complete", subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, BestVersion: true, EpisodePriority: map[int]int{1: 100, 2: 100, 3: 100}}, []int{}, true, true},
		{"full lower overall keeps all targets", subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, BestVersion: true, BestVersionFull: true, CurrentPriority: intPointer(90), EpisodePriority: map[int]int{1: 100, 2: 100, 3: 100}}, []int{1, 2, 3}, false, true},
		{"full nil overall keeps all targets", subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, BestVersion: true, BestVersionFull: true}, []int{1, 2, 3}, false, true},
		{"full100 completes", subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, BestVersion: true, BestVersionFull: true, CurrentPriority: intPointer(100)}, []int{}, true, true},
		{"full101 is not terminal", subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, BestVersion: true, BestVersionFull: true, CurrentPriority: intPointer(101)}, []int{1, 2, 3}, false, true},
		{"V2 known empty target full100 completes", subscription.TVTargetSnapshot{Season: 1, StartEpisode: 4, TotalEpisode: 3, BestVersion: true, BestVersionFull: true, CurrentPriority: intPointer(100)}, []int{}, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolution, err := subscription.DescribeTVTarget(test.snapshot)
			if err != nil {
				t.Fatal(err)
			}
			store := &tvQualityTargetStore{subscriptionFlowStore: &subscriptionFlowStore{}}
			library := &existingEpisodesSourceFake{episodes: resolution.TargetEpisodes}
			handler := &Handler{store: store, existingEpisodes: library}
			query, completable, matchable, err := handler.withCurrentTVMissingEpisodes(
				t.Context(), database.Subscription{ID: 42, Username: "owner"}, resolution, false,
				subscription.Query{MediaType: media.TypeTV},
			)
			missing := make([]int, 0, len(query.MissingEpisodes))
			for _, episode := range query.MissingEpisodes {
				missing = append(missing, episode.Episode)
			}
			if err != nil || !reflect.DeepEqual(missing, test.missing) || completable != test.completable || matchable != test.matchable || store.targetReadCalls != 0 || library.calls != 0 {
				t.Fatalf("missing=%v completable=%v matchable=%v targetReads=%d libraryReads=%d err=%v", missing, completable, matchable, store.targetReadCalls, library.calls, err)
			}
		})
	}
}

func TestTVQualityTargetCompletesThroughSeparateStoreCAS(t *testing.T) {
	t.Parallel()
	target := database.Subscription{ID: 42, Username: "owner", UpdatedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), Config: map[string]any{"best_version": true}}
	tests := []struct {
		name     string
		storeErr error
		wantErr  error
	}{
		{"quality completes", nil, nil},
		{"quality changed", database.ErrSubscriptionPreciseSearchStateConflict, ErrSubscriptionTargetChanged},
		{"deleted is not reported complete", database.ErrSubscriptionNotFound, ErrSubscriptionTargetChanged},
		{"failure stays sanitized", errors.New("db-secret"), ErrSubscriptionSearchStorage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := &tvQualityTargetStore{subscriptionFlowStore: &subscriptionFlowStore{}, qualityCompleteErr: test.storeErr}
			handler := &Handler{store: store}
			err := handler.finishSatisfiedTarget(t.Context(), target, subscription.PendingSearch)
			if !errors.Is(err, test.wantErr) || store.qualityCompleteCalls != 1 || store.completeCalls != 0 ||
				store.completeOwner != target.Username || store.completeID != target.ID || store.completeState != subscription.PendingSearch || !store.completeTime.Equal(target.UpdatedAt) {
				t.Fatalf("error=%v qualityCalls=%d ordinaryCalls=%d", err, store.qualityCompleteCalls, store.completeCalls)
			}
		})
	}
}

func TestTVTargetSnapshotQualityRoundTrip(t *testing.T) {
	t.Parallel()
	for _, quality := range []*int{nil, intPointer(0), intPointer(-1), intPointer(100)} {
		snapshot := subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 3, CurrentPriority: quality, EpisodePriority: map[int]int{1: 100}}
		config := applyTVTargetSnapshot(map[string]any{"best_version": true}, snapshot)
		if _, pointer := config["current_priority"].(*int); pointer {
			t.Fatal("config retained a pointer instead of a JSON scalar")
		}
		reread, _, err := tvTargetSnapshot(database.Subscription{Config: config})
		if err != nil || !reflect.DeepEqual(reread.CurrentPriority, quality) {
			t.Fatalf("quality=%v reread=%+v err=%v", quality, reread, err)
		}
	}
}
