package organization_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

func TestProjectTerminalResults(t *testing.T) {
	t.Parallel()
	batchID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	intentID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	seasonOne := terminalSnapshot(t, batchID, intentID, 1, []organization.FileFact{
		{Ordinal: 0, Role: "video", SizeBytes: 10, Season: 1, Episodes: []int{1}},
		{Ordinal: 1, Role: "subtitle", SizeBytes: 2, Season: 1, Episodes: []int{1}},
	})
	seasonTwo := terminalSnapshot(t, batchID, intentID, 2, []organization.FileFact{
		{Ordinal: 2, Role: "video", SizeBytes: 20, Season: 2, Episodes: []int{2}},
		{Ordinal: 3, Role: "subtitle", SizeBytes: 3, Season: 2, Episodes: []int{2}},
	})

	tests := []struct {
		name             string
		snapshots        []organization.SuccessSnapshot
		outcomes         []organization.FileOutcome
		wantGroupFiles   [][]int
		wantGroupErrors  []string
		wantTerminal     []int
		wantFailureOrder []int
	}{
		{
			name:      "最后失败保留组错误",
			snapshots: []organization.SuccessSnapshot{seasonOne},
			outcomes: []organization.FileOutcome{
				{Ordinal: 0, State: organization.FileOutcomeCompleted},
				{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "subtitle failed"},
			},
			wantGroupFiles: [][]int{{0}}, wantGroupErrors: []string{"subtitle failed"}, wantTerminal: []int{1},
			wantFailureOrder: []int{1},
		},
		{
			name:      "最后成功清空组错误",
			snapshots: []organization.SuccessSnapshot{seasonOne},
			outcomes: []organization.FileOutcome{
				{Ordinal: 0, State: organization.FileOutcomeTerminalFailed, Error: "video failed"},
				{Ordinal: 1, State: organization.FileOutcomeCompleted},
			},
			wantGroupFiles: [][]int{{1}}, wantGroupErrors: []string{""}, wantTerminal: []int{1},
			wantFailureOrder: []int{0},
		},
		{
			name:      "全失败只返回失败清单",
			snapshots: []organization.SuccessSnapshot{seasonOne, seasonTwo},
			outcomes: []organization.FileOutcome{
				{Ordinal: 3, State: organization.FileOutcomeTerminalFailed, Error: "failure 3"},
				{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "failure 1"},
				{Ordinal: 2, State: organization.FileOutcomeTerminalFailed, Error: "failure 2"},
				{Ordinal: 0, State: organization.FileOutcomeTerminalFailed, Error: "failure 0"},
			},
			wantFailureOrder: []int{0, 1, 2, 3},
		},
		{
			name:      "跨季按输入顺序独立投影",
			snapshots: []organization.SuccessSnapshot{seasonTwo, seasonOne},
			outcomes: []organization.FileOutcome{
				{Ordinal: 1, State: organization.FileOutcomeCompleted},
				{Ordinal: 3, State: organization.FileOutcomeCompleted},
				{Ordinal: 0, State: organization.FileOutcomeTerminalFailed, Error: "season 1 failed"},
				{Ordinal: 2, State: organization.FileOutcomeTerminalFailed, Error: "season 2 failed"},
			},
			wantGroupFiles: [][]int{{3}, {1}}, wantGroupErrors: []string{"", ""}, wantTerminal: []int{3, 1},
			wantFailureOrder: []int{0, 2},
		},
		{
			name: "非连续 ordinal 仍按精确集合覆盖",
			snapshots: []organization.SuccessSnapshot{terminalSnapshot(t, batchID, intentID, 3, []organization.FileFact{
				{Ordinal: 4, Role: "video", Season: 3},
				{Ordinal: 9, Role: "subtitle", Season: 3},
			})},
			outcomes: []organization.FileOutcome{
				{Ordinal: 9, State: organization.FileOutcomeCompleted},
				{Ordinal: 4, State: organization.FileOutcomeCompleted},
			},
			wantGroupFiles: [][]int{{4, 9}}, wantGroupErrors: []string{""}, wantTerminal: []int{9},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			groups, failures, err := organization.ProjectTerminalResults(tt.snapshots, tt.outcomes)
			if err != nil {
				t.Fatal(err)
			}
			if got := resultOrdinals(groups); !equalOrdinalGroups(got, tt.wantGroupFiles) {
				t.Fatalf("group files = %v, want %v", got, tt.wantGroupFiles)
			}
			gotErrors := make([]string, len(groups))
			for index := range groups {
				gotErrors[index] = groups[index].ErrMsg
			}
			if !slices.Equal(gotErrors, tt.wantGroupErrors) {
				t.Fatalf("group errors = %v, want %v", gotErrors, tt.wantGroupErrors)
			}
			gotTerminal := make([]int, len(groups))
			for index := range groups {
				gotTerminal[index] = groups[index].TerminalOrdinal
			}
			if !slices.Equal(gotTerminal, tt.wantTerminal) {
				t.Fatalf("terminal ordinals = %v, want %v", gotTerminal, tt.wantTerminal)
			}
			gotFailures := make([]int, len(failures))
			for index := range failures {
				gotFailures[index] = failures[index].File.Ordinal
				if failures[index].Snapshot.Media != terminalMedia() || failures[index].Error == "" {
					t.Fatalf("failure context = %#v", failures[index])
				}
			}
			if !slices.Equal(gotFailures, tt.wantFailureOrder) {
				t.Fatalf("failure order = %v, want %v", gotFailures, tt.wantFailureOrder)
			}
		})
	}
}

