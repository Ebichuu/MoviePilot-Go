package subscription

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type r08TVQualitySnapshot struct {
	TVTargetSnapshot
	Type     string  `json:"type"`
	TMDBID   *int    `json:"tmdbid"`
	DoubanID *string `json:"doubanid"`
}

type r08TVQualityExpected struct {
	Note            []int       `json:"note"`
	EpisodePriority map[int]int `json:"episode_priority"`
	CurrentPriority *int        `json:"current_priority"`
	LackEpisode     int         `json:"lack_episode"`
	Complete        bool        `json:"complete"`
	CoveredEpisodes []int       `json:"covered_episodes"`
}

type r08TVQualityFixture struct {
	Admission []struct {
		Name  string `json:"name"`
		Input struct {
			Subscribe        TVTargetSnapshot `json:"subscribe"`
			Priority         any              `json:"candidate_priority"`
			SelectedEpisodes []any            `json:"selected_episodes"`
			MetaEpisodes     []any            `json:"meta_episodes"`
		} `json:"input"`
		Expected struct {
			Allowed            bool  `json:"allowed"`
			InterestedEpisodes []int `json:"interested_episodes"`
			PreferFullPack     bool  `json:"prefer_full_pack"`
		} `json:"expected"`
	} `json:"admission"`
	Facts []struct {
		Name  string `json:"name"`
		Input struct {
			Subscribe r08TVQualitySnapshot `json:"subscribe"`
			Downloads []struct {
				Priority              any     `json:"priority"`
				SelectedEpisodes      []any   `json:"selected_episodes"`
				MetaEpisodes          []any   `json:"meta_episodes"`
				ConfirmedFullCoverage bool    `json:"confirmed_full_coverage"`
				MediaType             string  `json:"media_type"`
				TMDBID                *int    `json:"tmdbid"`
				DoubanID              *string `json:"doubanid"`
			} `json:"downloads"`
		} `json:"input"`
		Expected struct {
			Writer   r08TVQualityExpected `json:"writer"`
			Progress r08TVQualityExpected `json:"progress"`
		} `json:"expected"`
	} `json:"facts"`
}

func readR08TVQualityFixture(t *testing.T) r08TVQualityFixture {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve TV fixture location")
	}
	payload, err := os.ReadFile(filepath.Join(filepath.Dir(current), "..", "..", "testdata", "contracts", "fixtures", "r08", "tv-quality-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture r08TVQualityFixture
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Admission) == 0 || len(fixture.Facts) == 0 {
		t.Fatal("empty frozen V2 TV quality fixture")
	}
	return fixture
}

func r08TVQualityEpisodes(values []any, season int) []Episode {
	result := make([]Episode, 0, len(values))
	for _, value := range values {
		episode, valid := priorityInteger(value)
		if !valid {
			// Keep an explicit invalid range nonempty at the admission boundary.
			result = append(result, Episode{Season: -1, Episode: -1})
			continue
		}
		result = append(result, Episode{Season: season, Episode: episode})
	}
	return result
}

func TestPrepareTVBestVersionCandidateMatchesFrozenV2(t *testing.T) {
	t.Parallel()
	fixture := readR08TVQualityFixture(t)
	for _, test := range fixture.Admission {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			priority, valid := priorityInteger(test.Input.Priority)
			if !valid {
				t.Skip("领域 API 只接受已解析的整数质量；非法文本由输入适配层拒绝")
			}
			candidate := test.Input.SelectedEpisodes
			if candidate == nil {
				candidate = test.Input.MetaEpisodes
			}
			got, err := PrepareTVBestVersionCandidate(test.Input.Subscribe, priority, r08TVQualityEpisodes(candidate, test.Input.Subscribe.Season))
			if err != nil {
				t.Fatal(err)
			}
			if got.Allowed != test.Expected.Allowed || got.PreferFullPack != test.Expected.PreferFullPack {
				t.Fatalf("decision=%+v, V2=%+v", got, test.Expected)
			}
			if !test.Input.Subscribe.BestVersionFull && !reflect.DeepEqual(r08EpisodeNumbers(got.InterestedEpisodes), test.Expected.InterestedEpisodes) {
				t.Fatalf("interested=%v, V2=%v", got.InterestedEpisodes, test.Expected.InterestedEpisodes)
			}
		})
	}
}

