package subscriptionflow

import (
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// Frozen V2 subscribe.py:3626-3633 keeps the whole-season sentinel only
// when the subscription does not trim the season's original beginning.
// download.py:1259 admits that sentinel to the whole-season pass; an
// explicit remaining range reaches file selection through unpack instead.
func TestExecuteV2TVCandidatesOriginalWholeSeasonStartBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		start         any
		missing       []subscription.Episode
		titleEpisodes []int
		wantPaused    bool
	}{
		{
			name:    "default start keeps whole season",
			missing: v2TVTestEpisodes(1, 1, 2, 3),
		},
		{
			name:    "zero start keeps whole season",
			start:   0,
			missing: v2TVTestEpisodes(1, 1, 2, 3),
		},
		{
			name:    "first episode keeps whole season",
			start:   1,
			missing: v2TVTestEpisodes(1, 1, 2, 3),
		},
		{
			name:       "trimmed beginning selects only requested files",
			start:      2,
			missing:    v2TVTestEpisodes(1, 2, 3),
			wantPaused: true,
		},
		{
			name:       "last episode selects only requested file",
			start:      3,
			missing:    v2TVTestEpisodes(1, 3),
			wantPaused: true,
		},
		{
			name:          "explicit title range still downloads whole torrent",
			start:         2,
			missing:       v2TVTestEpisodes(1, 2, 3),
			titleEpisodes: []int{2, 3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, test.missing)
			fixture.target.Config["total_episode"] = 3
			if test.start != nil {
				fixture.target.Config["start_episode"] = test.start
			}
			fixture.adapter.body = v2TVTestMetainfo(t,
				"Show.S01E01.mkv", "Show.S01E02.mkv", "Show.S01E03.mkv")
			candidate := v2TVExecutionCandidate("season-pack", 1, test.titleEpisodes...)

			got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target,
				fixture.query, []subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, false)
			if err != nil || len(got.RemainingEpisodes) != 0 ||
				!reflect.DeepEqual(got.AcceptedCandidateKeys, []string{"season-pack"}) ||
				len(fixture.submission.calls) != 1 || fixture.execution.calls != 1 {
				t.Fatalf("selection = %#v, error = %v, stages = %#v, executions = %d",
					got, err, fixture.submission.calls, fixture.execution.calls)
			}
			call := fixture.submission.calls[0]
			if call.plan.Paused != test.wantPaused || !reflect.DeepEqual(call.episodes, test.missing) {
				t.Fatalf("paused = %v, staged episodes = %#v; want paused = %v, episodes = %#v",
					call.plan.Paused, call.episodes, test.wantPaused, test.missing)
			}
			wantInputEpisodes := make([]organization.Episode, len(test.missing))
			for index, episode := range test.missing {
				wantInputEpisodes[index] = organization.Episode{Season: episode.Season, Episode: episode.Episode}
			}
			if !reflect.DeepEqual(call.plan.TVCoverage, test.missing) ||
				!reflect.DeepEqual(call.input.Episodes, wantInputEpisodes) {
				t.Fatalf("coverage = %#v, success episodes = %#v; want %#v / %#v",
					call.plan.TVCoverage, call.input.Episodes, test.missing, wantInputEpisodes)
			}
		})
	}
}
