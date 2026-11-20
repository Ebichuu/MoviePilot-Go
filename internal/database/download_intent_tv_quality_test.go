package database

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestDownloadIntentTVFactsNormalizationAndIsolation(t *testing.T) {
	base := DownloadIntentPlan{IntentID: uuid.New(), SiteID: 1, DownloaderID: downloadIntentPlanTestDownloaderID,
		ResolverKind: "haidan", ResolverVersion: 1, ResourceID: "quality", TVPriority: r08MoviePriority(90),
		TVCoverage: []subscription.Episode{{Season: 1, Episode: 2}, {Season: 1, Episode: 1}, {Season: 1, Episode: 2}}}
	normalized, err := normalizeDownloadIntentPlanFields(base)
	if err != nil || len(normalized.TVCoverage) != 2 || normalized.TVCoverage[0].Episode != 1 {
		t.Fatalf("normalized=%+v err=%v", normalized, err)
	}
	base.TVCoverage[0].Episode = 99
	*base.TVPriority = 100
	if normalized.TVCoverage[1].Episode != 2 || *normalized.TVPriority != 90 {
		t.Fatal("snapshot aliases caller data")
	}
	for _, mutate := range []func(*DownloadIntentPlan){
		func(p *DownloadIntentPlan) { p.TVPriority = r08MoviePriority(100) },
		func(p *DownloadIntentPlan) { p.TVPriority = nil },
		func(p *DownloadIntentPlan) { p.TVCoverage = nil },
		func(p *DownloadIntentPlan) { p.TVCoverage = []subscription.Episode{} },
		func(p *DownloadIntentPlan) { p.TVFullCoverage = true },
		func(p *DownloadIntentPlan) { p.TVCoverage = []subscription.Episode{{Season: 2, Episode: 1}} },
	} {
		other := normalized
		mutate(&other)
		if sameDownloadIntentPlan(normalized, other) {
			t.Fatal("different TV facts treated as replay")
		}
	}
	legacy, knownEmpty := normalized, normalized
	legacy.TVPriority, legacy.TVCoverage = nil, nil
	knownEmpty.TVPriority, knownEmpty.TVCoverage = nil, []subscription.Episode{}
	if sameDownloadIntentPlan(legacy, knownEmpty) {
		t.Fatal("unknown and known-empty conflated")
	}
	for _, mutate := range []func(*DownloadIntentPlan){
		func(p *DownloadIntentPlan) { p.TVPriority = r08MoviePriority(2147483648) },
		func(p *DownloadIntentPlan) { p.TVPriority = r08MoviePriority(-2147483649) },
		func(p *DownloadIntentPlan) { p.TVCoverage = nil },
		func(p *DownloadIntentPlan) { p.MoviePriority = r08MoviePriority(90) },
		func(p *DownloadIntentPlan) { p.TVPriority = nil; p.TVCoverage = nil; p.TVFullCoverage = true },
		func(p *DownloadIntentPlan) { p.TVCoverage = []subscription.Episode{{Season: -1, Episode: 1}} },
		func(p *DownloadIntentPlan) { p.TVCoverage = []subscription.Episode{{Season: 1, Episode: 0}} },
		func(p *DownloadIntentPlan) { p.TVCoverage = make([]subscription.Episode, maxSubscriptionEpisodes+1) },
	} {
		invalid := normalized
		mutate(&invalid)
		if _, err := normalizeDownloadIntentPlanFields(invalid); !errors.Is(err, ErrInvalidDownloadIntentPlan) {
			t.Fatalf("invalid=%+v err=%v", invalid, err)
		}
	}
}