func TestRecordTVDownloadPriorityMatchesFrozenV2(t *testing.T) {
	t.Parallel()
	fixture := readR08TVQualityFixture(t)
	for _, test := range fixture.Facts {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			written := cloneTVTargetSnapshot(test.Input.Subscribe.TVTargetSnapshot)
			covered := make(map[int]struct{})
			allowed := make(map[Episode]struct{})
			for _, target := range tvTargetEpisodes(written) {
				allowed[target] = struct{}{}
			}
			for _, download := range test.Input.Downloads {
				if download.MediaType != test.Input.Subscribe.Type ||
					(test.Input.Subscribe.TMDBID != nil && download.TMDBID != nil && *test.Input.Subscribe.TMDBID != *download.TMDBID) ||
					(test.Input.Subscribe.DoubanID != nil && download.DoubanID != nil && *test.Input.Subscribe.DoubanID != *download.DoubanID) {
					continue
				}
				var priority *int
				// V2's writer accepts int excluding bool; a numeric text is still
				// unknown quality at this boundary, unlike full-mode admission.
				if value, ok := download.Priority.(json.Number); ok {
					if number, err := value.Int64(); err == nil && int64(int(number)) == number {
						priority = intPtr(int(number))
					}
				}
				episodes := download.SelectedEpisodes
				if len(episodes) == 0 {
					episodes = download.MetaEpisodes
				}
				actual := r08TVQualityEpisodes(episodes, written.Season)
				if len(actual) == 0 && download.ConfirmedFullCoverage {
					actual = tvTargetEpisodes(written)
				}
				var err error
				written, err = RecordTVDownloadPriority(written, priority, actual, download.ConfirmedFullCoverage)
				if err != nil {
					t.Fatal(err)
				}
				for _, episode := range actual {
					if _, valid := allowed[episode]; valid {
						covered[episode.Episode] = struct{}{}
					}
				}
			}
			facts, err := DescribeTVBestVersion(written)
			if err != nil {
				t.Fatal(err)
			}
			coveredNumbers := make([]int, 0, len(covered))
			for episode := range covered {
				coveredNumbers = append(coveredNumbers, episode)
			}
			sort.Ints(coveredNumbers)
			want := test.Expected.Writer
			if !reflect.DeepEqual(written.Note, want.Note) || !reflect.DeepEqual(written.EpisodePriority, want.EpisodePriority) ||
				!reflect.DeepEqual(written.CurrentPriority, want.CurrentPriority) || written.LackEpisode != want.LackEpisode ||
				facts.Complete != want.Complete || !reflect.DeepEqual(coveredNumbers, want.CoveredEpisodes) {
				t.Fatalf("writer=%+v facts=%+v covered=%v, V2=%+v", written, facts, coveredNumbers, want)
			}
			if written.BestVersion {
				// The independent progress entry derives quality and any-version
				// lack; ordinary TV still needs its external media-library lefts.
				current := written.CurrentPriority
				if !written.BestVersionFull {
					current = intPtr(facts.CurrentPriority)
				}
				lack := len(tvTargetEpisodes(written)) - len(facts.DownloadedEpisodes)
				if !reflect.DeepEqual(current, test.Expected.Progress.CurrentPriority) || lack != test.Expected.Progress.LackEpisode || facts.Complete != test.Expected.Progress.Complete {
					t.Fatalf("progress current=%v lack=%d complete=%v, V2=%+v", current, lack, facts.Complete, test.Expected.Progress)
				}
			}
		})
	}
}

func TestPrepareTVBestVersionCandidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		input      TVTargetSnapshot
		priority   int
		candidate  []Episode
		allowed    bool
		interested []int
		preferPack bool
	}{
		{"ordinary ignores quality gate", TVTargetSnapshot{Season: 1, TotalEpisode: 2, CurrentPriority: intPtr(100)}, -1, nil, true, []int{}, false},
		{"full nil baseline rejects zero", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, 0, nil, false, []int{}, false},
		{"full nil baseline rejects negative", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, -1, nil, false, []int{}, false},
		{"full zero baseline accepts positive", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, CurrentPriority: intPtr(0)}, 1, nil, true, []int{}, false},
		{"full equal baseline rejects", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, CurrentPriority: intPtr(90)}, 90, nil, false, []int{}, false},
		{"full negative baseline upgrades", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, CurrentPriority: intPtr(-2)}, -1, nil, true, []int{}, false},
		{"full unknown target gate remains separate", TVTargetSnapshot{Season: 1, BestVersion: true, BestVersionFull: true}, 1, nil, true, []int{}, false},
		{"missing per episode accepts negative without preferring pack", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true}, -1, nil, true, []int{1, 2}, false},
		{"missing per episode accepts zero without preferring pack", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true}, 0, nil, true, []int{1, 2}, false},
		{"legacy zero is a known baseline", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, CurrentPriority: intPtr(0)}, 0, nil, false, []int{}, false},
		{"legacy negative baseline upgrades", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, CurrentPriority: intPtr(-2)}, -1, nil, true, []int{1, 2}, true},
		{"some episodes upgrade", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, EpisodePriority: map[int]int{1: 100, 2: 40}}, 90, nil, true, []int{2}, false},
		{"strict upgrade allows priority above completion value", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, EpisodePriority: map[int]int{1: 100, 2: 100}}, 101, nil, true, []int{1, 2}, true},
		{"explicit wrong season cannot improve", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true}, 90, []Episode{{Season: 2, Episode: 1}}, false, []int{}, true},
		{"explicit target scope and deduplication", TVTargetSnapshot{Season: 0, StartEpisode: 2, TotalEpisode: 3, BestVersion: true, EpisodePriority: map[int]int{2: 90}}, 90, []Episode{{Season: 0, Episode: 1}, {Season: 0, Episode: 3}, {Season: 0, Episode: 3}}, true, []int{3}, false},
		{"unknown per episode target rejects", TVTargetSnapshot{Season: 1, BestVersion: true}, 90, nil, false, []int{}, false},
		{"known empty per episode target rejects", TVTargetSnapshot{Season: 1, StartEpisode: 3, TotalEpisode: 2, BestVersion: true}, 90, nil, false, []int{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := PrepareTVBestVersionCandidate(test.input, test.priority, test.candidate)
			if err != nil {
				t.Fatal(err)
			}
			if got.Allowed != test.allowed || got.PreferFullPack != test.preferPack ||
				!reflect.DeepEqual(r08EpisodeNumbers(got.InterestedEpisodes), test.interested) {
				t.Fatalf("decision=%+v, want allowed=%v interested=%v preferPack=%v", got, test.allowed, test.interested, test.preferPack)
			}
		})
	}
}