func TestProjectTerminalResultsRejectsInvalidCollections(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	intentID := uuid.New()
	base := terminalSnapshot(t, batchID, intentID, 1, []organization.FileFact{
		{Ordinal: 0, Role: "video", Season: 1},
		{Ordinal: 1, Role: "subtitle", Season: 1},
	})
	validOutcomes := []organization.FileOutcome{
		{Ordinal: 0, State: organization.FileOutcomeCompleted},
		{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "failed"},
	}

	tests := []struct {
		name      string
		snapshots func() []organization.SuccessSnapshot
		outcomes  func() []organization.FileOutcome
	}{
		{name: "缺少快照", snapshots: func() []organization.SuccessSnapshot { return nil }},
		{name: "缺少 outcome", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }},
		{name: "非法快照", snapshots: func() []organization.SuccessSnapshot {
			item := base.Clone()
			item.GroupKey = "invalid"
			return []organization.SuccessSnapshot{item}
		}, outcomes: func() []organization.FileOutcome { return append([]organization.FileOutcome(nil), validOutcomes...) }},
		{name: "重复快照组", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base, base} }, outcomes: func() []organization.FileOutcome { return append([]organization.FileOutcome(nil), validOutcomes...) }},
		{name: "跨快照重复 ordinal", snapshots: func() []organization.SuccessSnapshot {
			other := terminalSnapshot(t, batchID, intentID, 2, []organization.FileFact{{Ordinal: 1, Role: "video", Season: 2}})
			return []organization.SuccessSnapshot{base, other}
		}, outcomes: func() []organization.FileOutcome { return append([]organization.FileOutcome(nil), validOutcomes...) }},
		{name: "批次不一致", snapshots: func() []organization.SuccessSnapshot {
			other := terminalSnapshot(t, uuid.New(), intentID, 2, []organization.FileFact{{Ordinal: 2, Role: "video", Season: 2}})
			return []organization.SuccessSnapshot{base, other}
		}, outcomes: func() []organization.FileOutcome {
			return append(append([]organization.FileOutcome(nil), validOutcomes...), organization.FileOutcome{Ordinal: 2, State: organization.FileOutcomeCompleted})
		}},
		{name: "缺少一个 ordinal", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome { return validOutcomes[:1] }},
		{name: "重复 outcome ordinal", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{validOutcomes[0], validOutcomes[0]}
		}},
		{name: "未知 outcome ordinal", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{validOutcomes[0], {Ordinal: 9, State: organization.FileOutcomeCompleted}}
		}},
		{name: "负数 ordinal", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{{Ordinal: -1, State: organization.FileOutcomeCompleted}, validOutcomes[1]}
		}},
		{name: "非终态", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{{Ordinal: 0, State: "organizing"}, validOutcomes[1]}
		}},
		{name: "成功携带错误", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{{Ordinal: 0, State: organization.FileOutcomeCompleted, Error: "unexpected"}, validOutcomes[1]}
		}},
		{name: "失败缺少错误", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{validOutcomes[0], {Ordinal: 1, State: organization.FileOutcomeTerminalFailed}}
		}},
		{name: "失败错误有首尾空白", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{validOutcomes[0], {Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: " failed "}}
		}},
		{name: "失败错误含控制字符", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{validOutcomes[0], {Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "failed\nsecret"}}
		}},
		{name: "失败错误超过上限", snapshots: func() []organization.SuccessSnapshot { return []organization.SuccessSnapshot{base} }, outcomes: func() []organization.FileOutcome {
			return []organization.FileOutcome{validOutcomes[0], {Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: strings.Repeat("x", 2049)}}
		}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var outcomes []organization.FileOutcome
			if tt.outcomes != nil {
				outcomes = tt.outcomes()
			}
			if _, _, err := organization.ProjectTerminalResults(tt.snapshots(), outcomes); !errors.Is(err, organization.ErrInvalidTerminalResults) {
				t.Fatalf("ProjectTerminalResults() error = %v, want ErrInvalidTerminalResults", err)
			}
		})
	}
}

