package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestTVTargetMatchesExtractedV2Fixture(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file")
	}
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "testdata", "contracts", "fixtures", "r06", "v2-target-change.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name           string                 `json:"name"`
			Action         string                 `json:"action"`
			Snapshot       TVTargetSnapshot       `json:"snapshot"`
			MetadataTotal  int                    `json:"metadata_total"`
			Update         TVPublicTargetUpdate   `json:"update"`
			ShrinkEvidence TVTargetShrinkEvidence `json:"shrink_evidence"`
			Expected       TVTargetResolution     `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			var got TVTargetResolution
			var err error
			switch test.Action {
			case "create":
				got, err = ResolveTVCreationTarget(test.Snapshot, test.MetadataTotal)
			case "public_update":
				got, err = ApplyTVPublicTargetUpdate(test.Snapshot, test.Update)
			case "automatic":
				got, err = ResolveTVAutomaticTarget(test.Snapshot, test.MetadataTotal, test.ShrinkEvidence)
			case "reset":
				got, err = ResetTVTarget(test.Snapshot)
			case "describe":
				got, err = DescribeTVTarget(test.Snapshot)
			default:
				t.Fatalf("unknown fixture action %q", test.Action)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.Expected) {
				t.Fatalf("result=%#v, expected=%#v", got, test.Expected)
			}
		})
	}
}

func TestResolveTVCreationTarget(t *testing.T) {
	tests := []struct {
		name        string
		input       TVTargetSnapshot
		metadata    int
		knowledge   TVTargetKnowledge
		total, lack int
	}{
		{"unknown", TVTargetSnapshot{Season: 1}, 0, TVTargetUnknown, 0, 0},
		{"metadata total", TVTargetSnapshot{Season: 1}, 3, TVTargetKnown, 3, 3},
		{"positive requested total", TVTargetSnapshot{Season: 1, TotalEpisode: 2}, 5, TVTargetKnown, 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTVCreationTarget(tt.input, tt.metadata)
			if err != nil || got.Knowledge != tt.knowledge || got.Snapshot.TotalEpisode != tt.total || got.Snapshot.LackEpisode != tt.lack {
				t.Fatalf("result=%#v err=%v", got, err)
			}
		})
	}
}

func TestApplyTVPublicTargetUpdateLocksOnlyChangedTotal(t *testing.T) {
	base := TVTargetSnapshot{Season: 1, TotalEpisode: 3, LackEpisode: 1}
	got, err := ApplyTVPublicTargetUpdate(base, TVPublicTargetUpdate{TotalEpisode: intPtr(5)})
	if err != nil || !got.Snapshot.ManualTotalEpisode || got.Snapshot.LackEpisode != 3 {
		t.Fatalf("expansion=%#v err=%v", got, err)
	}
	got, err = ApplyTVPublicTargetUpdate(base, TVPublicTargetUpdate{TotalEpisode: intPtr(3)})
	if err != nil || got.Snapshot.ManualTotalEpisode {
		t.Fatalf("same total=%#v err=%v", got, err)
	}
	got, err = ApplyTVPublicTargetUpdate(TVTargetSnapshot{Season: 1, TotalEpisode: 5, LackEpisode: 3}, TVPublicTargetUpdate{TotalEpisode: intPtr(2)})
	if err != nil || !got.Snapshot.ManualTotalEpisode || got.Snapshot.LackEpisode != 3 {
		t.Fatalf("decrease=%#v err=%v", got, err)
	}
}

func TestResolveTVAutomaticTargetAndShrinkEvidence(t *testing.T) {
	base := TVTargetSnapshot{Season: 1, TotalEpisode: 8, LackEpisode: 4}
	got, err := ResolveTVAutomaticTarget(base, 4, TVTargetShrinkEvidence{Available: true, MissingEpisodes: []int{6, 8}})
	if err != nil || got.Snapshot.TotalEpisode != 7 {
		t.Fatalf("shrink clamp=%#v err=%v", got, err)
	}
	base = TVTargetSnapshot{Season: 1, TotalEpisode: 2, LackEpisode: 0, Note: []int{1}}
	got, err = ResolveTVAutomaticTarget(base, 4, TVTargetShrinkEvidence{})
	if err != nil || got.Snapshot.TotalEpisode != 4 || got.Snapshot.LackEpisode != 2 {
		t.Fatalf("expand=%#v err=%v", got, err)
	}
	base.ManualTotalEpisode = true
	got, err = ResolveTVAutomaticTarget(base, 6, TVTargetShrinkEvidence{})
	if err != nil || got.Changed || got.Snapshot.TotalEpisode != 2 {
		t.Fatalf("manual lock=%#v err=%v", got, err)
	}
}

func TestTVTargetSeasonStartAndReset(t *testing.T) {
	base := TVTargetSnapshot{Season: 1, StartEpisode: 0, TotalEpisode: 5, LackEpisode: 2, Note: []int{1, 3, 5}}
	got, err := ApplyTVPublicTargetUpdate(base, TVPublicTargetUpdate{Season: intPtr(2), StartEpisode: intPtr(3)})
	if err != nil || len(got.TargetEpisodes) != 3 || got.TargetEpisodes[0].Season != 2 || got.TargetEpisodes[0].Episode != 3 {
		t.Fatalf("range change=%#v err=%v", got, err)
	}
	if got.Snapshot.LackEpisode != 2 || len(got.Snapshot.Note) != 3 {
		t.Fatalf("facts unexpectedly changed=%#v", got.Snapshot)
	}
	reset, err := ResetTVTarget(TVTargetSnapshot{Season: 1, TotalEpisode: 4, LackEpisode: 1, ManualTotalEpisode: true, Note: []int{1}, EpisodePriority: map[int]int{1: 100}})
	if err != nil || reset.Snapshot.ManualTotalEpisode || reset.Snapshot.LackEpisode != 4 || len(reset.Snapshot.Note) != 0 || reset.Snapshot.CurrentPriority != nil {
		t.Fatalf("reset=%#v err=%v", reset, err)
	}
}

func TestTVTargetNoteEpisodesFiltersCurrentRange(t *testing.T) {
	got, err := DescribeTVTarget(TVTargetSnapshot{Season: 2, StartEpisode: 3, TotalEpisode: 5, Note: []int{1, 3, 5, 6}})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := TargetNoteEpisodes(got.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Episode{{Season: 2, Episode: 3}, {Season: 2, Episode: 5}}; len(filtered) != len(want) || filtered[0] != want[0] || filtered[1] != want[1] {
		t.Fatalf("filtered notes=%#v", filtered)
	}
}

func TestTVTargetRejectsUnboundedFacts(t *testing.T) {
	tooMany := make([]int, maxTVTargetFacts+1)
	if _, err := DescribeTVTarget(TVTargetSnapshot{Season: 1, TotalEpisode: 1, Note: tooMany}); err != ErrInvalidTVTarget {
		t.Fatalf("DescribeTVTarget() error=%v", err)
	}
	if _, err := TargetNoteEpisodes(TVTargetSnapshot{Season: 1, TotalEpisode: maxTVTargetEpisode + 1}); err != ErrInvalidTVTarget {
		t.Fatalf("TargetNoteEpisodes() error=%v", err)
	}
	if _, err := ApplyTVPublicTargetUpdate(
		TVTargetSnapshot{Season: 1, TotalEpisode: 1, LackEpisode: maxTVTargetFacts},
		TVPublicTargetUpdate{TotalEpisode: intPtr(2)},
	); err != ErrInvalidTVTarget {
		t.Fatalf("ApplyTVPublicTargetUpdate() error=%v", err)
	}
	if _, err := ResolveTVAutomaticTarget(
		TVTargetSnapshot{Season: 1, TotalEpisode: 1, LackEpisode: maxTVTargetFacts},
		2,
		TVTargetShrinkEvidence{},
	); err != ErrInvalidTVTarget {
		t.Fatalf("ResolveTVAutomaticTarget() error=%v", err)
	}
}

func TestTVTargetSuccessfulOutputsAreValidInputs(t *testing.T) {
	results := []TVTargetResolution{}
	created, err := ResolveTVCreationTarget(TVTargetSnapshot{Season: 1}, 3)
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, created)
	updated, err := ApplyTVPublicTargetUpdate(created.Snapshot, TVPublicTargetUpdate{TotalEpisode: intPtr(5)})
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, updated)
	automatic, err := ResolveTVAutomaticTarget(
		TVTargetSnapshot{Season: 1, TotalEpisode: 3, LackEpisode: 1}, 5, TVTargetShrinkEvidence{},
	)
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, automatic)
	reset, err := ResetTVTarget(updated.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	results = append(results, reset)
	for _, result := range results {
		if _, err := DescribeTVTarget(result.Snapshot); err != nil {
			t.Fatalf("successful output rejected on round trip: %#v: %v", result, err)
		}
	}
}

func intPtr(value int) *int { return &value }