func TestRecordTVDownloadPriority(t *testing.T) {
	t.Parallel()
	fullTarget := []Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	tests := []struct {
		name           string
		input          TVTargetSnapshot
		priority       *int
		actual         []Episode
		fullCoverage   bool
		wantNote       []int
		wantPriorities map[int]int
		wantCurrent    *int
	}{
		{"ordinary retains per episode quality", TVTargetSnapshot{Season: 1, TotalEpisode: 2, LackEpisode: 2}, intPtr(90), fullTarget[:1], false, []int{1}, map[int]int{1: 90}, nil},
		{"unknown plan quality records note only", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true}, nil, fullTarget[:1], false, []int{1}, map[int]int{}, nil},
		{"zero quality remains an explicit fact", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true}, intPtr(0), fullTarget[:1], false, []int{1}, map[int]int{1: 0}, nil},
		{"negative quality remains an explicit fact", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true}, intPtr(-1), fullTarget[:1], false, []int{1}, map[int]int{1: -1}, nil},
		{"lower quality retains baseline but records coverage", TVTargetSnapshot{Season: 1, TotalEpisode: 2, EpisodePriority: map[int]int{1: 100}}, intPtr(90), fullTarget, false, []int{1, 2}, map[int]int{1: 100, 2: 90}, nil},
		{"negative baseline upgrades", TVTargetSnapshot{Season: 1, TotalEpisode: 2, EpisodePriority: map[int]int{1: -2}}, intPtr(-1), fullTarget[:1], false, []int{1}, map[int]int{1: -1}, nil},
		{"legacy facts materialize without rewriting overall", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, CurrentPriority: intPtr(80)}, intPtr(90), fullTarget[:1], false, []int{1}, map[int]int{1: 90, 2: 80}, intPtr(80)},
		{"nil priority does not upgrade legacy facts", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, CurrentPriority: intPtr(80)}, nil, fullTarget[:1], false, []int{1}, map[int]int{1: 80, 2: 80}, intPtr(80)},
		{"partial full mode records per episode only", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, CurrentPriority: intPtr(70)}, intPtr(100), fullTarget[:1], false, []int{1}, map[int]int{1: 100}, intPtr(70)},
		{"V2 confirmed partial coverage raises overall", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, CurrentPriority: intPtr(70)}, intPtr(100), fullTarget[:1], true, []int{1}, map[int]int{1: 100}, intPtr(100)},
		{"actual complete without confirmation cannot raise overall", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, intPtr(100), fullTarget, false, []int{1, 2}, map[int]int{1: 100, 2: 100}, nil},
		{"confirmed full mode raises overall", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, intPtr(100), fullTarget, true, []int{1, 2}, map[int]int{1: 100, 2: 100}, intPtr(100)},
		{"full mode never decreases overall", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, CurrentPriority: intPtr(100)}, intPtr(90), fullTarget, true, []int{1, 2}, map[int]int{1: 90, 2: 90}, intPtr(100)},
		{"full nil baseline does not take zero", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, intPtr(0), fullTarget, true, []int{1, 2}, map[int]int{1: 0, 2: 0}, nil},
		{"full negative baseline may upgrade", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, CurrentPriority: intPtr(-2)}, intPtr(-1), fullTarget, true, []int{1, 2}, map[int]int{1: -1, 2: -1}, intPtr(-1)},
		{"full missing quality records coverage without guessing", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, nil, fullTarget, true, []int{1, 2}, map[int]int{}, nil},
		{"V2 confirmed empty coverage falls back to target", TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, intPtr(100), nil, true, []int{1, 2}, map[int]int{1: 100, 2: 100}, intPtr(100)},
		{"V2 writer applies numbers to current season range", TVTargetSnapshot{Season: 0, StartEpisode: 2, TotalEpisode: 3}, intPtr(90), []Episode{{Season: 0, Episode: 1}, {Season: 1, Episode: 2}, {Season: 0, Episode: 3}, {Season: 0, Episode: 3}}, false, []int{2, 3}, map[int]int{2: 90, 3: 90}, nil},
		{"note union retains zero and filters negatives", TVTargetSnapshot{Season: 1, TotalEpisode: 2, Note: []int{-1, 0, 2, 2}}, intPtr(90), fullTarget[:1], false, []int{0, 1, 2}, map[int]int{1: 90}, nil},
		{"unknown target cannot produce quality", TVTargetSnapshot{Season: 1, BestVersion: true, BestVersionFull: true}, intPtr(100), fullTarget, true, []int{}, map[int]int{}, nil},
		{"known empty target cannot produce quality", TVTargetSnapshot{Season: 1, StartEpisode: 3, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}, intPtr(100), fullTarget, true, []int{}, map[int]int{}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := RecordTVDownloadPriority(test.input, test.priority, test.actual, test.fullCoverage)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Note, test.wantNote) || !reflect.DeepEqual(got.EpisodePriority, test.wantPriorities) ||
				!reflect.DeepEqual(got.CurrentPriority, test.wantCurrent) || got.LackEpisode != test.input.LackEpisode {
				t.Fatalf("snapshot=%+v, want note=%v priorities=%v current=%v original lack=%d", got, test.wantNote, test.wantPriorities, test.wantCurrent, test.input.LackEpisode)
			}
		})
	}
}

