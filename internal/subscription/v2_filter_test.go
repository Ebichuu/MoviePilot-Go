package subscription

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

func TestFilterV2CandidatesUsesPersistedGroupOrderAndLastPriority(t *testing.T) {
	candidates := []Candidate{
		v2FilterCandidate("4k", "Example 2160p HDR", ""),
		v2FilterCandidate("1080p", "Example 1080p", ""),
	}
	groups := []V2FilterRuleGroup{
		{Name: "reverse", RuleString: "1080P > 4K"},
		{Name: "preferred", RuleString: "4K > 1080P"},
	}

	got, err := FilterV2Candidates(
		candidates,
		[]string{"preferred", "reverse"},
		groups,
		nil,
		V2FilterMedia{MediaType: string(media.TypeMovie)},
		time.Now(),
	)
	if err != nil {
		t.Fatalf("FilterV2Candidates() error = %v", err)
	}
	if keys, priorities := v2FilteredKeysAndPriorities(got); !reflect.DeepEqual(keys, []string{"4k", "1080p"}) ||
		!reflect.DeepEqual(priorities, []int{100, 99}) {
		t.Fatalf("result = (%v, %v), want ([4k 1080p], [100 99])", keys, priorities)
	}
	if candidates[0].Resource.PriOrder != 0 || candidates[1].Resource.PriOrder != 0 {
		t.Fatalf("FilterV2Candidates() mutated input priorities: %#v", candidates)
	}
}

func TestFilterV2CandidatesMatchesV2GroupApplicability(t *testing.T) {
	candidate := v2FilterCandidate("candidate", "Example 2160p", "")
	tests := []struct {
		name      string
		group     V2FilterRuleGroup
		facts     V2FilterMedia
		wantOrder int
	}{
		{
			name:      "empty media type applies even with category",
			group:     V2FilterRuleGroup{Name: "g", RuleString: "4K", Category: "other"},
			facts:     V2FilterMedia{MediaType: string(media.TypeMovie), Category: "movie"},
			wantOrder: 100,
		},
		{
			name:      "category match ignores media type mismatch",
			group:     V2FilterRuleGroup{Name: "g", RuleString: "4K", MediaType: string(media.TypeMovie), Category: "anime"},
			facts:     V2FilterMedia{MediaType: string(media.TypeTV), Category: "anime"},
			wantOrder: 100,
		},
		{
			name:  "typed group without category does not match a different nonempty category",
			group: V2FilterRuleGroup{Name: "g", RuleString: "4K", MediaType: string(media.TypeMovie)},
			facts: V2FilterMedia{MediaType: string(media.TypeTV), Category: "tv"},
		},
		{
			name:      "empty categories retain V2 fallback comparison",
			group:     V2FilterRuleGroup{Name: "g", RuleString: "4K", MediaType: string(media.TypeMovie)},
			facts:     V2FilterMedia{MediaType: string(media.TypeTV)},
			wantOrder: 100,
		},
		{
			name:  "category mismatch is not applicable",
			group: V2FilterRuleGroup{Name: "g", RuleString: "4K", MediaType: string(media.TypeTV), Category: "anime"},
			facts: V2FilterMedia{MediaType: string(media.TypeTV), Category: "documentary"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate.Resource.PriOrder = 7
			got, err := FilterV2Candidates(
				[]Candidate{candidate}, []string{"g"}, []V2FilterRuleGroup{tt.group}, nil, tt.facts, time.Now(),
			)
			if err != nil {
				t.Fatalf("FilterV2Candidates() error = %v", err)
			}
			want := tt.wantOrder
			if want == 0 {
				want = 7
			}
			if len(got) != 1 || got[0].Resource.PriOrder != want {
				t.Fatalf("result = %#v, want one candidate with priority %d", got, want)
			}
		})
	}
}