func TestProjectTerminalResultsCollectionBoundary(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	intentID := uuid.New()
	files := make([]organization.FileFact, 10000)
	outcomes := make([]organization.FileOutcome, 10000)
	for ordinal := range files {
		files[ordinal] = organization.FileFact{Ordinal: ordinal, Role: "video", Season: 1}
		outcomes[ordinal] = organization.FileOutcome{Ordinal: ordinal, State: organization.FileOutcomeCompleted}
	}
	snapshot := terminalSnapshot(t, batchID, intentID, 1, files)
	groups, failures, err := organization.ProjectTerminalResults([]organization.SuccessSnapshot{snapshot}, outcomes)
	if err != nil || len(groups) != 1 || len(groups[0].Snapshot.Files) != 10000 || len(failures) != 0 {
		t.Fatalf("10000 file projection = groups %d failures %d error %v", len(groups), len(failures), err)
	}

	first := terminalSnapshot(t, batchID, intentID, 1, files[:5001])
	secondFiles := make([]organization.FileFact, 5000)
	for index := range secondFiles {
		secondFiles[index] = organization.FileFact{Ordinal: index + 5001, Role: "video", Season: 2}
	}
	second := terminalSnapshot(t, batchID, intentID, 2, secondFiles)
	tooManyOutcomes := append(append([]organization.FileOutcome(nil), outcomes...), organization.FileOutcome{Ordinal: 10000, State: organization.FileOutcomeCompleted})
	if _, _, err := organization.ProjectTerminalResults([]organization.SuccessSnapshot{first, second}, tooManyOutcomes); !errors.Is(err, organization.ErrInvalidTerminalResults) {
		t.Fatalf("10001 file projection error = %v", err)
	}
}

