package subscriptionflow

import (
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// Frozen V2 subscribe.py:1965-1969 rejects TV resources with multiple seasons
// during RSS matching, before either normal or best-version download selection.
func TestRSSHandlerMatchesFrozenV2MultiSeasonRejection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		season      int
		titles      []string
		wantID      string
		bestVersion bool
	}{
		{
			name: "multiple seasons beginning with target", season: 1,
			titles: []string{"Example.Movie.S01-S02.2026.1080p.WEB-DL"},
		},
		{
			name: "multiple seasons containing target", season: 2,
			titles: []string{"Example.Movie.S01-S02.2026.1080p.WEB-DL"},
		},
		{
			name: "best version rejects multiple seasons", season: 1, bestVersion: true,
			titles: []string{"Example.Movie.S01-S02.2026.1080p.WEB-DL"},
		},
		{
			name: "single season remains eligible", season: 1, wantID: "41",
			titles: []string{"Example.Movie.S01.2026.1080p.WEB-DL"},
		},
		{
			name: "single episode remains eligible", season: 1, wantID: "41",
			titles: []string{"Example.Movie.S01E01.2026.1080p.WEB-DL"},
		},
		{
			name: "single season after multiple seasons remains eligible", season: 1, wantID: "42",
			titles: []string{
				"Example.Movie.S01-S02.2026.1080p.WEB-DL",
				"Example.Movie.S01.2026.1080p.WEB-DL",
			},
		},
	}
	for _, staged := range []bool{false, true} {
		name := "async"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					target := subscriptionFlowTarget()
					target.InternalState = subscription.Active
					target.MediaType = string(media.TypeTV)
					target.MediaSource = "themoviedb"
					target.MediaID = "42"
					target.Season = &tt.season
					target.Config["total_episode"] = float64(2)
					target.Config["start_episode"] = float64(1)
					target.Config["best_version"] = tt.bestVersion
					store := newRSSFlowStore(target)
					store.episodes = []subscription.SubscriptionEpisode{
						{SubscriptionID: target.ID, Season: tt.season, Episode: 1, State: subscription.SubscriptionEpisodeMissing},
						{SubscriptionID: target.ID, Season: tt.season, Episode: 2, State: subscription.SubscriptionEpisodeMissing},
					}
					adapter := rssFlowAdapter{
						subscriptionFlowAdapter: subscriptionFlowAdapter{
							id: 7, name: "rss", kind: "declarative-rss", version: 1,
							body: v2TVTestMetainfo(t, "Example.Movie.S01E01.mkv", "Example.Movie.S01E02.mkv"),
						},
						key: "sha256:rss-multiseason-parity",
					}
					submission := &subscriptionFlowSubmission{}
					base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission)
					setSubscriptionFlowTVMetadata(base, target)
					if staged {
						base.execution = &subscriptionFlowExecution{submission: submission, store: store.subscriptionFlowStore}
					}
					handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
					if err != nil {
						t.Fatal(err)
					}
					resources := make([]indexer.Resource, 0, len(tt.titles))
					for i, title := range tt.titles {
						resourceID := "41"
						if i > 0 {
							resourceID = "42"
						}
						resources = append(resources, indexer.Resource{SiteID: 7, Site: "rss", Title: title, DownloadID: resourceID})
					}
					matched, err := handler.matchRSSTarget(t.Context(), rssSources([]indexer.Adapter{adapter}), map[int][]indexer.Resource{7: resources}, target)
					if err != nil {
						t.Fatal(err)
					}
					wantMatched := tt.wantID != ""
					if matched != wantMatched {
						t.Fatalf("matched = %t, want %t", matched, wantMatched)
					}
					calls := submission.calls + submission.rssStageCalls + submission.stageCalls
					wantCalls := 0
					if wantMatched {
						wantCalls = 1
					}
					if calls != wantCalls || submission.plan.ResourceID != tt.wantID {
						t.Fatalf("submissions/resource = %d/%q, want %d/%q", calls, submission.plan.ResourceID, wantCalls, tt.wantID)
					}
				})
			}
		})
	}
}
