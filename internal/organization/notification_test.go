package organization_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

type goldenScenario struct {
	Name      string                           `json:"name"`
	AppDomain string                           `json:"app_domain"`
	Snapshots []organization.SuccessSnapshot   `json:"snapshots"`
	Expected  []organization.V2OrganizeSuccess `json:"expected"`
}

func TestOrganizeSuccessV2Golden(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("testdata/organize_success_v2.golden.json")
	if err != nil {
		t.Fatalf("读取 golden: %v", err)
	}
	var scenarios []goldenScenario
	if err := json.Unmarshal(body, &scenarios); err != nil {
		t.Fatalf("解析 golden: %v", err)
	}
	if len(scenarios) != 6 {
		t.Fatalf("golden 场景数 = %d, want 6", len(scenarios))
	}
	for _, scenario := range scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()
			got := make([]organization.V2OrganizeSuccess, len(scenario.Snapshots))
			keys := make([]string, len(scenario.Snapshots))
			for index, snapshot := range scenario.Snapshots {
				if snapshot.GroupKey == "" {
					if len(snapshot.Files) == 0 {
						t.Fatalf("golden snapshot %d 缺少文件", index)
					}
					groupKey, buildErr := organization.BuildGroupKey(snapshot.Media, snapshot.Files[0].Season)
					if buildErr != nil {
						t.Fatalf("BuildGroupKey(%d): %v", index, buildErr)
					}
					snapshot.GroupKey = groupKey
				}
				keys[index] = snapshot.GroupKey
				rendered, renderErr := organization.RenderV2OrganizeSuccess(snapshot, scenario.AppDomain)
				if renderErr != nil {
					t.Fatalf("RenderV2OrganizeSuccess(%d): %v", index, renderErr)
				}
				got[index] = rendered
			}
			if !reflect.DeepEqual(got, scenario.Expected) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(scenario.Expected, "", "  ")
				t.Fatalf("渲染结果不匹配\ngot: %s\nwant: %s", gotJSON, wantJSON)
			}
			if scenario.Name == "cross_season" && keys[0] == keys[1] {
				t.Fatal("跨季快照使用了相同 group key")
			}
		})
	}
}

func TestRenderV2OrganizeSuccessFormatsSmallVoteLikeV2(t *testing.T) {
	t.Parallel()
	mediaContext := organization.MediaContext{
		TitleYear:   "低评分示例 (2026)",
		MediaType:   "电影",
		VoteAverage: 0.00001,
	}
	groupKey, err := organization.BuildGroupKey(mediaContext, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := organization.SuccessSnapshot{
		BatchID:    uuid.New(),
		IntentID:   uuid.New(),
		GroupKey:   groupKey,
		Version:    organization.CurrentSuccessVersion,
		NeedNotify: true,
		Media:      mediaContext,
		Files:      []organization.FileFact{{Ordinal: 0, Role: "video"}},
	}

	got, err := organization.RenderV2OrganizeSuccess(snapshot, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Context.VoteAverage != "1e-05" ||
		got.Text != "评分：1e-05，类型：电影，共1个文件，大小：0.0B" {
		t.Fatalf("小数评分渲染 = %#v", got)
	}
}

func TestNormalizeSuccessInput(t *testing.T) {
	t.Parallel()
	intentID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	tests := []struct {
		name    string
		input   organization.SuccessInput
		wantErr error
	}{
		{
			name:  "排序去重且不修改调用方",
			input: organization.SuccessInput{IntentID: intentID, Version: 1, Username: " alice ", Episodes: []organization.Episode{{Season: 2, Episode: 3}, {Season: 1, Episode: 2}, {Season: 1, Episode: 2}}},
		},
		{name: "legacy 版本零", input: organization.SuccessInput{IntentID: intentID, Legacy: true}},
		{name: "新输入错误版本", input: organization.SuccessInput{IntentID: intentID}, wantErr: organization.ErrInvalidSuccessInput},
		{name: "来源与 ID 必须成对", input: organization.SuccessInput{IntentID: intentID, Version: 1, Media: organization.MediaContext{Source: "themoviedb"}}, wantErr: organization.ErrInvalidSuccessInput},
		{name: "拒绝 NUL", input: organization.SuccessInput{IntentID: intentID, Version: 1, Username: "a\x00b"}, wantErr: organization.ErrInvalidSuccessInput},
		{name: "拒绝非法评分", input: organization.SuccessInput{IntentID: intentID, Version: 1, Media: organization.MediaContext{VoteAverage: math.NaN()}}, wantErr: organization.ErrInvalidSuccessInput},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := tt.input.Clone()
			got, err := organization.NormalizeSuccessInput(tt.input)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NormalizeSuccessInput() error = %v, want %v", err, tt.wantErr)
			}
			if tt.name != "拒绝非法评分" && !reflect.DeepEqual(tt.input, before) {
				t.Fatalf("NormalizeSuccessInput 修改调用方: got %#v, want %#v", tt.input, before)
			}
			if tt.wantErr != nil {
				return
			}
			if !got.Valid() {
				t.Fatalf("规范化结果无效: %#v", got)
			}
			if tt.name == "排序去重且不修改调用方" {
				want := []organization.Episode{{Season: 1, Episode: 2}, {Season: 2, Episode: 3}}
				if got.Username != "alice" || !reflect.DeepEqual(got.Episodes, want) {
					t.Fatalf("规范化结果 = %#v", got)
				}
			}
		})
	}
}