func TestFilterV2CandidatesKeepsCandidatesWhenNoApplicableRuleString(t *testing.T) {
	candidate := v2FilterCandidate("candidate", "Example 2160p", "")
	candidate.Resource.PriOrder = 42
	tests := []struct {
		name      string
		requested []string
		groups    []V2FilterRuleGroup
	}{
		{name: "no requested groups"},
		{name: "unknown group", requested: []string{"missing"}, groups: []V2FilterRuleGroup{{Name: "known", RuleString: "4K"}}},
		{name: "empty rule string", requested: []string{"g"}, groups: []V2FilterRuleGroup{{Name: "g", RuleString: ""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FilterV2Candidates(
				[]Candidate{candidate}, tt.requested, tt.groups, nil,
				V2FilterMedia{MediaType: string(media.TypeMovie)}, time.Now(),
			)
			if err != nil {
				t.Fatalf("FilterV2Candidates() error = %v", err)
			}
			if len(got) != 1 || got[0].Resource.PriOrder != 42 {
				t.Fatalf("result = %#v, want unchanged candidate", got)
			}
		})
	}

	_, err := FilterV2Candidates(
		[]Candidate{candidate}, []string{"g"}, []V2FilterRuleGroup{{Name: "g", RuleString: "   "}}, nil,
		V2FilterMedia{MediaType: string(media.TypeMovie)}, time.Now(),
	)
	if !errors.Is(err, ErrInvalidV2FilterRules) {
		t.Fatalf("whitespace rule error = %v, want ErrInvalidV2FilterRules", err)
	}
}

func TestFilterV2CandidatesUsesLazyV2BooleanEvaluation(t *testing.T) {
	candidate := v2FilterCandidate("candidate", "Example HDR", "")
	tests := []struct {
		name       string
		expression string
		custom     []V2CustomFilterRule
		wantCount  int
	}{
		{name: "unknown is false", expression: "UNKNOWN"},
		{name: "negated unknown is true", expression: "!UNKNOWN", wantCount: 1},
		{name: "rule ids are case sensitive", expression: "hdr"},
		{name: "valid prefix ignores trailing input", expression: "HDR trailing", wantCount: 1},
		{name: "or short circuit skips invalid regex", expression: "HDR | BROKEN", custom: []V2CustomFilterRule{{ID: "BROKEN", Include: "("}}, wantCount: 1},
		{name: "and short circuit skips invalid regex", expression: "UNKNOWN & BROKEN", custom: []V2CustomFilterRule{{ID: "BROKEN", Include: "("}}},
		{name: "numeric letter atom", expression: "4K", wantCount: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FilterV2Candidates(
				[]Candidate{candidate}, []string{"g"}, []V2FilterRuleGroup{{Name: "g", RuleString: tt.expression}},
				tt.custom, V2FilterMedia{}, time.Now(),
			)
			if err != nil {
				t.Fatalf("FilterV2Candidates() error = %v", err)
			}
			if len(got) != tt.wantCount {
				t.Fatalf("len(result) = %d, want %d", len(got), tt.wantCount)
			}
		})
	}

	got, err := FilterV2Candidates(
		[]Candidate{candidate}, []string{"g"}, []V2FilterRuleGroup{{Name: "g", RuleString: "HDR > ("}}, nil,
		V2FilterMedia{}, time.Now(),
	)
	if err != nil || len(got) != 1 {
		t.Fatalf("unreached invalid lower level = (%#v, %v), want successful first level", got, err)
	}
}

func TestFilterV2CandidatesPreservesUnboundedV2PriorityLevels(t *testing.T) {
	levels := make([]string, 102)
	for index := range levels {
		levels[index] = fmt.Sprintf("MISS%dX", index)
	}
	levels[100] = "ZERO"
	levels[101] = "NEGATIVE"
	candidates := []Candidate{
		v2FilterCandidate("zero", "zero", ""),
		v2FilterCandidate("negative", "negative", ""),
	}
	custom := []V2CustomFilterRule{
		{ID: "ZERO", Include: "zero"},
		{ID: "NEGATIVE", Include: "negative"},
	}

	got, err := FilterV2Candidates(
		candidates, []string{"g"}, []V2FilterRuleGroup{{Name: "g", RuleString: strings.Join(levels, " > ")}},
		custom, V2FilterMedia{}, time.Now(),
	)
	if err != nil {
		t.Fatalf("FilterV2Candidates() error = %v", err)
	}
	if keys, priorities := v2FilteredKeysAndPriorities(got); !reflect.DeepEqual(keys, []string{"zero", "negative"}) ||
		!reflect.DeepEqual(priorities, []int{0, -1}) {
		t.Fatalf("result = (%v, %v), want ([zero negative], [0 -1])", keys, priorities)
	}
}