func TestRecordTVDownloadPrioritySeparatesWriterProgressAndCompletion(t *testing.T) {
	t.Parallel()
	input := TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, LackEpisode: 2, CurrentPriority: intPtr(0)}
	actual := []Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	written, err := RecordTVDownloadPriority(input, intPtr(90), actual, false)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := DescribeTVBestVersion(written)
	if err != nil || written.LackEpisode != 2 || *written.CurrentPriority != 0 || facts.CurrentPriority != 90 ||
		len(facts.DownloadedEpisodes) != 2 || len(facts.PendingEpisodes) != 2 || facts.Complete {
		t.Fatalf("writer=%+v facts=%+v err=%v", written, facts, err)
	}
	for _, quality := range []int{100, 101} {
		completed, err := RecordTVDownloadPriority(written, intPtr(quality), actual, false)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := DescribeTVBestVersion(completed)
		if err != nil || facts.Complete != (quality == 100) || facts.CurrentPriority != quality {
			t.Fatalf("priority=%d facts=%+v err=%v", quality, facts, err)
		}
	}
}

func TestRecordTVDownloadPriorityReplayAndInputIsolation(t *testing.T) {
	t.Parallel()
	input := TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true, Note: []int{2}, EpisodePriority: map[int]int{2: 80}, CurrentPriority: intPtr(80)}
	original := cloneTVTargetSnapshot(input)
	actual := []Episode{{Season: 1, Episode: 2}, {Season: 1, Episode: 1}, {Season: 1, Episode: 1}}
	quality := 100
	written, err := RecordTVDownloadPriority(input, &quality, actual, true)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := RecordTVDownloadPriority(written, &quality, actual, true)
	if err != nil || !reflect.DeepEqual(written, replayed) || !reflect.DeepEqual(input, original) {
		t.Fatalf("writer=%+v replay=%+v input=%+v err=%v", written, replayed, input, err)
	}
	quality = 1
	written.Note[0] = 999
	written.EpisodePriority[2] = 1
	*written.CurrentPriority = 1
	if !reflect.DeepEqual(input, original) || *replayed.CurrentPriority != 100 || replayed.Note[0] != 1 || replayed.EpisodePriority[2] != 100 {
		t.Fatal("writer aliased input, priority argument, or replayed snapshot")
	}
}

func TestTVDownloadPriorityFactLimits(t *testing.T) {
	t.Parallel()
	input := TVTargetSnapshot{Season: 1, TotalEpisode: 2, BestVersion: true, BestVersionFull: true}
	actual := make([]Episode, maxTVTargetFacts)
	for index := range actual {
		actual[index] = Episode{Season: 1, Episode: 1}
	}
	written, err := RecordTVDownloadPriority(input, intPtr(100), actual, true)
	if err != nil || written.CurrentPriority == nil || *written.CurrentPriority != 100 || !reflect.DeepEqual(written.Note, []int{1}) {
		t.Fatalf("exact limit=%+v err=%v", written, err)
	}
	if _, err := RecordTVDownloadPriority(input, intPtr(100), append(actual, actual[0]), true); !errors.Is(err, ErrInvalidTVTarget) {
		t.Fatalf("oversized actual error=%v", err)
	}
	if _, err := PrepareTVBestVersionCandidate(input, 100, append(actual, actual[0])); !errors.Is(err, ErrInvalidTVTarget) {
		t.Fatalf("oversized candidate error=%v", err)
	}
	if _, err := RecordTVDownloadPriority(TVTargetSnapshot{TotalEpisode: maxTVTargetEpisode + 1}, nil, nil, false); !errors.Is(err, ErrInvalidTVTarget) {
		t.Fatalf("invalid snapshot error=%v", err)
	}
	for _, field := range []string{"note", "episode_priority"} {
		t.Run(field+" union overflow", func(t *testing.T) {
			t.Parallel()
			large := TVTargetSnapshot{Season: 1, TotalEpisode: 2}
			if field == "note" {
				large.Note = make([]int, maxTVTargetFacts)
				for index := range large.Note {
					large.Note[index] = maxTVTargetEpisode + index + 1
				}
			} else {
				large.EpisodePriority = make(map[int]int, maxTVTargetFacts)
				for index := range maxTVTargetFacts {
					large.EpisodePriority[maxTVTargetEpisode+index+1] = 90
				}
			}
			if _, err := RecordTVDownloadPriority(large, intPtr(100), []Episode{{Season: 1, Episode: 1}}, false); !errors.Is(err, ErrInvalidTVTarget) {
				t.Fatalf("union overflow error=%v", err)
			}
		})
	}
}
