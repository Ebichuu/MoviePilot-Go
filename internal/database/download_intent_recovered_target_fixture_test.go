package database

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

type recoveredFixtureNeed struct {
	Season   int   `json:"season"`
	Episodes []int `json:"episodes"`
	Total    int   `json:"total_episode"`
	Start    int   `json:"start_episode"`
}

func recoveredFixtureEpisodes(container map[string]map[string]recoveredFixtureNeed) []subscription.Episode {
	result := []subscription.Episode{}
	for _, seasons := range container {
		for _, need := range seasons {
			numbers := need.Episodes
			if len(numbers) == 0 {
				for episode := need.Start; episode <= need.Total; episode++ {
					numbers = append(numbers, episode)
				}
			}
			for _, number := range numbers {
				result = append(result, subscription.Episode{Season: need.Season, Episode: number})
			}
		}
	}
	return result
}

func TestRecoveredTVAcceptedSettlementMatchesFrozenV2WriterAndProgress(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../testdata/contracts/fixtures/r08/tv-recovered-target-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name  string `json:"name"`
			Input struct {
				State       string         `json:"state"`
				ConfigTotal int            `json:"total_episode"`
				Start       int            `json:"start_episode"`
				Note        []int          `json:"note"`
				Priorities  map[string]int `json:"episode_priority"`
				Current     int            `json:"current_priority"`
				Cancel      bool           `json:"completion_cancel"`
			} `json:"input"`
			Expected struct {
				Resolution struct {
					NoExists map[string]map[string]recoveredFixtureNeed `json:"no_exists"`
				} `json:"resolution"`
				Lefts     map[string]map[string]recoveredFixtureNeed `json:"lefts"`
				Downloads []struct {
					Meta     []int `json:"meta_episodes"`
					Selected []int `json:"selected_episodes"`
				} `json:"downloads"`
				Lack       int            `json:"lack_episode"`
				Note       []int          `json:"note"`
				Priorities map[string]int `json:"episode_priority"`
				Current    int            `json:"current_priority"`
				Completed  bool           `json:"completed"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	tested := 0
	for _, tc := range corpus.Cases {
		if len(tc.Expected.Downloads) != 1 || tc.Input.Cancel {
			continue
		}
		missing := recoveredFixtureEpisodes(tc.Expected.Resolution.NoExists)
		if len(missing) == 0 {
			continue
		}
		total := 0
		for _, seasons := range tc.Expected.Resolution.NoExists {
			for _, need := range seasons {
				total = need.Total
			}
		}
		// 此处以原版 lefts 给出数据库边界的已消费事实；选择器等价由入口测试验证。
		remaining := recoveredFixtureEpisodes(tc.Expected.Lefts)
		consumed := []subscription.Episode{}
		for _, episode := range missing {
			found := false
			for _, left := range remaining {
				if episode == left {
					found = true
					break
				}
			}
			if !found {
				consumed = append(consumed, episode)
			}
		}
		if len(consumed) == 0 {
			continue
		}
		tested++
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			config := map[string]any{"total_episode": tc.Input.ConfigTotal, "start_episode": tc.Input.Start, "note": tc.Input.Note, "episode_priority": tc.Input.Priorities, "current_priority": tc.Input.Current}
			parent := downloadIntentAcceptanceSubscription{mediaType: "tv", config: config}
			quality := []subscription.Episode{}
			for _, episode := range tc.Expected.Downloads[0].Selected {
				quality = append(quality, subscription.Episode{Season: 1, Episode: episode})
			}
			facts := downloadIntentTVFacts{priority: r08MoviePriority(90), coverage: quality, recovered: &subscription.TVRecoveredTarget{TotalEpisode: total, MissingEpisodes: missing, ExpectedCoverage: consumed}}
			next, patchJSON, err := buildDownloadIntentAcceptanceSettlement(t.Context(), nil, 42, parent, nil, nil, facts)
			var patch map[string]any
			_ = json.Unmarshal(patchJSON, &patch)
			if err != nil || !reflect.DeepEqual(patch, map[string]any{"lack_episode": float64(tc.Expected.Lack)}) {
				t.Fatalf("patch=%v err=%v", patch, err)
			}
			completed := next == subscription.Completed && tc.Input.State != "P"
			if completed != tc.Expected.Completed || !reflect.DeepEqual(config["note"], tc.Expected.Note) ||
				!reflect.DeepEqual(config["episode_priority"], tc.Expected.Priorities) || config["current_priority"] != tc.Expected.Current || config["total_episode"] != 0 {
				t.Fatalf("next=%s config=%v completed=%v", next, config, completed)
			}
		})
	}
	if tested == 0 {
		t.Fatal("frozen corpus did not exercise an accepted download")
	}
}