func TestFilterV2CandidatesMatchesV2RuleFields(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	zero, one := float64(0), float64(1)
	tests := []struct {
		name       string
		candidate  Candidate
		expression string
		custom     []V2CustomFilterRule
		facts      V2FilterMedia
		want       bool
	}{
		{name: "builtin lookbehind regex", candidate: v2FilterCandidate("blu", "Example UHD BluRay HEVC", ""), expression: "BLU", want: true},
		{name: "labels field replaces nonempty default", candidate: func() Candidate {
			item := v2FilterCandidate("labels", "官方 Example", "")
			item.Resource.Labels = []string{"normal"}
			return item
		}(), expression: "GZ"},
		{name: "empty selected field falls back to default", candidate: v2FilterCandidate("labels-fallback", "官方 Example", ""), expression: "GZ", want: true},
		{name: "custom rule overrides builtin", candidate: v2FilterCandidate("override", "Example 1080p", ""), expression: "4K", custom: []V2CustomFilterRule{{ID: "4K", Include: "1080p"}}, want: true},
		{name: "tmdb original language bypasses regex", candidate: v2FilterCandidate("language", "Example", ""), expression: "CNVOI", facts: V2FilterMedia{OriginalLanguage: "zh"}, want: true},
		{name: "free rejects nil", candidate: v2FilterCandidate("free-nil", "Example", ""), expression: "FREE"},
		{name: "free accepts zero", candidate: func() Candidate {
			item := v2FilterCandidate("free-zero", "Example", "")
			item.Resource.DownloadVolumeFactor = &zero
			return item
		}(), expression: "FREE", want: true},
		{name: "free rejects one", candidate: func() Candidate {
			item := v2FilterCandidate("free-one", "Example", "")
			item.Resource.DownloadVolumeFactor = &one
			return item
		}(), expression: "FREE"},
		{name: "seeders inclusive", candidate: func() Candidate {
			item := v2FilterCandidate("seeders", "Example", "")
			item.Resource.Seeders = 5
			return item
		}(), expression: "CUSTOM", custom: []V2CustomFilterRule{{ID: "CUSTOM", Seeders: "5"}}, want: true},
		{name: "subtitle episodes divide size", candidate: func() Candidate {
			item := v2FilterCandidate("size", "Example S01", "E01-E04")
			item.Resource.SizeBytes = 400 * 1024 * 1024
			return item
		}(), expression: "CUSTOM", custom: []V2CustomFilterRule{{ID: "CUSTOM", SizeRange: "100-100"}}, want: true},
		{name: "publish lower boundary inclusive", candidate: func() Candidate {
			item := v2FilterCandidate("publish", "Example", "")
			item.Resource.PublishedAt = now.Add(-60 * time.Minute)
			return item
		}(), expression: "CUSTOM", custom: []V2CustomFilterRule{{ID: "CUSTOM", PublishTime: "60-120"}}, want: true},
		{name: "future partial minute floors negative", candidate: func() Candidate {
			item := v2FilterCandidate("future", "Example", "")
			item.Resource.PublishedAt = now.Add(30 * time.Second)
			return item
		}(), expression: "CUSTOM", custom: []V2CustomFilterRule{{ID: "CUSTOM", PublishTime: "0"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FilterV2Candidates(
				[]Candidate{tt.candidate}, []string{"g"}, []V2FilterRuleGroup{{Name: "g", RuleString: tt.expression}},
				tt.custom, tt.facts, now,
			)
			if err != nil {
				t.Fatalf("FilterV2Candidates() error = %v", err)
			}
			if (len(got) == 1) != tt.want {
				t.Fatalf("matched = %t, want %t", len(got) == 1, tt.want)
			}
		})
	}
}

func TestFilterV2CandidatesRejectsReachedInvalidRules(t *testing.T) {
	candidate := v2FilterCandidate("candidate", "Example", "")
	tests := []struct {
		name       string
		expression string
		custom     []V2CustomFilterRule
	}{
		{name: "invalid expression", expression: "("},
		{name: "invalid regex", expression: "BROKEN", custom: []V2CustomFilterRule{{ID: "BROKEN", Include: "("}}},
		{name: "invalid size", expression: "BROKEN", custom: []V2CustomFilterRule{{ID: "BROKEN", SizeRange: "one-two"}}},
		{name: "invalid seeders", expression: "BROKEN", custom: []V2CustomFilterRule{{ID: "BROKEN", Seeders: "many"}}},
		{name: "invalid publish time", expression: "BROKEN", custom: []V2CustomFilterRule{{ID: "BROKEN", PublishTime: "soon"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FilterV2Candidates(
				[]Candidate{candidate}, []string{"g"}, []V2FilterRuleGroup{{Name: "g", RuleString: tt.expression}},
				tt.custom, V2FilterMedia{}, time.Now(),
			)
			if !errors.Is(err, ErrInvalidV2FilterRules) {
				t.Fatalf("error = %v, want ErrInvalidV2FilterRules", err)
			}
		})
	}
}

func v2FilterCandidate(key, title, description string) Candidate {
	return Candidate{
		Key: key,
		Resource: indexer.Resource{
			SiteID: 1, Site: "Fixture", Title: title, Description: description,
			DownloadID: key, SizeBytes: 1024,
		},
		Parsed: media.Parse(title, media.Options{}),
	}
}

func v2FilteredKeysAndPriorities(candidates []Candidate) ([]string, []int) {
	keys := make([]string, 0, len(candidates))
	priorities := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		keys = append(keys, candidate.Key)
		priorities = append(priorities, candidate.Resource.PriOrder)
	}
	return keys, priorities
}
