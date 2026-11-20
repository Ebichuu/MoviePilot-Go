package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

type r08FactsFixture struct {
	Normalization []struct {
		Name     string      `json:"name"`
		Input    any         `json:"input"`
		Expected map[int]int `json:"expected"`
	} `json:"normalization"`
	Facts []struct {
		Name     string           `json:"name"`
		Input    TVTargetSnapshot `json:"input"`
		Expected struct {
			Priorities      map[int]int `json:"episode_priority"`
			Downloaded      []int       `json:"downloaded"`
			Pending         []int       `json:"pending"`
			CurrentPriority int         `json:"current_priority"`
			Complete        bool        `json:"complete"`
			Lack            *int        `json:"lack_episode"`
		} `json:"expected"`
	} `json:"facts"`
}

func readR08FactsFixture(t *testing.T) r08FactsFixture {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve fixture location")
	}
	payload, err := os.ReadFile(filepath.Join(filepath.Dir(current), "..", "..", "testdata", "contracts", "fixtures", "r08", "shared-facts-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture r08FactsFixture
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Normalization) == 0 || len(fixture.Facts) == 0 {
		t.Fatal("empty V2 fixture")
	}
	return fixture
}

func TestDescribeTVBestVersionMatchesFrozenV2(t *testing.T) {
	t.Parallel()
	fixture := readR08FactsFixture(t)
	for _, test := range fixture.Facts {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, err := DescribeTVBestVersion(test.Input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.EpisodePriority, test.Expected.Priorities) ||
				!reflect.DeepEqual(r08EpisodeNumbers(got.DownloadedEpisodes), test.Expected.Downloaded) ||
				!reflect.DeepEqual(r08EpisodeNumbers(got.PendingEpisodes), test.Expected.Pending) ||
				got.CurrentPriority != test.Expected.CurrentPriority || got.Complete != test.Expected.Complete {
				t.Fatalf("facts=%+v want=%+v", got, test.Expected)
			}
			if test.Expected.Lack != nil && bestVersionTVLackEpisode(test.Input) != *test.Expected.Lack {
				t.Fatalf("lack=%d want=%d", bestVersionTVLackEpisode(test.Input), *test.Expected.Lack)
			}
		})
	}
}

func r08EpisodeNumbers(episodes []Episode) []int {
	numbers := make([]int, 0, len(episodes))
	for _, episode := range episodes {
		numbers = append(numbers, episode.Episode)
	}
	return numbers
}

func TestPendingTVBestVersionEpisodesExplicitEmptyOverride(t *testing.T) {
	t.Parallel()
	snapshot := TVTargetSnapshot{
		Season: 1, TotalEpisode: 2, BestVersion: true, CurrentPriority: intPtr(100),
		EpisodePriority: map[int]int{1: 100, 2: 100},
	}
	tests := []struct {
		name     string
		override map[int]int
		want     []int
	}{
		{"nil inherits", nil, []int{}},
		{"empty stays empty", map[int]int{}, []int{1, 2}},
		{"provided facts replace", map[int]int{1: 100}, []int{2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := PendingTVBestVersionEpisodes(snapshot, test.override)
			if err != nil || !reflect.DeepEqual(r08EpisodeNumbers(got), test.want) {
				t.Fatalf("pending=%v want=%v err=%v", got, test.want, err)
			}
		})
	}
	facts, err := DescribeTVBestVersion(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	facts.EpisodePriority[1] = 0
	if snapshot.EpisodePriority[1] != 100 {
		t.Fatal("facts mutated source priorities")
	}
}

func TestDescribeTVBestVersionRejectsUnboundedTarget(t *testing.T) {
	t.Parallel()
	if _, err := DescribeTVBestVersion(TVTargetSnapshot{TotalEpisode: maxTVTargetEpisode + 1}); err == nil {
		t.Fatal("accepted unbounded target")
	}
}
