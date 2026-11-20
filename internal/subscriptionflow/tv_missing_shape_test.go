package subscriptionflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestOrdinaryTVMissingShapeMatchesFrozenV2(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Sources []struct{ Path, SHA256 string }
		Cases   []struct {
			Name          string
			Compatibility string
			Input         struct {
				LibrarySeasons map[int][]int `json:"library_seasons"`
				Subscribe      struct {
					Season       int
					StartEpisode int `json:"start_episode"`
					Note         []int
				}
			}
			Stages struct {
				EffectiveTotal struct{ Expected int } `json:"effective_total"`
			}
			Expected struct {
				Shape    string
				NoExists map[string]map[int]struct {
					Episodes     []int
					StartEpisode int `json:"start_episode"`
					TotalEpisode int `json:"total_episode"`
				} `json:"no_exists"`
			}
		}
	}
	contracts := filepath.Join("..", "..", "testdata", "contracts")
	payload, err := os.ReadFile(filepath.Join(contracts, "fixtures", "r08", "tv-missing-shape-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, source := range fixture.Sources {
		payload, err := os.ReadFile(filepath.Join(contracts, filepath.FromSlash(source.Path)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		if hex.EncodeToString(digest[:]) != source.SHA256 {
			t.Fatalf("frozen source changed: %s", source.Path)
		}
	}
	nativeCases := 0
	for _, tc := range fixture.Cases {
		// Synthetic season numbers and unresolved metadata have separate source
		// evidence. This test asserts the native count-based production inputs.
		if tc.Compatibility != "native_count_expansion" {
			continue
		}
		nativeCases++
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			resolution, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{
				Season: tc.Input.Subscribe.Season, StartEpisode: tc.Input.Subscribe.StartEpisode,
				TotalEpisode: tc.Stages.EffectiveTotal.Expected,
				Note:         append([]int{}, tc.Input.Subscribe.Note...),
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, resolution.TargetEpisodes)
			fixture.target.Season = intPointerCopy(tc.Input.Subscribe.Season)
			fixture.target.Config["total_episode"] = resolution.Snapshot.TotalEpisode
			fixture.target.Config["start_episode"] = resolution.Snapshot.StartEpisode
			fixture.target.Config["note"] = append([]int{}, tc.Input.Subscribe.Note...)
			source := &existingEpisodesSourceFake{}
			for season, episodes := range tc.Input.LibrarySeasons {
				source.episodes = append(source.episodes, v2TVTestEpisodes(season, episodes...)...)
			}
			fixture.handler.SetExistingEpisodesSource(source)
			query, _, _, err := fixture.handler.withCurrentTVMissingEpisodes(
				t.Context(), fixture.target, resolution, false, fixture.query,
			)
			if err != nil {
				t.Fatal(err)
			}
			want := []subscription.Episode{}
			if need, ok := tc.Expected.NoExists["tmdb:123"][tc.Input.Subscribe.Season]; ok {
				if tc.Expected.Shape == "whole_season" {
					for episode := need.StartEpisode; episode <= need.TotalEpisode; episode++ {
						want = append(want, subscription.Episode{Season: tc.Input.Subscribe.Season, Episode: episode})
					}
				} else {
					want = append(want, v2TVTestEpisodes(tc.Input.Subscribe.Season, need.Episodes...)...)
				}
			}
			if !slices.Equal(query.MissingEpisodes, want) ||
				query.FullSeasonMissing[tc.Input.Subscribe.Season] != (tc.Expected.Shape == "whole_season") {
				t.Fatalf("query=%+v want episodes=%v shape=%s", query, want, tc.Expected.Shape)
			}
			if source.calls > 1 || len(fixture.store.upserted) != 0 ||
				!reflect.DeepEqual(fixture.target.Config["note"], append([]int{}, tc.Input.Subscribe.Note...)) {
				t.Fatalf("lookup mutated facts or repeated: calls=%d upserted=%v config=%v",
					source.calls, fixture.store.upserted, fixture.target.Config)
			}
		})
	}
	if nativeCases != 20 {
		t.Fatalf("native cases=%d want 20; review fixture coverage", nativeCases)
	}
}

