package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func recoveredTargetFixture(expected ...int) *subscription.TVRecoveredTarget {
	target := &subscription.TVRecoveredTarget{TotalEpisode: 3,
		MissingEpisodes: []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}}}
	for _, number := range expected {
		target.ExpectedCoverage = append(target.ExpectedCoverage, subscription.Episode{Season: 1, Episode: number})
	}
	return target
}

func TestDownloadIntentRecoveredTargetIsImmutableAndIsolated(t *testing.T) {
	t.Parallel()
	base := DownloadIntentPlan{IntentID: uuid.New(), SiteID: 1, DownloaderID: downloadIntentPlanTestDownloaderID,
		ResolverKind: "haidan", ResolverVersion: 1, ResourceID: "recovered", TVPriority: r08MoviePriority(90),
		TVCoverage: []subscription.Episode{{Season: 1, Episode: 101}}, TVRecoveredTarget: recoveredTargetFixture(1)}
	got, err := normalizeDownloadIntentPlanFields(base)
	if err != nil {
		t.Fatal(err)
	}
	base.TVRecoveredTarget.TotalEpisode = 2
	base.TVRecoveredTarget.MissingEpisodes[0].Episode = 2
	base.TVRecoveredTarget.ExpectedCoverage[0].Episode = 2
	if !reflect.DeepEqual(got.TVRecoveredTarget, recoveredTargetFixture(1)) {
		t.Fatal("recovered target aliases caller memory")
	}
	for _, tc := range []struct {
		name   string
		change func(*DownloadIntentPlan)
	}{
		{"legacy has no recovered target", func(p *DownloadIntentPlan) { p.TVRecoveredTarget = nil }},
		{"different missing facts", func(p *DownloadIntentPlan) {
			p.TVRecoveredTarget.MissingEpisodes = p.TVRecoveredTarget.MissingEpisodes[:2]
		}},
		{"different consumed coverage", func(p *DownloadIntentPlan) {
			p.TVRecoveredTarget.ExpectedCoverage = []subscription.Episode{{Season: 1, Episode: 2}}
		}},
		{"different true total", func(p *DownloadIntentPlan) { p.TVRecoveredTarget.TotalEpisode = 4 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			other := got
			other.TVRecoveredTarget, _ = subscription.NormalizeTVRecoveredTarget(got.TVRecoveredTarget)
			tc.change(&other)
			if sameDownloadIntentPlan(got, other) {
				t.Fatal("changed recovered facts treated as immutable replay")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*DownloadIntentPlan)
	}{
		{"movie plan", func(p *DownloadIntentPlan) { p.MoviePriority = r08MoviePriority(90) }},
		{"unknown quality priority", func(p *DownloadIntentPlan) { p.TVPriority = nil }},
		{"unknown quality coverage", func(p *DownloadIntentPlan) { p.TVCoverage = nil; p.TVPriority = nil }},
		{"unresolved recovered total", func(p *DownloadIntentPlan) { p.TVRecoveredTarget.TotalEpisode = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			other := got
			other.TVRecoveredTarget, _ = subscription.NormalizeTVRecoveredTarget(got.TVRecoveredTarget)
			tc.change(&other)
			if _, err := normalizeDownloadIntentPlanFields(other); !errors.Is(err, ErrInvalidDownloadIntentPlan) {
				t.Fatalf("invalid recovered plan error=%v", err)
			}
		})
	}
}

func TestScanDownloadIntentRecoveredTargetPreservesNullableBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		raw     []byte
		want    *subscription.TVRecoveredTarget
		invalid bool
	}{
		{name: "legacy SQL null"},
		{name: "recovered target", raw: encodeDownloadIntentTVRecoveredTarget(recoveredTargetFixture(1)), want: recoveredTargetFixture(1)},
		{name: "JSON null is corrupt", raw: []byte(`null`), invalid: true},
		{name: "empty object is corrupt", raw: []byte(`{}`), invalid: true},
		{name: "wrong JSON shape", raw: []byte(`[]`), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := downloadIntentPlanRowFunc(func(dest ...any) error {
				*dest[0].(*uuid.UUID) = uuid.New()
				*dest[1].(*int64) = 1
				*dest[2].(*string) = downloadIntentPlanTestDownloaderID
				*dest[3].(*string) = "haidan"
				*dest[4].(*int16) = 1
				*dest[5].(*string) = "recovered"
				if tc.raw != nil {
					*dest[11].(**int) = r08MoviePriority(90)
				}
				*dest[12].(*[]byte) = []byte(`[]`)
				*dest[14].(*[]byte) = tc.raw
				return nil
			})
			got, err := NewStore(nil, mustDownloadIntentPlanTestSecretBox(t)).scanDownloadIntentPlan(row)
			if tc.invalid != (err != nil) || !tc.invalid && !reflect.DeepEqual(got.TVRecoveredTarget, tc.want) {
				t.Fatalf("target=%+v err=%v", got.TVRecoveredTarget, err)
			}
		})
	}
}

