package subscriptionflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestTVTargetSnapshotAcceptsV2IntegerAndBooleanFlags(t *testing.T) {
	season := 2
	snapshot, legacy, err := tvTargetSnapshot(database.Subscription{
		Season: &season,
		Config: map[string]any{
			"total_episode": 4.0, "start_episode": 2.0, "lack_episode": 3.0,
			"manual_total_episode": 1.0, "best_version": true, "best_version_full": 0,
			"note": []any{2.0}, "current_priority": 5.0,
			"episode_priority": map[string]any{"2": 5.0},
		},
	})
	if err != nil || legacy || snapshot.Season != 2 || snapshot.TotalEpisode != 4 ||
		!snapshot.ManualTotalEpisode || !snapshot.BestVersion || snapshot.BestVersionFull ||
		!reflect.DeepEqual(snapshot.Note, []int{2}) || snapshot.CurrentPriority == nil ||
		*snapshot.CurrentPriority != 5 || snapshot.EpisodePriority[2] != 5 {
		t.Fatalf("snapshot=%#v legacy=%v err=%v", snapshot, legacy, err)
	}
}

func TestTVTargetSnapshotNormalizesMixedEpisodePriority(t *testing.T) {
	t.Parallel()
	snapshot, _, err := tvTargetSnapshot(database.Subscription{Config: map[string]any{
		"total_episode": 3, "best_version": true,
		"episode_priority": map[string]any{"1": "100", "2": nil, "bad": 90, "3": 90.9},
	}})
	if err != nil || !reflect.DeepEqual(snapshot.EpisodePriority, map[int]int{1: 100, 3: 90}) {
		t.Fatalf("priorities=%v err=%v", snapshot.EpisodePriority, err)
	}
}

func TestRefreshTVTargetUsesAnyVersionFactsInsteadOfCompletedQuality(t *testing.T) {
	t.Parallel()
	target := database.Subscription{
		ID: 42, Username: "alice", Name: "Show", MediaType: string(media.TypeTV),
		UpdatedAt:   time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
		MediaSource: "themoviedb", MediaID: "7",
		Config: map[string]any{
			"total_episode": 2, "lack_episode": 1, "best_version": true,
			"episode_priority": map[string]any{"1": "90", "2": "100", "bad": nil},
		},
	}
	store := &subscriptionFlowStore{}
	handler := &Handler{store: store, mediaDetails: &subscriptionFlowMediaDetails{item: metadata.Media{
		Type: "tv", SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 1, EpisodeCount: 3}},
	}}}
	refreshed, resolution, _, err := handler.refreshTVTarget(t.Context(), target)
	if err != nil || resolution.Snapshot.LackEpisode != 1 || resolution.Snapshot.CurrentPriority == nil ||
		*resolution.Snapshot.CurrentPriority != 0 || !reflect.DeepEqual(resolution.Snapshot.EpisodePriority, map[int]int{1: 90, 2: 100}) ||
		refreshed.Config["lack_episode"] != 1 || len(store.reconciled) != 1 {
		t.Fatalf("resolution=%+v refreshed=%+v err=%v", resolution, refreshed.Config, err)
	}
	facts, err := subscription.DescribeTVBestVersion(resolution.Snapshot)
	if err != nil || facts.Complete || len(facts.DownloadedEpisodes) != 2 || len(facts.PendingEpisodes) != 2 {
		t.Fatalf("facts=%+v err=%v", facts, err)
	}
}

func TestRefreshTVTargetExpandsAndUsesShrinkEvidence(t *testing.T) {
	tests := []struct {
		name          string
		metadataTotal int
		existing      []subscription.Episode
		wantTotal     int
		wantLack      int
	}{
		{name: "expand", metadataTotal: 4, wantTotal: 4, wantLack: 4},
		{
			name: "shrink clamped to confirmed maximum", metadataTotal: 3,
			existing:  []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}, {Season: 1, Episode: 4}},
			wantTotal: 4, wantLack: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			initialTotal := 2
			if tt.metadataTotal < initialTotal || len(tt.existing) > 0 {
				initialTotal = 5
			}
			store := &subscriptionFlowStore{}
			resolver := &subscriptionFlowMediaDetails{item: metadata.Media{
				Type: "tv", SeasonInfo: []metadata.SeasonInfo{{SeasonNumber: 1, EpisodeCount: tt.metadataTotal}},
			}}
			handler := &Handler{store: store, mediaDetails: resolver}
			if tt.existing != nil {
				handler.existingEpisodes = &existingEpisodesSourceFake{episodes: tt.existing}
			}
			season := 1
			target := database.Subscription{
				ID: 42, Username: "alice", Name: "Show", MediaType: string(media.TypeTV),
				Season: &season, MediaSource: "themoviedb", MediaID: "7", UpdatedAt: now,
				Config: map[string]any{"total_episode": initialTotal, "lack_episode": initialTotal},
			}
			refreshed, resolution, legacy, err := handler.refreshTVTarget(t.Context(), target)
			if err != nil || legacy || resolution.Snapshot.TotalEpisode != tt.wantTotal ||
				resolution.Snapshot.LackEpisode != tt.wantLack || refreshed.Config["total_episode"] != tt.wantTotal ||
				len(store.reconciled) != 1 {
				t.Fatalf("resolution=%#v refreshed=%#v legacy=%v reconciles=%d err=%v", resolution, refreshed, legacy, len(store.reconciled), err)
			}
		})
	}
}

func TestRefreshTVTargetManualLockSkipsMetadata(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	store := &subscriptionFlowStore{}
	resolver := &subscriptionFlowMediaDetails{item: metadata.Media{Type: "tv", EpisodeCount: 10}}
	handler := &Handler{store: store, mediaDetails: resolver}
	season := 1
	_, resolution, _, err := handler.refreshTVTarget(t.Context(), database.Subscription{
		ID: 43, Username: "alice", Name: "Show", MediaType: string(media.TypeTV), Season: &season,
		MediaSource: "themoviedb", MediaID: "8", UpdatedAt: now,
		Config: map[string]any{"total_episode": 2, "lack_episode": 2, "manual_total_episode": 1},
	})
	if err != nil || resolution.Snapshot.TotalEpisode != 2 || resolver.calls != 0 {
		t.Fatalf("resolution=%#v metadata calls=%d err=%v", resolution, resolver.calls, err)
	}
}