func TestProjectTerminalResultsErrorBoundary(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	intentID := uuid.New()
	snapshot := terminalSnapshot(t, batchID, intentID, 1, []organization.FileFact{{
		Ordinal: 0, Role: "video", Season: 1,
	}})
	maximum := strings.Repeat("错", 682) + "xx"
	if len(maximum) != 2048 {
		t.Fatalf("test error size = %d", len(maximum))
	}
	groups, failures, err := organization.ProjectTerminalResults(
		[]organization.SuccessSnapshot{snapshot},
		[]organization.FileOutcome{{
			Ordinal: 0, State: organization.FileOutcomeTerminalFailed, Error: maximum,
		}},
	)
	if err != nil || len(groups) != 0 || len(failures) != 1 || failures[0].Error != maximum {
		t.Fatalf("2048-byte error projection = groups %d failures %d error %v", len(groups), len(failures), err)
	}

	invalidUTF8 := string([]byte{0xff})
	if _, _, err := organization.ProjectTerminalResults(
		[]organization.SuccessSnapshot{snapshot},
		[]organization.FileOutcome{{
			Ordinal: 0, State: organization.FileOutcomeTerminalFailed, Error: invalidUTF8,
		}},
	); !errors.Is(err, organization.ErrInvalidTerminalResults) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestProjectTerminalResultsDoesNotMutateOrShareInputs(t *testing.T) {
	t.Parallel()
	batchID := uuid.New()
	intentID := uuid.New()
	snapshot := terminalSnapshot(t, batchID, intentID, 1, []organization.FileFact{
		{Ordinal: 0, Role: "video", Season: 1, Episodes: []int{1}},
		{Ordinal: 1, Role: "subtitle", Season: 1, Episodes: []int{2}},
	})
	snapshots := []organization.SuccessSnapshot{snapshot}
	outcomes := []organization.FileOutcome{
		{Ordinal: 0, State: organization.FileOutcomeCompleted},
		{Ordinal: 1, State: organization.FileOutcomeTerminalFailed, Error: "failed"},
	}
	before := []organization.SuccessSnapshot{snapshot.Clone()}
	groups, failures, err := organization.ProjectTerminalResults(snapshots, outcomes)
	if err != nil {
		t.Fatal(err)
	}
	groups[0].Snapshot.Files[0].Episodes[0] = 99
	failures[0].File.Episodes[0] = 98
	failures[0].Snapshot.Media.TitleYear = "changed"
	if !reflect.DeepEqual(snapshots, before) {
		t.Fatalf("input snapshots mutated: got %#v want %#v", snapshots, before)
	}
	if snapshots[0].Files[0].Episodes[0] != 1 || snapshots[0].Files[1].Episodes[0] != 2 || snapshots[0].Media.TitleYear != terminalMedia().TitleYear {
		t.Fatal("result shares mutable input state")
	}

	groupClone := groups[0].Clone()
	groupClone.Snapshot.Files[0].Episodes[0] = 42
	failureClone := failures[0].Clone()
	failureClone.File.Episodes[0] = 43
	if groups[0].Snapshot.Files[0].Episodes[0] == 42 || failures[0].File.Episodes[0] == 43 {
		t.Fatal("result Clone shares mutable slices")
	}
}

func resultOrdinals(groups []organization.ResultSnapshot) [][]int {
	result := make([][]int, len(groups))
	for groupIndex, group := range groups {
		result[groupIndex] = make([]int, len(group.Snapshot.Files))
		for fileIndex, file := range group.Snapshot.Files {
			result[groupIndex][fileIndex] = file.Ordinal
		}
	}
	return result
}

func equalOrdinalGroups(left, right [][]int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !slices.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func terminalSnapshot(
	t *testing.T,
	batchID uuid.UUID,
	intentID uuid.UUID,
	season int,
	files []organization.FileFact,
) organization.SuccessSnapshot {
	t.Helper()
	media := terminalMedia()
	groupKey, err := organization.BuildGroupKey(media, season)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := organization.NormalizeSuccessSnapshot(organization.SuccessSnapshot{
		BatchID: batchID, IntentID: intentID, GroupKey: groupKey,
		Version: organization.CurrentSuccessVersion, Username: "alice",
		NeedNotify: true, Background: true, Media: media, Files: files,
	})
	if err != nil {
		t.Fatalf("NormalizeSuccessSnapshot(): %v", err)
	}
	return snapshot
}

func terminalMedia() organization.MediaContext {
	return organization.MediaContext{
		Source: "themoviedb", MediaID: "7", TitleYear: "Example (2026)",
		MediaType: "电视剧", Category: "Drama", VoteAverage: 8.5,
		PosterURL:   "https://image.example/poster.jpg",
		BackdropURL: "https://image.example/backdrop.jpg",
	}
}