func TestRecoveredTVSubmissionUsesConsumptionIndependentlyOfQualityCoverage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*tvSubmissionRow, *[]subscription.Episode)
		want   error
	}{
		{name: "count-only full season metadata differs"},
		{name: "requested must exactly match consumption", change: func(_ *tvSubmissionRow, requested *[]subscription.Episode) {
			*requested = []subscription.Episode{{Season: 1, Episode: 2}}
		}, want: ErrSubscriptionEpisodeBindingConflict},
		{name: "quality still matches immutable success input", change: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.success = `[]` }, want: ErrSubscriptionEpisodeBindingConflict},
		{name: "positive configured total keeps existing path", change: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.config = `{"total_episode":3}` }, want: ErrSubscriptionEpisodeBindingConflict},
		{name: "best version is not recovered ordinary", change: func(row *tvSubmissionRow, _ *[]subscription.Episode) {
			row.config = `{"total_episode":0,"best_version":true}`
		}, want: ErrSubscriptionEpisodeBindingConflict},
		{name: "corrupt recovered facts reject", change: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.recovered = `null` }, want: ErrSubscriptionEpisodePersistence},
		{name: "paused parent rejects", change: func(row *tvSubmissionRow, _ *[]subscription.Episode) { row.state, row.v2 = subscription.Paused, "S" }, want: ErrSubscriptionEpisodeBindingConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := newTVSubmissionRow()
			row.config = `{"total_episode":0}`
			row.plan = `[{"season":1,"episode":101}]`
			row.success = row.plan
			row.recovered = string(encodeDownloadIntentTVRecoveredTarget(recoveredTargetFixture(1)))
			requested := []subscription.Episode{{Season: 1, Episode: 1}}
			if tc.change != nil {
				tc.change(&row, &requested)
			}
			independent, err := verifyTVBestVersionSubmission(t.Context(), &tvSubmissionTestQuery{row: row}, "owner", 42, uuid.New(), requested, nil)
			if !errors.Is(err, tc.want) || independent != (tc.want == nil) {
				t.Fatalf("independent=%v err=%v want=%v", independent, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name            string
		state, expected subscription.State
		allowed         bool
	}{
		{"precise claimed replay", subscription.Acquired, subscription.PendingSearch, true},
		{"RSS P replay", subscription.PendingIdentification, subscription.PendingIdentification, true},
		{"precise cannot replay P", subscription.PendingIdentification, subscription.PendingSearch, false},
		{"released intent cannot reclaim R", subscription.Active, subscription.Active, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := newTVSubmissionRow()
			row.config, row.state = `{"total_episode":0}`, tc.state
			row.v2, _ = subscription.V2State(tc.state)
			row.recovered = string(encodeDownloadIntentTVRecoveredTarget(recoveredTargetFixture(1)))
			independent, err := verifyTVBestVersionSubmission(t.Context(), &tvSubmissionTestQuery{row: row}, "owner", 42, uuid.New(), recoveredTargetFixture(1).ExpectedCoverage, &tc.expected)
			if independent != tc.allowed || tc.allowed && err != nil || !tc.allowed && !errors.Is(err, ErrSubscriptionEpisodeBindingConflict) {
				t.Fatalf("independent=%v err=%v", independent, err)
			}
		})
	}
}

func TestRecoveredTVAcceptanceUsesLeftsWithoutPublishingWriterFacts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		expected []int
		want     subscription.State
		lack     int
	}{
		{"partial returns N", []int{1}, subscription.PendingSearch, 2},
		{"all missing completes", []int{1, 2, 3}, subscription.Completed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := map[string]any{"total_episode": 0, "note": []int{9}, "episode_priority": map[string]int{"9": 90}, "current_priority": 90}
			before, _ := json.Marshal(config)
			parent := downloadIntentAcceptanceSubscription{mediaType: "tv", config: config}
			facts := downloadIntentTVFacts{priority: r08MoviePriority(100), coverage: []subscription.Episode{{Season: 1, Episode: 101}}, recovered: recoveredTargetFixture(tc.expected...)}
			next, raw, err := buildDownloadIntentAcceptanceSettlement(t.Context(), nil, 42, parent, []subscription.Episode{{Season: 1, Episode: 99}}, nil, facts)
			var patch map[string]any
			_ = json.Unmarshal(raw, &patch)
			after, _ := json.Marshal(config)
			if err != nil || next != tc.want || !reflect.DeepEqual(patch, map[string]any{"lack_episode": float64(tc.lack)}) || string(before) != string(after) {
				t.Fatalf("next=%s patch=%v err=%v config=%v", next, patch, err, config)
			}
		})
	}
}

type recoveredTargetCountTx struct {
	pgx.Tx
	query string
	args  []any
}

func (tx *recoveredTargetCountTx) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	tx.query, tx.args = query, args
	return downloadIntentPlanRowFunc(func(dest ...any) error { *dest[0].(*int64) = 1; return nil })
}

