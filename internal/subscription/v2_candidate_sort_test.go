package subscription

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
)

func TestSortV2CandidatesMatchesDefaultPriorityOrder(t *testing.T) {
	t.Parallel()

	candidates := []Candidate{
		v2SortCandidate("low-rule", "SiteA", "Example.S01E01", 80, 1000, 100),
		v2SortCandidate("upload-low", "SiteB", "Example.S01E01", 90, 500, 100),
		v2SortCandidate("upload-high-seeders-low", "SiteA", "Example.S01E01", 90, 1, 1),
		v2SortCandidate("upload-high-seeders-high", "SiteA", "Example.S01E01", 90, 2, 1),
	}

	got := SortV2Candidates(candidates, nil, map[string]float64{"SiteA": 200, "SiteB": 100})
	want := []string{"upload-high-seeders-high", "upload-high-seeders-low", "upload-low", "low-rule"}
	if keys := v2SortCandidateKeys(got); !reflect.DeepEqual(keys, want) {
		t.Fatalf("SortV2Candidates() keys = %v, want %v", keys, want)
	}
}

func TestSortV2CandidatesUsesV2MediaTitleBeforePriorityFields(t *testing.T) {
	t.Parallel()

	lowTitle := v2SortCandidate("low-title", "SiteA", "Release.S01E01", 999, 9999, 0)
	lowTitle.MediaTitle = "Alpha Show"
	highTitle := v2SortCandidate("high-title", "SiteB", "Release.S01E01", 0, 1, 999)
	highTitle.MediaTitle = "Zulu Show"

	got := SortV2Candidates([]Candidate{lowTitle, highTitle}, []string{"torrent", "site", "upload", "seeder"}, nil)
	if want := []string{"high-title", "low-title"}; !reflect.DeepEqual(v2SortCandidateKeys(got), want) {
		t.Fatalf("SortV2Candidates() keys = %v, want %v", v2SortCandidateKeys(got), want)
	}
}

func TestV2MediaTitlePrefixMatchesPythonLjustForUnicode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		title string
		want  int
	}{
		{name: "empty", title: "", want: 200},
		{name: "unicode", title: "剧集", want: 200},
		{name: "exact width", title: strings.Repeat("a", 200), want: 200},
		{name: "long title is not truncated", title: strings.Repeat("片", 201), want: 201},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := v2MediaTitlePrefix(test.title)
			if len([]rune(got)) != test.want {
				t.Fatalf("v2MediaTitlePrefix(%q) rune length = %d, want %d", test.title, len([]rune(got)), test.want)
			}
		})
	}
}

func TestSortV2CandidatesRanksSeasonPackAndEpisodeCountLast(t *testing.T) {
	t.Parallel()

	single := v2SortCandidate("single", "Site", "Example.S01E01", 0, 1, 0)
	collection := v2SortCandidate("collection", "Site", "Example.S01E01-E04", 0, 1, 0)
	collection.Parsed = media.ParseResult{
		Type: media.TypeTV, BeginSeason: 1, EndSeason: 1, BeginEpisode: 1, EndEpisode: 4,
	}
	seasonPack := v2SortCandidate("season-pack", "Site", "Example.S01", 0, 1, 0)
	candidates := []Candidate{single, collection, seasonPack}

	got := SortV2Candidates(candidates, nil, nil)
	want := []string{"season-pack", "collection", "single"}
	if keys := v2SortCandidateKeys(got); !reflect.DeepEqual(keys, want) {
		t.Fatalf("SortV2Candidates() keys = %v, want %v", keys, want)
	}
}

func TestSortV2CandidatesHonorsConfiguredSiteRuleAndStableTies(t *testing.T) {
	t.Parallel()

	first := v2SortCandidate("first", "SiteA", "Example.S01E01", 0, 1, 5)
	first.MatchScore = 1
	first.QualityScore = 1
	first.Resource.SizeBytes = 1
	first.Resource.PublishedAt = time.Unix(1, 0)
	second := v2SortCandidate("second", "SiteB", "Example.S01E01", 100, 999, 2)
	second.MatchScore = 999999
	second.QualityScore = 999999
	second.Resource.SizeBytes = 999999
	second.Resource.PublishedAt = time.Unix(999999, 0)
	tied := v2SortCandidate("tied", "SiteA", "Example.S01E01", 0, 1, 5)

	got := SortV2Candidates(
		[]Candidate{first, second, tied},
		[]string{"site"},
		map[string]float64{"SiteB": 999999},
	)
	want := []string{"second", "first", "tied"}
	if keys := v2SortCandidateKeys(got); !reflect.DeepEqual(keys, want) {
		t.Fatalf("SortV2Candidates() keys = %v, want %v", keys, want)
	}
}