func TestOrdinaryTVMissingShapeSelectsOriginalStage(t *testing.T) {
	t.Parallel()
	for _, rss := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			start   int
			note    []int
			library []subscription.Episode
			full    bool
		}{
			{name: "整季空事实", start: 1, full: true},
			{name: "范围外下载记录", start: 1, note: []int{99}},
			{name: "零集号历史记录", start: 1, note: []int{0}},
			{name: "负集号历史记录", start: 1, note: []int{-1}},
			{name: "超上限历史记录", start: 1, note: []int{5001}},
			{name: "同季范围外库事实", start: 1, library: v2TVTestEpisodes(1, 99)},
			{name: "其他季库事实", start: 1, library: v2TVTestEpisodes(2, 99), full: true},
			{name: "起点裁剪", start: 2},
		} {
			entry := "精准"
			if rss {
				entry = "RSS"
			}
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				resolution, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{
					Season: 1, StartEpisode: tc.start, TotalEpisode: 3,
					Note: append([]int{}, tc.note...),
				})
				if err != nil {
					t.Fatal(err)
				}
				fixture := newV2TVExecutionFixture(t, subscription.Active, resolution.TargetEpisodes)
				fixture.target.Config["total_episode"] = 3
				fixture.target.Config["start_episode"] = tc.start
				fixture.target.Config["note"] = tc.note
				fixture.adapter.body = v2TVTestMetainfo(t, "Show.S01E01.mkv", "Show.S01E02.mkv", "Show.S01E03.mkv")
				fixture.handler.SetExistingEpisodesSource(&existingEpisodesSourceFake{episodes: tc.library})
				query, _, _, err := fixture.handler.withCurrentTVMissingEpisodes(
					t.Context(), fixture.target, resolution, false, fixture.query,
				)
				if err != nil {
					t.Fatal(err)
				}
				// A season-only title takes the unpack path when the missing list
				// is explicit, even when the explicit list covers the full range.
				got, err := fixture.handler.executeV2TVCandidates(t.Context(), fixture.target, query,
					[]subscription.Candidate{v2TVExecutionCandidate("shape-stage", 1)}, []indexer.Adapter{fixture.adapter}, rss)
				if err != nil || len(fixture.submission.calls) != 1 || len(got.AcceptedCandidateKeys) != 1 {
					t.Fatalf("result=%+v stages=%+v err=%v", got, fixture.submission.calls, err)
				}
				call := fixture.submission.calls[0]
				if call.plan.Paused == tc.full || !reflect.DeepEqual(call.episodes, query.MissingEpisodes) ||
					len(got.RemainingEpisodes) != 0 {
					t.Fatalf("stage paused=%v episodes=%v remaining=%v full=%v", call.plan.Paused,
						call.episodes, got.RemainingEpisodes, tc.full)
				}
			})
		}
	}
}

func TestV2TVExplicitFullRangeDoesNotInferWholeSeason(t *testing.T) {
	t.Parallel()
	fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2, 3))
	fixture.query.FullSeasonMissing = nil
	needs, err := v2TVSeasonNeeds(fixture.target, fixture.query)
	if err != nil || len(needs) != 1 || needs[0].FullSeason {
		t.Fatalf("explicit needs=%+v err=%v", needs, err)
	}
}

func TestOrdinaryTVInFlightEpisodeClearsWholeSeasonShape(t *testing.T) {
	t.Parallel()
	for _, state := range []subscription.SubscriptionEpisodeState{
		subscription.SubscriptionEpisodeAcquired,
		subscription.SubscriptionEpisodeDownloading,
		subscription.SubscriptionEpisodePendingOrganization,
		subscription.SubscriptionEpisodeOrganizing,
		subscription.SubscriptionEpisodePendingIndex,
		subscription.SubscriptionEpisodeIndexed,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.PendingSearch, v2TVTestEpisodes(1, 1, 2))
			fixture.store.episodes[0].State = state
			resolution, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{Season: 1, TotalEpisode: 2})
			if err != nil {
				t.Fatal(err)
			}
			query, _, _, err := fixture.handler.withCurrentTVMissingEpisodes(
				t.Context(), fixture.target, resolution, false, fixture.query,
			)
			if err != nil || query.FullSeasonMissing[1] || !reflect.DeepEqual(query.MissingEpisodes, v2TVTestEpisodes(1, 2)) {
				t.Fatalf("query=%+v err=%v", query, err)
			}
		})
	}
}

func TestCurrentTVMissingQueryRebuildsWholeSeasonShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		best   bool
		legacy bool
		total  int
	}{
		{name: "洗版清除普通形状", best: true, total: 2},
		{name: "未知目标清除旧形状", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newV2TVExecutionFixture(t, subscription.Active, v2TVTestEpisodes(1, 1, 2))
			fixture.target.Config["best_version"] = tc.best
			resolution, err := subscription.DescribeTVTarget(subscription.TVTargetSnapshot{
				Season: 1, TotalEpisode: tc.total, BestVersion: tc.best,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture.query.FullSeasonMissing = map[int]bool{1: true, 2: true}
			source := &existingEpisodesSourceFake{}
			fixture.handler.SetExistingEpisodesSource(source)
			query, _, _, err := fixture.handler.withCurrentTVMissingEpisodes(
				t.Context(), fixture.target, resolution, tc.legacy, fixture.query,
			)
			if err != nil || len(query.FullSeasonMissing) != 0 || query.FullSeasonMissing == nil {
				t.Fatalf("query=%+v err=%v", query, err)
			}
			if tc.best && source.calls != 0 {
				t.Fatalf("best-version query read ordinary library: %d", source.calls)
			}
		})
	}
}
