package subscriptionflow

import (
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
	"github.com/moviepilot-go/moviepilot-go/internal/torrentmeta"
)

func TestV2TVTorrentEpisodeProjectionSkipsOutOfRangeFiles(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{
		"Show.S01E5001.mkv", "Show.E9999.mkv", "Show.S01E4999-E5001.mkv",
		"Show.E4999-E5001.mkv", "Show.S01E00.mkv",
	} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			got := v2TVEpisodesFromTorrentFiles([]torrentmeta.File{
				{Path: "Show.S01E01.mkv"}, {Path: invalid},
			}, nil, 1)
			if want := v2TVTestEpisodes(1, 1); !reflect.DeepEqual(got, want) {
				t.Fatalf("projection = %v, want %v", got, want)
			}
		})
	}
}

func TestExecuteV2TVCandidatesOutOfRangeFileDoesNotStopBatch(t *testing.T) {
	t.Parallel()
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1))
	fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.E9999.mkv")
	got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
		[]subscription.Candidate{v2TVExecutionCandidate("full", 1)},
		[]indexer.Adapter{fixture.adapter}, false)
	if err != nil || len(got.RemainingEpisodes) != 0 || fixture.execution.calls != 1 {
		t.Fatalf("result=%#v calls=%d err=%v", got, fixture.execution.calls, err)
	}
}
