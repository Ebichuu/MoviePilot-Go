package subscriptionflow

import (
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// Frozen download.py passes episodes only in its unpack stage. The persisted
// marker must preserve that distinction while keeping quality/notification facts.
func TestV2TVStagesPersistOriginalFileSelectionAndQualityFacts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		best       bool
		full       bool
		missing    []int
		priorities map[int]int
		title      []int
		paused     bool
		facts      []int
	}{
		{name: "普通整季", missing: []int{1, 2}, facts: []int{1, 2}},
		{name: "普通标题", missing: []int{1, 2}, title: []int{1}, facts: []int{1}},
		{name: "普通拆包", missing: []int{2}, title: []int{1, 2}, paused: true, facts: []int{2}},
		{name: "分集洗版整包", best: true, priorities: map[int]int{1: 20, 2: 20}, facts: []int{1, 2}},
		{name: "分集洗版标题", best: true, priorities: map[int]int{1: 20, 2: 80}, title: []int{1}, facts: []int{1}},
		{name: "分集洗版拆包", best: true, priorities: map[int]int{1: 80, 2: 20}, title: []int{1, 2}, paused: true, facts: []int{2}},
		{name: "全集洗版整包", best: true, full: true, facts: []int{1, 2}},
		{name: "全集洗版标题保留目标外元数据事实", best: true, full: true, title: []int{1, 2, 3}, facts: []int{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var fixture v2TVExecutionFixture
			if tc.best {
				fixture = newV2TVBestVersionFixture(t, subscription.Active, 2, tc.priorities, tc.full)
			} else {
				fixture = newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, tc.missing...))
				fixture.target.Config["total_episode"] = 2
			}
			fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv")
			candidate := v2TVBestVersionCandidate("stage-contract", 40, 1, tc.title...)
			got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, fixture.query,
				[]subscription.Candidate{candidate}, []indexer.Adapter{fixture.adapter}, tc.best)
			if err != nil || len(fixture.submission.calls) != 1 || len(got.AcceptedCandidateKeys) != 1 {
				t.Fatalf("result=%#v staged=%#v err=%v", got, fixture.submission.calls, err)
			}
			call := fixture.submission.calls[0]
			wantCoverage := v2TVTestEpisodes(1, tc.facts...)
			wantInput := make([]organization.Episode, len(wantCoverage))
			for i, episode := range wantCoverage {
				wantInput[i] = organization.Episode{Season: episode.Season, Episode: episode.Episode}
			}
			if call.plan.TVCoverage == nil || call.plan.Paused != tc.paused ||
				!reflect.DeepEqual(call.plan.TVCoverage, wantCoverage) || !reflect.DeepEqual(call.input.Episodes, wantInput) {
				t.Fatalf("paused=%v coverage=%v input=%v, want paused=%v facts=%v",
					call.plan.Paused, call.plan.TVCoverage, call.input.Episodes, tc.paused, wantCoverage)
			}
		})
	}
}