func TestNormalizeSuccessInputSchemaBoundaries(t *testing.T) {
	t.Parallel()
	intentID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	tests := []struct {
		name    string
		input   organization.SuccessInput
		wantErr bool
	}{
		{name: "所有文本恰好达到 schema 上限", input: organization.SuccessInput{
			IntentID: intentID, Version: 1, Username: strings.Repeat("u", 255),
			Media: organization.MediaContext{
				Source: strings.Repeat("s", 32), MediaID: strings.Repeat("i", 128),
				TitleYear: strings.Repeat("t", 512), MediaType: strings.Repeat("m", 32), Category: strings.Repeat("c", 128),
			},
		}},
		{name: "source 超限", input: organization.SuccessInput{IntentID: intentID, Version: 1, Media: organization.MediaContext{Source: strings.Repeat("s", 33), MediaID: "1"}}, wantErr: true},
		{name: "media id 超限", input: organization.SuccessInput{IntentID: intentID, Version: 1, Media: organization.MediaContext{Source: "tmdb", MediaID: strings.Repeat("i", 129)}}, wantErr: true},
		{name: "title 超限", input: organization.SuccessInput{IntentID: intentID, Version: 1, Media: organization.MediaContext{TitleYear: strings.Repeat("t", 513)}}, wantErr: true},
		{name: "type 超限", input: organization.SuccessInput{IntentID: intentID, Version: 1, Media: organization.MediaContext{MediaType: strings.Repeat("m", 33)}}, wantErr: true},
		{name: "category 超限", input: organization.SuccessInput{IntentID: intentID, Version: 1, Media: organization.MediaContext{Category: strings.Repeat("c", 129)}}, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := organization.NormalizeSuccessInput(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeSuccessInput() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeSuccessInputEpisodeCountBoundary(t *testing.T) {
	t.Parallel()
	episodes := make([]organization.Episode, 10000)
	for index := range episodes {
		episodes[index] = organization.Episode{Season: 1, Episode: index + 1}
	}
	input := organization.SuccessInput{IntentID: uuid.New(), Version: 1, Episodes: episodes}
	if _, err := organization.NormalizeSuccessInput(input); err != nil {
		t.Fatalf("10000 episodes: %v", err)
	}
	input.Episodes = append(input.Episodes, organization.Episode{Season: 1, Episode: 10001})
	if _, err := organization.NormalizeSuccessInput(input); !errors.Is(err, organization.ErrInvalidSuccessInput) {
		t.Fatalf("10001 episodes error = %v", err)
	}
}

func TestNormalizeSuccessSnapshotAndClone(t *testing.T) {
	t.Parallel()
	media := organization.MediaContext{Source: "themoviedb", MediaID: "7", TitleYear: "剧 (2026)", MediaType: "电视剧"}
	key, err := organization.BuildGroupKey(media, 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := organization.SuccessSnapshot{
		BatchID: uuid.MustParse("22222222-2222-4222-8222-222222222222"), IntentID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		GroupKey: key, Version: 1, NeedNotify: true, Media: media,
		Files: []organization.FileFact{{Ordinal: 1, Role: " media ", SizeBytes: 2, Season: 1, Episodes: []int{3, 2, 2}}, {Ordinal: 0, Role: "media", SizeBytes: 1, Season: 1, Episodes: []int{1}}},
	}
	before := snapshot.Clone()
	normalized, err := organization.NormalizeSuccessSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot, before) {
		t.Fatal("NormalizeSuccessSnapshot 修改了调用方")
	}
	if normalized.Files[0].Ordinal != 0 || !reflect.DeepEqual(normalized.Files[1].Episodes, []int{2, 3}) {
		t.Fatalf("文件规范化结果 = %#v", normalized.Files)
	}
	cloned := normalized.Clone()
	cloned.Files[0].Episodes = append(cloned.Files[0].Episodes, 99)
	if reflect.DeepEqual(cloned.Files, normalized.Files) {
		t.Fatal("SuccessSnapshot.Clone 共享了文件 episode 切片")
	}
}

func TestSuccessInputClonePreservesEmptyEpisodeArray(t *testing.T) {
	t.Parallel()
	input := organization.SuccessInput{Episodes: make([]organization.Episode, 0)}
	cloned := input.Clone()
	if cloned.Episodes == nil || len(cloned.Episodes) != 0 {
		t.Fatalf("Clone().Episodes = %#v, want non-nil empty slice", cloned.Episodes)
	}
}

func TestNormalizeSuccessSnapshotRejectsInvalidFacts(t *testing.T) {
	t.Parallel()
	media := organization.MediaContext{TitleYear: "示例", MediaType: "电影"}
	key0, _ := organization.BuildGroupKey(media, 0)
	key1, _ := organization.BuildGroupKey(media, 1)
	base := organization.SuccessSnapshot{
		BatchID: uuid.New(), IntentID: uuid.New(), GroupKey: key0, Version: 1,
		Files: []organization.FileFact{{Ordinal: 0, Role: "media", Season: 0}}, Media: media,
	}
	tests := []struct {
		name   string
		mutate func(*organization.SuccessSnapshot)
	}{
		{name: "重复 ordinal", mutate: func(s *organization.SuccessSnapshot) { s.Files = append(s.Files, s.Files[0].Clone()) }},
		{name: "跨季", mutate: func(s *organization.SuccessSnapshot) {
			s.Files = append(s.Files, organization.FileFact{Ordinal: 1, Role: "media", Season: 1})
			s.GroupKey = key1
		}},
		{name: "group key 不匹配", mutate: func(s *organization.SuccessSnapshot) { s.GroupKey = key1 }},
		{name: "大小溢出", mutate: func(s *organization.SuccessSnapshot) {
			s.Files = append(s.Files, organization.FileFact{Ordinal: 1, Role: "media", SizeBytes: 1, Season: 0})
			s.Files[0].SizeBytes = math.MaxInt64
		}},
		{name: "缺少文件", mutate: func(s *organization.SuccessSnapshot) { s.Files = nil }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot := base.Clone()
			tt.mutate(&snapshot)
			if _, err := organization.NormalizeSuccessSnapshot(snapshot); !errors.Is(err, organization.ErrInvalidSuccessSnapshot) {
				t.Fatalf("NormalizeSuccessSnapshot() error = %v", err)
			}
		})
	}
}

func TestNormalizeSuccessSnapshotFileCountBoundary(t *testing.T) {
	t.Parallel()
	media := organization.MediaContext{TitleYear: "示例", MediaType: "电影"}
	key, err := organization.BuildGroupKey(media, 0)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]organization.FileFact, 10000)
	for index := range files {
		files[index] = organization.FileFact{Ordinal: index, Role: "media"}
	}
	snapshot := organization.SuccessSnapshot{BatchID: uuid.New(), IntentID: uuid.New(), GroupKey: key, Version: 1, Media: media, Files: files}
	if _, err := organization.NormalizeSuccessSnapshot(snapshot); err != nil {
		t.Fatalf("10000 files: %v", err)
	}
	snapshot.Files = append(snapshot.Files, organization.FileFact{Ordinal: 10000, Role: "media"})
	if _, err := organization.NormalizeSuccessSnapshot(snapshot); !errors.Is(err, organization.ErrInvalidSuccessSnapshot) {
		t.Fatalf("10001 files error = %v", err)
	}
}

func TestSuccessSnapshotEligible(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                           string
		needNotify, manual, background bool
		want                           bool
	}{
		{name: "目录关闭", want: false},
		{name: "普通任务", needNotify: true, want: true},
		{name: "实时手动", needNotify: true, manual: true, want: false},
		{name: "后台手动", needNotify: true, manual: true, background: true, want: true},
		{name: "后台自动", needNotify: true, background: true, want: true},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := (organization.SuccessSnapshot{NeedNotify: tt.needNotify, Manual: tt.manual, Background: tt.background}).Eligible()
			if got != tt.want {
				t.Fatalf("Eligible() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestFormatV2FileSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		size int64
		want string
	}{
		{name: "zero", size: 0, want: "0.0B"}, {name: "one", size: 1, want: "1.0B"},
		{name: "byte boundary", size: 1023, want: "1023.0B"}, {name: "one kib", size: 1024, want: "1.0K"},
		{name: "kib upper oddity", size: 1048575, want: "1024.0K"}, {name: "one mib", size: 1048576, want: "1.0M"},
		{name: "ten gib", size: 10 << 30, want: "10.0G"}, {name: "negative", size: -1, want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := organization.FormatV2FileSize(tt.size); got != tt.want {
				t.Fatalf("FormatV2FileSize(%d) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

func TestFormatV2Episodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []int
		want string
	}{
		{name: "empty", want: ""}, {name: "single", in: []int{1}, want: "E01"},
		{name: "gapped and sorted", in: []int{5, 2, 1, 3}, want: "E01-E03、E05"},
		{name: "season with episodes", in: []int{3, 1, 2}, want: "E01-E03"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := append([]int(nil), tt.in...)
			if got := organization.FormatV2Episodes(tt.in); got != tt.want {
				t.Fatalf("FormatV2Episodes() = %q, want %q", got, tt.want)
			}
			if !reflect.DeepEqual(tt.in, before) {
				t.Fatal("FormatV2Episodes 修改了调用方")
			}
		})
	}
	if got := organization.FormatV2SeasonEpisodes(2, nil); got != "S02" {
		t.Fatalf("FormatV2SeasonEpisodes() = %q", got)
	}
}

func TestV2MessageImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		media organization.MediaContext
		want  string
	}{
		{name: "backdrop first", media: organization.MediaContext{BackdropURL: "https://img/original/a-original.jpg", PosterURL: "https://img/original/poster.jpg"}, want: "https://img/w500/a-w500.jpg"},
		{name: "poster fallback", media: organization.MediaContext{PosterURL: "https://img/original/poster.jpg"}, want: "https://img/w500/poster.jpg"},
		{name: "empty", media: organization.MediaContext{}, want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := organization.V2MessageImage(tt.media); got != tt.want {
				t.Fatalf("V2MessageImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestV2HistoryLink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		domain string
		want   *string
	}{
		{name: "empty"},
		{name: "host only", domain: "mp.example.com", want: stringPointer("http://mp.example.com/#/history")},
		{name: "https host without trailing slash", domain: "https://mp.example.com", want: stringPointer("https://mp.example.com/#/history")},
		{name: "https host with trailing slash", domain: "https://mp.example.com/", want: stringPointer("https://mp.example.com/#/history")},
		{name: "subpath", domain: "https://mp.example.com/moviepilot", want: stringPointer("https://mp.example.com/moviepilot/#/history")},
		{name: "subpath with trailing slash", domain: "https://mp.example.com/moviepilot/", want: stringPointer("https://mp.example.com/moviepilot/#/history")},
		{name: "query preserved", domain: "https://mp.example.com/base?x=1", want: stringPointer("https://mp.example.com/base?x=1/#/history")},
		{name: "existing fragment replaced", domain: "https://mp.example.com/base#old", want: stringPointer("https://mp.example.com/base#/history")},
		{name: "query preserved and fragment replaced", domain: "https://mp.example.com/base?x=1#old", want: stringPointer("https://mp.example.com/base?x=1#/history")},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := organization.V2HistoryLink(tt.domain); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("V2HistoryLink(%q) = %#v, want %#v", tt.domain, got, tt.want)
			}
		})
	}
}

func TestBuildGroupKey(t *testing.T) {
	t.Parallel()
	byID, err := organization.BuildGroupKey(organization.MediaContext{Source: "TheMovieDB", MediaID: "7", TitleYear: "旧标题"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	renamed, _ := organization.BuildGroupKey(organization.MediaContext{Source: "themoviedb", MediaID: "7", TitleYear: "新标题"}, 1)
	nextSeason, _ := organization.BuildGroupKey(organization.MediaContext{Source: "themoviedb", MediaID: "7"}, 2)
	byTitle, _ := organization.BuildGroupKey(organization.MediaContext{TitleYear: "旧标题"}, 1)
	if byID != renamed || byID == nextSeason || byID == byTitle || !strings.HasPrefix(byID, "sha256:") {
		t.Fatalf("group keys: id=%q renamed=%q season=%q title=%q", byID, renamed, nextSeason, byTitle)
	}
}

func TestV2OrganizeSuccessCloneAndContextValues(t *testing.T) {
	t.Parallel()
	original := organization.V2OrganizeSuccess{Link: stringPointer("https://example/#/history"), Context: organization.V2OrganizeSuccessContext{TitleYear: "示例"}}
	cloned := original.Clone()
	*cloned.Link = "changed"
	values := cloned.Context.Values()
	values["title_year"] = "changed"
	if *original.Link != "https://example/#/history" || original.Context.TitleYear != "示例" {
		t.Fatal("Clone 或 Values 共享了可变状态")
	}
}

func FuzzFormatV2Episodes(f *testing.F) {
	f.Add([]byte{1, 2, 3, 5})
	f.Add([]byte{})
	f.Add([]byte{255, 0, 1})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 128 {
			raw = raw[:128]
		}
		episodes := make([]int, len(raw))
		for index, value := range raw {
			episodes[index] = int(value) + 1
		}
		before := append([]int{}, episodes...)
		got := organization.FormatV2Episodes(episodes)
		if len(got) > len(episodes)*14 {
			t.Fatalf("输出无界: input=%d output=%d", len(episodes), len(got))
		}
		if !reflect.DeepEqual(episodes, before) {
			t.Fatal("FormatV2Episodes 修改了输入")
		}
	})
}

func FuzzV2HistoryLink(f *testing.F) {
	f.Add("")
	f.Add("https://mp.example.com/base")
	f.Add("mp.example.com")
	f.Fuzz(func(t *testing.T, domain string) {
		link := organization.V2HistoryLink(domain)
		if link != nil && len(*link) > 4096 {
			t.Fatalf("link 输出超过上限: %d", len(*link))
		}
	})
}

func stringPointer(value string) *string { return &value }