func TestScanDownloadIntentTVFactsDistinguishesLegacyCoverage(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(`[]`), []byte(`[{"season":1,"episode":2}]`), []byte(`null`), []byte(`{}`), []byte(`[{"season":1,"episode":0}]`)} {
		row := downloadIntentPlanRowFunc(func(dest ...any) error {
			*dest[0].(*uuid.UUID) = uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
			*dest[1].(*int64) = 1
			*dest[2].(*string) = downloadIntentPlanTestDownloaderID
			*dest[3].(*string) = "haidan"
			*dest[4].(*int16) = 1
			*dest[5].(*string) = "quality"
			*dest[9].(*time.Time) = time.Now()
			*dest[12].(*[]byte) = raw
			return nil
		})
		got, err := NewStore(nil, mustDownloadIntentPlanTestSecretBox(t)).scanDownloadIntentPlan(row)
		valid := string(raw) == "" || string(raw) == "[]" || string(raw) == `[{"season":1,"episode":2}]`
		if valid && err != nil || !valid && err == nil {
			t.Fatalf("raw=%s result=%+v err=%v", raw, got, err)
		}
		if valid && (got.TVCoverage == nil) != (raw == nil) {
			t.Fatal("lost nullable coverage")
		}
	}
}

func TestTVQualityAcceptanceWriterProgressCompletion(t *testing.T) {
	e1, e2 := subscription.Episode{Season: 1, Episode: 1}, subscription.Episode{Season: 1, Episode: 2}
	for _, tc := range []struct {
		name        string
		best, full  bool
		current     *int
		priorities  map[string]any
		note        []int
		quality     *int
		actual      []subscription.Episode
		consumed    []subscription.Episode
		confirmed   bool
		missing     int64
		want        subscription.State
		wantCurrent *int
		wantLack    int
		wantNote    []int
		wantQuality map[string]any
	}{
		{name: "low quality any-version progress", best: true, quality: r08MoviePriority(90), actual: []subscription.Episode{e1}, want: subscription.Active, wantCurrent: r08MoviePriority(0), wantLack: 1, wantNote: []int{1}, wantQuality: map[string]any{"1": float64(90)}},
		{name: "all targets exact100", best: true, priorities: map[string]any{"1": 90, "2": 100}, quality: r08MoviePriority(100), actual: []subscription.Episode{e1}, want: subscription.Completed, wantCurrent: r08MoviePriority(100), wantNote: []int{1}, wantQuality: map[string]any{"1": float64(100), "2": float64(100)}},
		{name: "101 stays R", best: true, quality: r08MoviePriority(101), actual: []subscription.Episode{e1, e2}, confirmed: true, want: subscription.Active, wantCurrent: r08MoviePriority(101), wantNote: []int{1, 2}, wantQuality: map[string]any{"1": float64(101), "2": float64(101)}},
		{name: "lower replay preserves quality", best: true, priorities: map[string]any{"1": 100, "2": 90}, quality: r08MoviePriority(80), actual: []subscription.Episode{e1, e2}, want: subscription.Active, wantCurrent: r08MoviePriority(90), wantNote: []int{1, 2}, wantQuality: map[string]any{"1": float64(100), "2": float64(90)}},
		{name: "negative quality still actual note", best: true, quality: r08MoviePriority(-1), actual: []subscription.Episode{e1}, want: subscription.Active, wantCurrent: r08MoviePriority(-1), wantLack: 1, wantNote: []int{1}, wantQuality: map[string]any{"1": float64(-1)}},
		{name: "full100 actual confirmed", best: true, full: true, current: r08MoviePriority(90), quality: r08MoviePriority(100), actual: []subscription.Episode{e1, e2}, confirmed: true, want: subscription.Completed, wantCurrent: r08MoviePriority(100), wantNote: []int{1, 2}, wantQuality: map[string]any{"1": float64(100), "2": float64(100)}},
		{name: "V2 full confirmed partial completes", best: true, full: true, current: r08MoviePriority(90), quality: r08MoviePriority(100), actual: []subscription.Episode{e1}, confirmed: true, want: subscription.Completed, wantCurrent: r08MoviePriority(100), wantLack: 1, wantNote: []int{1}, wantQuality: map[string]any{"1": float64(100)}},
		{name: "full no confirmation", best: true, full: true, current: r08MoviePriority(90), quality: r08MoviePriority(100), actual: []subscription.Episode{e1, e2}, want: subscription.Active, wantCurrent: r08MoviePriority(90), wantNote: []int{1, 2}, wantQuality: map[string]any{"1": float64(100), "2": float64(100)}},
		{name: "full same priority", best: true, full: true, current: r08MoviePriority(101), quality: r08MoviePriority(100), actual: []subscription.Episode{e1, e2}, confirmed: true, want: subscription.Active, wantCurrent: r08MoviePriority(101), wantNote: []int{1, 2}, wantQuality: map[string]any{"1": float64(100), "2": float64(100)}},
		{name: "known empty never consumes bound guesses", best: true, quality: r08MoviePriority(100), actual: []subscription.Episode{}, consumed: []subscription.Episode{e1, e2}, confirmed: false, want: subscription.Active, wantCurrent: r08MoviePriority(0), wantLack: 2},
		{name: "legacy plan no quality inference", best: true, consumed: []subscription.Episode{e1}, want: subscription.Active, wantCurrent: r08MoviePriority(0), wantLack: 1, wantNote: []int{1}},
		{name: "V2 writer current target numbers", best: true, quality: r08MoviePriority(100), actual: []subscription.Episode{{Season: 2, Episode: 1}, {Season: 1, Episode: 3}}, confirmed: true, want: subscription.Active, wantCurrent: r08MoviePriority(0), wantLack: 1, wantNote: []int{1}, wantQuality: map[string]any{"1": float64(100)}},
		{name: "ordinary quality without overall derivation", current: r08MoviePriority(30), quality: r08MoviePriority(90), actual: []subscription.Episode{e1}, missing: 1, want: subscription.PendingSearch, wantCurrent: nil, wantLack: 1, wantNote: []int{1}, wantQuality: map[string]any{"1": float64(90)}},
		{name: "ordinary missing driven by rows", quality: r08MoviePriority(100), actual: []subscription.Episode{e1}, missing: 0, want: subscription.Completed, wantLack: 0, wantNote: []int{1}, wantQuality: map[string]any{"1": float64(100)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			season := 1
			config := map[string]any{"best_version": tc.best, "best_version_full": tc.full, "total_episode": 2, "lack_episode": 2, "episode_priority": tc.priorities, "note": tc.note}
			if tc.current != nil {
				config["current_priority"] = *tc.current
			}
			original, _ := json.Marshal(config)
			next, patch, err := buildTVQualityAcceptancePatch(downloadIntentAcceptanceSubscription{mediaType: "电视剧", season: &season, config: config}, tc.consumed, tc.missing, downloadIntentTVFacts{priority: tc.quality, coverage: tc.actual, fullCoverage: tc.confirmed})
			if err != nil || next != tc.want {
				t.Fatalf("state=%s patch=%s err=%v", next, patch, err)
			}
			var got map[string]any
			if err := json.Unmarshal(patch, &got); err != nil {
				t.Fatal(err)
			}
			if got["lack_episode"] != float64(tc.wantLack) {
				t.Fatalf("lack=%v patch=%s", got["lack_episode"], patch)
			}
			if tc.wantCurrent == nil {
				if _, exists := got["current_priority"]; exists {
					t.Fatal("ordinary overall priority changed")
				}
			} else if got["current_priority"] != float64(*tc.wantCurrent) {
				t.Fatalf("current patch=%s", patch)
			}
			var notes []int
			data, _ := json.Marshal(got["note"])
			_ = json.Unmarshal(data, &notes)
			if len(notes) != len(tc.wantNote) || len(notes) > 0 && !reflect.DeepEqual(notes, tc.wantNote) {
				t.Fatalf("note=%v want=%v", notes, tc.wantNote)
			}
			if tc.wantQuality != nil && !reflect.DeepEqual(got["episode_priority"], tc.wantQuality) {
				t.Fatalf("quality=%#v want=%#v", got["episode_priority"], tc.wantQuality)
			}
			if tc.quality == nil {
				if _, exists := got["episode_priority"]; exists {
					t.Fatal("legacy quality invented")
				}
			}
			after, _ := json.Marshal(config)
			if string(original) != string(after) {
				t.Fatal("writer mutated parent config")
			}
		})
	}
}