func TestSortV2CandidatesMatchesV2PaddedFloatingPointUploadOrder(t *testing.T) {
	t.Parallel()

	lower := v2SortCandidate("lower", "SiteA", "Example.S01E01", 0, 100, 0)
	higher := v2SortCandidate("higher", "SiteB", "Example.S01E01", 0, 1, 0)

	got := SortV2Candidates(
		[]Candidate{lower, higher},
		[]string{"upload"},
		map[string]float64{"SiteA": 1.25, "SiteB": 1.5},
	)
	if want := []string{"lower", "higher"}; !reflect.DeepEqual(v2SortCandidateKeys(got), want) {
		t.Fatalf("SortV2Candidates() keys = %v, want %v", v2SortCandidateKeys(got), want)
	}
}

func TestV2PythonFloatStringMatchesSiteUploadOracle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value float64
		want  string
	}{
		{name: "positive zero falls back to integer zero", value: 0, want: "000000000000000000000000000000"},
		{name: "negative zero falls back to integer zero", value: math.Copysign(0, -1), want: "000000000000000000000000000000"},
		{name: "one below fixed lower boundary", value: math.Nextafter(1e-4, 0), want: "0000000009.999999999999999e-05"},
		{name: "exact fixed lower boundary", value: 1e-4, want: "0000000000000000000000000.0001"},
		{name: "one above fixed lower boundary", value: math.Nextafter(1e-4, math.Inf(1)), want: "000000000.00010000000000000002"},
		{name: "scientific below lower boundary", value: 1e-5, want: "00000000000000000000000001e-05"},
		{name: "fraction within fixed range", value: 1.234567890123456e-4, want: "0000000000.0001234567890123456"},
		{name: "fraction below fixed range", value: 1.234567890123456e-5, want: "0000000001.234567890123456e-05"},
		{name: "exact one quadrillion stays fixed", value: 1e15, want: "0000000000001000000000000000.0"},
		{name: "fractional mantissa at exponent fifteen stays fixed", value: 1.234567890123456e15, want: "0000000000001234567890123456.0"},
		{name: "one below scientific upper boundary", value: math.Nextafter(1e16, 0), want: "0000000000009999999999999998.0"},
		{name: "exact scientific upper boundary", value: 1e16, want: "00000000000000000000000001e+16"},
		{name: "one above scientific upper boundary", value: math.Nextafter(1e16, math.Inf(1)), want: "000000001.0000000000000002e+16"},
		{name: "negative fixed value", value: -1e15, want: "00000000000-1000000000000000.0"},
		{name: "negative scientific value", value: -1e16, want: "000000000000000000000000-1e+16"},
		{name: "nan remains truthy", value: math.NaN(), want: "000000000000000000000000000nan"},
		{name: "positive infinity remains truthy", value: math.Inf(1), want: "000000000000000000000000000inf"},
		{name: "negative infinity remains truthy", value: math.Inf(-1), want: "00000000000000000000000000-inf"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := v2ZeroPad(v2PythonFloatString(test.value), 30)
			if got != test.want {
				t.Fatalf("padded V2 site upload = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSortV2CandidatesDoesNotTruncateStoredRules(t *testing.T) {
	t.Parallel()

	rules := make([]string, 16)
	for index := range rules {
		rules[index] = "unknown"
	}
	rules = append(rules, "site")
	preferred := v2SortCandidate("preferred", "SiteA", "Example.S01E01", 0, 1, 1)
	other := v2SortCandidate("other", "SiteB", "Example.S01E01", 0, 100, 9)

	got := SortV2Candidates([]Candidate{other, preferred}, rules, nil)
	if want := []string{"preferred", "other"}; !reflect.DeepEqual(v2SortCandidateKeys(got), want) {
		t.Fatalf("SortV2Candidates() keys = %v, want %v", v2SortCandidateKeys(got), want)
	}
}

func TestSortV2CandidatesIgnoresNonV2RuleTokensExactly(t *testing.T) {
	t.Parallel()

	seasonPack := v2SortCandidate("season-pack", "SiteA", "Example.S01", 0, 1, 9)
	singleEpisode := v2SortCandidate("single", "SiteB", "Example.S01E01", 0, 1, 1)

	got := SortV2Candidates([]Candidate{singleEpisode, seasonPack}, []string{" SITE "}, nil)
	if want := []string{"season-pack", "single"}; !reflect.DeepEqual(v2SortCandidateKeys(got), want) {
		t.Fatalf("SortV2Candidates() keys = %v, want %v", v2SortCandidateKeys(got), want)
	}
}

func v2SortCandidate(
	key string,
	site string,
	title string,
	priority int,
	seeders int,
	siteOrder int,
) Candidate {
	return Candidate{
		Key: key,
		Resource: indexer.Resource{
			Site: site, Title: title, PriOrder: priority, Seeders: seeders, SiteOrder: siteOrder,
		},
		Parsed: media.Parse(title, media.Options{}),
	}
}

func v2SortCandidateKeys(candidates []Candidate) []string {
	keys := make([]string, len(candidates))
	for index, candidate := range candidates {
		keys[index] = candidate.Key
	}
	return keys
}