func TestRecoveredTVAcceptanceUsesCurrentPositiveTargetAfterPublicChange(t *testing.T) {
	t.Parallel()
	parent := downloadIntentAcceptanceSubscription{mediaType: "tv", config: map[string]any{"total_episode": 2}}
	facts := downloadIntentTVFacts{priority: r08MoviePriority(90), coverage: []subscription.Episode{{Season: 1, Episode: 101}}, recovered: recoveredTargetFixture(1)}
	tx := &recoveredTargetCountTx{}
	next, raw, err := buildDownloadIntentAcceptanceSettlement(t.Context(), tx, 42, parent, facts.recovered.ExpectedCoverage, nil, facts)
	var patch map[string]any
	_ = json.Unmarshal(raw, &patch)
	if err != nil || next != subscription.PendingSearch || patch["lack_episode"] != float64(1) ||
		tx.query != countMissingCurrentTargetEpisodesAfterCoverageSQL || !reflect.DeepEqual(tx.args[3], []int32{1}) {
		t.Fatalf("next=%s patch=%v query=%s args=%v err=%v", next, patch, tx.query, tx.args, err)
	}
	if _, ok := patch["note"]; ok {
		t.Fatal("unrelated metadata coverage wrote note")
	}
}

func TestRecoveredTVAcceptanceProjectsCurrentSeasonWithoutDiscardingOtherSeasonLefts(t *testing.T) {
	t.Parallel()
	season := 2
	parent := downloadIntentAcceptanceSubscription{mediaType: "tv", season: &season, config: map[string]any{"total_episode": 0}}
	facts := downloadIntentTVFacts{coverage: []subscription.Episode{{Season: 1, Episode: 1}}, recovered: recoveredTargetFixture(1)}
	next, raw, err := buildDownloadIntentAcceptanceSettlement(t.Context(), nil, 42, parent, nil, nil, facts)
	var patch map[string]any
	_ = json.Unmarshal(raw, &patch)
	if err != nil || next != subscription.PendingSearch || !reflect.DeepEqual(patch, map[string]any{"lack_episode": float64(0)}) {
		t.Fatalf("next=%s patch=%v err=%v", next, patch, err)
	}
	parent.config["total_episode"] = 3
	tx := &recoveredTargetCountTx{}
	if _, _, err := buildDownloadIntentAcceptanceSettlement(t.Context(), tx, 42, parent, facts.recovered.ExpectedCoverage, nil, facts); err != nil ||
		!reflect.DeepEqual(tx.args[3], []int32{}) {
		t.Fatalf("positive target received another season's proven coverage: args=%v err=%v", tx.args, err)
	}
}

func TestRecoveredTVPlanMigrationPreservesFactsOnRollback(t *testing.T) {
	t.Parallel()
	raw, err := migrationFS.ReadFile("migrations/040_download_intent_tv_recovered_target.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"tv_recovered_target JSONB", "jsonb_typeof(tv_recovered_target) = 'object'", "tv_priority IS NOT NULL AND tv_coverage IS NOT NULL AND movie_priority IS NULL", "WHERE tv_recovered_target IS NOT NULL", "RAISE EXCEPTION"} {
		if !strings.Contains(string(raw), required) {
			t.Fatalf("missing migration boundary %q", required)
		}
	}
}

func TestRecordRecoveredTVProgressStateMatrixAndCancellation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state subscription.State
		lack  int
		want  subscription.State
	}{
		{"N lefts become R", subscription.PendingSearch, 2, subscription.Active},
		{"R lefts stay R", subscription.Active, 2, subscription.Active},
		{"P lefts stay P", subscription.PendingIdentification, 2, subscription.PendingIdentification},
		{"N zero completes", subscription.PendingSearch, 0, subscription.Completed},
		{"R zero completes", subscription.Active, 0, subscription.Completed},
		{"P zero stays P", subscription.PendingIdentification, 0, subscription.PendingIdentification},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			version := time.Now()
			query, args, err := buildRecordRecoveredTVSubscriptionProgress(" owner ", 42, tc.state, version, tc.lack)
			if err != nil || query != recordRecoveredTVSubscriptionProgressSQL || args[1] != "owner" || args[6] != tc.want {
				t.Fatalf("args=%v err=%v", args, err)
			}
			var patch map[string]any
			_ = json.Unmarshal(args[5].([]byte), &patch)
			if !reflect.DeepEqual(patch, map[string]any{"lack_episode": float64(tc.lack)}) {
				t.Fatalf("patch=%v", patch)
			}
		})
	}
	var store *Store
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.RecordRecoveredTVSubscriptionProgress(ctx, "owner", 42, subscription.PendingSearch, time.Now(), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if err := store.RecordRecoveredTVSubscriptionProgress(t.Context(), "owner", 42, subscription.Paused, time.Now(), 1); !errors.Is(err, ErrInvalidSubscriptionPreciseSearch) {
		t.Fatalf("paused=%v", err)
	}
	if _, _, err := buildRecordRecoveredTVSubscriptionProgress("owner", 42, subscription.Active, time.Now(), -1); !errors.Is(err, ErrInvalidSubscriptionPreciseSearch) {
		t.Fatalf("negative lack=%v", err)
	}
}
