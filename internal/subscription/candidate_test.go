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

func TestSelectCandidatesFiltersResources(t *testing.T) {
	t.Parallel()
	season := 1
	query := Query{
		Title: "Example Show", MediaType: media.TypeTV, Year: "2026", Season: &season,
		SiteIDs: []int{2}, Include: []string{"WEB-DL"}, Exclude: []string{"CAM"},
		Filter: "HEVC & (1080P | 2160P)", MissingEpisodes: []Episode{{Season: 1, Episode: 2}},
	}
	resources := []indexer.Resource{
		candidateResource(2, "accepted", "Example.Show.S01E02.2026.1080p.WEB-DL.HEVC", 4, time.Time{}),
		candidateResource(1, "wrong-site", "Example.Show.S01E02.2026.1080p.WEB-DL.HEVC", 40, time.Time{}),
		candidateResource(2, "wrong-title", "Other.Show.S01E02.2026.1080p.WEB-DL.HEVC", 40, time.Time{}),
		candidateResource(2, "wrong-year", "Example.Show.S01E02.2025.1080p.WEB-DL.HEVC", 40, time.Time{}),
		candidateResource(2, "wrong-season", "Example.Show.S02E02.2026.1080p.WEB-DL.HEVC", 40, time.Time{}),
		candidateResource(2, "wrong-episode", "Example.Show.S01E03.2026.1080p.WEB-DL.HEVC", 40, time.Time{}),
		candidateResource(2, "excluded", "Example.Show.S01E02.2026.1080p.WEB-DL.HEVC.CAM", 40, time.Time{}),
		candidateResource(2, "filter-miss", "Example.Show.S01E02.2026.720p.WEB-DL.HEVC", 40, time.Time{}),
	}

	got, err := SelectCandidates(query, resources)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Resource.DownloadID != "accepted" {
		t.Fatalf("SelectCandidates() = %#v, want accepted resource", got)
	}
	if !reflect.DeepEqual(got[0].CoveredEpisodes, []Episode{{Season: 1, Episode: 2}}) {
		t.Fatalf("CoveredEpisodes = %#v", got[0].CoveredEpisodes)
	}
}

func TestSelectCandidatesAppliesV2ReleasePatternsAsRequiredFilters(t *testing.T) {
	t.Parallel()
	query := Query{
		Title: "Example Movie", MediaType: media.TypeMovie,
		Quality: "WEB-?DL|WEB-?RIP", Resolution: "1080[pi]|x1080", Effect: "HDR10|DOVI",
	}
	resources := []indexer.Resource{
		candidateResource(1, "accepted", "Example.Movie.2026.1080p.WEB-DL.HDR10", 1, time.Time{}),
		candidateResource(1, "wrong-quality", "Example.Movie.2026.1080p.BluRay.HDR10", 10, time.Time{}),
		candidateResource(1, "wrong-resolution", "Example.Movie.2026.2160p.WEB-DL.HDR10", 10, time.Time{}),
		candidateResource(1, "wrong-effect", "Example.Movie.2026.1080p.WEB-DL.SDR", 10, time.Time{}),
	}

	got, err := SelectCandidates(query, resources)
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"accepted"}) {
		t.Fatalf("release pattern candidates = %v", identifiers)
	}
}

func TestSelectCandidatesMatchesV2AdditionalFilterFields(t *testing.T) {
	t.Parallel()
	zero, one := float64(0), float64(1)
	accepted := candidateResource(1, "accepted", "Example.Movie.2026.1080p.WEB-DL", 1, time.Time{})
	accepted.Description = "Chinese subtitle"
	accepted.Labels = []string{"official"}
	accepted.UploadVolumeFactor = &one
	accepted.DownloadVolumeFactor = &zero
	accepted.Category = "must-not-be-filtered"

	got, err := SelectCandidates(Query{
		Title: "Example Movie", MediaType: media.TypeMovie,
		Include: []string{`Chinese subtitle.*official.*免费`},
		Exclude: []string{"must-not-be-filtered"},
	}, []indexer.Resource{accepted})
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"accepted"}) {
		t.Fatalf("V2 additional filter fields = %v", identifiers)
	}
}

func TestSelectCandidatesMatchesV2ReleasePatternsAgainstTitleOnly(t *testing.T) {
	t.Parallel()
	resource := candidateResource(1, "subtitle-only", "Example.Movie.2026", 1, time.Time{})
	resource.Description = "1080p WEB-DL HDR10"
	got, err := SelectCandidates(Query{
		Title: "Example Movie", MediaType: media.TypeMovie,
		Quality: "WEB-DL", Resolution: "1080p", Effect: "HDR10",
	}, []indexer.Resource{resource})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("title-only release filters accepted subtitle match: %#v", got)
	}
}

func TestSelectCandidatesAppliesIncludeAndExcludeRegularExpressions(t *testing.T) {
	t.Parallel()
	query := Query{
		Title: "Example Movie", MediaType: media.TypeMovie,
		Include: []string{"WEB-?DL", "10BIT|HDR"}, Exclude: []string{"CAM|TELESYNC"},
	}
	resources := []indexer.Resource{
		candidateResource(1, "accepted", "Example.Movie.2026.1080p.WEB-DL.10bit", 1, time.Time{}),
		candidateResource(1, "missing-include", "Example.Movie.2026.1080p.WEB-DL.SDR", 10, time.Time{}),
		candidateResource(1, "excluded", "Example.Movie.2026.1080p.WEB-DL.10bit.CAM", 10, time.Time{}),
	}
	got, err := SelectCandidates(query, resources)
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"accepted"}) {
		t.Fatalf("include/exclude regex candidates = %v", identifiers)
	}
}

func TestSelectCandidatesSupportsV2PythonStyleRegularExpressions(t *testing.T) {
	t.Parallel()
	resources := []indexer.Resource{
		candidateResource(1, "accepted", "Example.Movie.2026.BluRay.RIP", 1, time.Time{}),
		candidateResource(1, "excluded", "Example.Movie.2026.WEB-RIP", 1, time.Time{}),
	}
	got, err := SelectCandidates(Query{
		Title: "Example Movie", MediaType: media.TypeMovie,
		Include: []string{`(?<!WEB-)RIP`},
	}, resources)
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"accepted"}) {
		t.Fatalf("Python-style regex candidates = %v", identifiers)
	}
}

func TestSelectCandidatesRejectsInvalidOrOversizedReleasePatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*Query, string)
		pattern   string
	}{
		{name: "invalid quality", pattern: "[", configure: func(query *Query, pattern string) { query.Quality = pattern }},
		{name: "invalid resolution", pattern: "[", configure: func(query *Query, pattern string) { query.Resolution = pattern }},
		{name: "invalid effect", pattern: "[", configure: func(query *Query, pattern string) { query.Effect = pattern }},
		{name: "invalid include", pattern: "[", configure: func(query *Query, pattern string) { query.Include = []string{pattern} }},
		{name: "invalid exclude", pattern: "[", configure: func(query *Query, pattern string) { query.Exclude = []string{pattern} }},
		{name: "oversized quality", pattern: strings.Repeat("a", maxCandidatePatternLength+1), configure: func(query *Query, pattern string) { query.Quality = pattern }},
		{name: "oversized resolution", pattern: strings.Repeat("a", maxCandidatePatternLength+1), configure: func(query *Query, pattern string) { query.Resolution = pattern }},
		{name: "oversized effect", pattern: strings.Repeat("a", maxCandidatePatternLength+1), configure: func(query *Query, pattern string) { query.Effect = pattern }},
		{name: "oversized include", pattern: strings.Repeat("a", maxCandidatePatternLength+1), configure: func(query *Query, pattern string) { query.Include = []string{pattern} }},
		{name: "oversized exclude", pattern: strings.Repeat("a", maxCandidatePatternLength+1), configure: func(query *Query, pattern string) { query.Exclude = []string{pattern} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query := Query{Title: "Example Movie", MediaType: media.TypeMovie}
			tt.configure(&query, tt.pattern)
			_, err := SelectCandidates(query, nil)
			if !errors.Is(err, ErrInvalidCandidateQuery) {
				t.Fatalf("SelectCandidates() error = %v", err)
			}
		})
	}
}

func TestSelectCandidatesAcceptsPatternBoundariesAndUsesAllFilterTextSources(t *testing.T) {
	t.Parallel()
	boundary := strings.Repeat("a", maxCandidatePatternLength)
	if _, err := SelectCandidates(Query{
		Title: "Example Movie", MediaType: media.TypeMovie,
		Quality: boundary, Resolution: " ", Effect: "\t",
		Include: []string{" ", "\t"}, Exclude: []string{" "},
	}, nil); err != nil {
		t.Fatalf("512-byte or blank pattern rejected: %v", err)
	}

	accepted := candidateResource(1, "accepted-filter-sources", "Example.Movie.2026", 1, time.Time{})
	accepted.Labels = []string{"hdr10"}
	accepted.Description = "Web-Dl"
	rejected := candidateResource(1, "missing-description", "Example.Movie.2026", 1, time.Time{})
	rejected.Labels = []string{"HDR10"}
	got, err := SelectCandidates(Query{
		Title: "Example Movie", MediaType: media.TypeMovie,
		Include: []string{"HDR10", "WEB-DL"},
	}, []indexer.Resource{accepted, rejected})
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"accepted-filter-sources"}) {
		t.Fatalf("filter text source candidates = %v", identifiers)
	}
}

func TestSelectCandidatesMatchesAliasesAndIMDBIdentity(t *testing.T) {
	t.Parallel()
	alias := candidateResource(1, "alias", "Example.Movie.2026.1080p.WEB-DL", 1, time.Time{})
	exactID := candidateResource(1, "imdb", "Localized.Release.2026.1080p.WEB-DL", 1, time.Time{})
	exactID.IMDBID = "TT1234567"
	wrongID := candidateResource(1, "wrong-imdb", "Example.Movie.2026.1080p.WEB-DL", 1, time.Time{})
	wrongID.IMDBID = "tt7654321"

	got, err := SelectCandidates(Query{
		Title: "Local Title", Aliases: []string{"Example Movie"}, Year: "2026",
		MediaType: media.TypeMovie, IMDBID: "tt1234567",
	}, []indexer.Resource{alias, exactID, wrongID})
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"imdb", "alias"}) {
		t.Fatalf("identity candidates = %v", identifiers)
	}
}

func TestSelectCandidatesMatchesMissingEpisodes(t *testing.T) {
	t.Parallel()
	season := 1
	query := Query{
		Title: "Example Show", MediaType: media.TypeTV, Season: &season,
		MissingEpisodes: []Episode{
			{Season: 1, Episode: 2}, {Season: 1, Episode: 3}, {Season: 1, Episode: 4},
		},
	}
	resources := []indexer.Resource{
		candidateResource(1, "episode-one", "Example.Show.S01E01.1080p.WEB-DL", 10, time.Time{}),
		candidateResource(1, "episode-two", "Example.Show.S01E02.1080p.WEB-DL", 10, time.Time{}),
		candidateResource(1, "episode-range", "Example.Show.S01E02-S01E03.1080p.WEB-DL", 10, time.Time{}),
		candidateResource(1, "season-pack", "Example.Show.S01.1080p.BluRay", 10, time.Time{}),
		candidateResource(1, "other-season", "Example.Show.S02E02.1080p.WEB-DL", 10, time.Time{}),
	}

	got, err := SelectCandidates(query, resources)
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"season-pack", "episode-range", "episode-two"}) {
		t.Fatalf("candidate order = %v", identifiers)
	}
	wantCoverage := [][]Episode{
		{{Season: 1, Episode: 2}, {Season: 1, Episode: 3}, {Season: 1, Episode: 4}},
		{{Season: 1, Episode: 2}, {Season: 1, Episode: 3}},
		{{Season: 1, Episode: 2}},
	}
	for index := range got {
		if !reflect.DeepEqual(got[index].CoveredEpisodes, wantCoverage[index]) {
			t.Fatalf("candidate %s coverage = %#v, want %#v", got[index].Resource.DownloadID, got[index].CoveredEpisodes, wantCoverage[index])
		}
	}
}

func TestSelectCandidatesSupportsSpecialSeason(t *testing.T) {
	t.Parallel()
	season := 0
	got, err := SelectCandidates(Query{
		Title: "Example Show", MediaType: media.TypeTV, Season: &season,
		MissingEpisodes: []Episode{{Season: 0, Episode: 1}},
	}, []indexer.Resource{
		candidateResource(1, "special", "Example.Show.S00E01.1080p.WEB-DL", 1, time.Time{}),
		candidateResource(1, "regular", "Example.Show.S01E01.1080p.WEB-DL", 1, time.Time{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if identifiers := candidateIDs(got); !reflect.DeepEqual(identifiers, []string{"special"}) {
		t.Fatalf("special season candidates = %v", identifiers)
	}
}

func TestSelectCandidatesUsesDeterministicSortAndDeduplication(t *testing.T) {
	t.Parallel()
	newer := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)
	query := Query{
		Title: "Example Movie", MediaType: media.TypeMovie, SiteIDs: []int{2, 1},
		QualityOrder: []string{"1080P", "2160P"},
	}
	resources := []indexer.Resource{
		candidateResource(1, "site-one", "Example.Movie.2026.2160p.BluRay.REMUX", 100, newer),
		candidateResource(2, "preferred-quality", "Example.Movie.2026.1080p.WEB-DL", 2, older),
		candidateResource(2, "more-seeders", "Example.Movie.2026.1080p.WEB-DL", 20, older),
		candidateResource(2, "newer", "Example.Movie.2026.1080p.WEB-DL", 20, newer),
		candidateResource(2, "duplicate", "Example.Movie.2026.720p.WEB-DL", 1, older),
		candidateResource(2, "duplicate", "Example.Movie.2026.1080p.WEB-DL", 50, newer),
	}

	forward, err := SelectCandidates(query, resources)
	if err != nil {
		t.Fatal(err)
	}
	reversedInput := append([]indexer.Resource(nil), resources...)
	for left, right := 0, len(reversedInput)-1; left < right; left, right = left+1, right-1 {
		reversedInput[left], reversedInput[right] = reversedInput[right], reversedInput[left]
	}
	reversed, err := SelectCandidates(query, reversedInput)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("input order changed candidates:\nforward=%#v\nreverse=%#v", forward, reversed)
	}
	want := []string{"duplicate", "newer", "more-seeders", "preferred-quality", "site-one"}
	if identifiers := candidateIDs(forward); !reflect.DeepEqual(identifiers, want) {
		t.Fatalf("candidate order = %v, want %v", identifiers, want)
	}
	if forward[0].Parsed.Resolution != "1080p" {
		t.Fatalf("deduplication retained %q, want 1080p duplicate", forward[0].Parsed.Resolution)
	}
}

func TestSelectCandidatesReturnsStableOpaqueKeys(t *testing.T) {
	t.Parallel()
	secret := "torrent-42?passkey=do-not-expose"
	base := candidateResource(7, secret, "Example.Movie.2026.1080p.WEB-DL", 1, time.Time{})
	changedTitle := base
	changedTitle.Title = "Example.Movie.2026.2160p.BluRay"

	first, ok := candidateKey(base)
	if !ok {
		t.Fatal("candidateKey() rejected valid resource")
	}
	second, ok := candidateKey(changedTitle)
	if !ok || first != second {
		t.Fatalf("stable key changed: first=%q second=%q", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 || strings.Contains(first, secret) {
		t.Fatalf("candidate key is not an opaque SHA-256 value: %q", first)
	}
	changedSite := base
	changedSite.SiteID++
	other, _ := candidateKey(changedSite)
	if other == first {
		t.Fatal("different sites must not share a candidate key")
	}
}

func TestSelectCandidatesEnforcesBounds(t *testing.T) {
	t.Parallel()
	resources := make([]indexer.Resource, MaxCandidateLimit+25)
	for index := range resources {
		resources[index] = candidateResource(1, fmt.Sprintf("resource-%03d", index), "Example.Movie.2026.1080p.WEB-DL", index, time.Time{})
	}
	got, err := SelectCandidates(Query{Title: "Example Movie", MediaType: media.TypeMovie, Limit: MaxCandidateLimit}, resources)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxCandidateLimit {
		t.Fatalf("candidate count = %d, want %d", len(got), MaxCandidateLimit)
	}

	tooMany := make([]indexer.Resource, maxCandidateInputs+1)
	if _, err := SelectCandidates(Query{Title: "Example Movie", MediaType: media.TypeMovie}, tooMany); !errors.Is(err, ErrTooManyCandidateInputs) {
		t.Fatalf("large input error = %v", err)
	}
}

func TestMatchV2DownloadCandidatesPreservesOrderWithoutPreSortLimit(t *testing.T) {
	t.Parallel()

	resources := make([]indexer.Resource, MaxCandidateLimit+1)
	for index := range resources {
		resources[index] = indexer.Resource{
			SiteID:     1,
			Site:       "Fixture",
			Title:      "Example Movie 2026 1080p WEB-DL",
			DownloadID: fmt.Sprintf("resource-%03d", index),
		}
	}
	resources[len(resources)-1].PriOrder = 100
	got, err := MatchV2DownloadCandidates(
		Query{Title: "Example Movie", Year: "2026", MediaType: media.TypeMovie, Limit: MaxCandidateLimit},
		resources,
	)
	if err != nil {
		t.Fatal(err)
	}
	lastID := fmt.Sprintf("resource-%03d", MaxCandidateLimit)
	if len(got) != len(resources) || got[0].Resource.DownloadID != "resource-000" ||
		got[len(got)-1].Resource.DownloadID != lastID {
		t.Fatalf("V2 matched order/count = first:%q last:%q count:%d",
			got[0].Resource.DownloadID, got[len(got)-1].Resource.DownloadID, len(got))
	}
	sorted := SortV2Candidates(got, []string{"torrent"}, nil)
	if sorted[0].Resource.DownloadID != lastID {
		t.Fatalf("V2 sort lost post-limit candidate: first = %q", sorted[0].Resource.DownloadID)
	}
}

func TestMatchV2DownloadCandidatesKeepsFirstDuplicate(t *testing.T) {
	t.Parallel()

	resources := []indexer.Resource{
		{SiteID: 1, Site: "First", Title: "Example Movie 2026", DownloadID: "same", Seeders: 1},
		{SiteID: 1, Site: "Second", Title: "Example Movie 2026", DownloadID: "same", Seeders: 999},
	}
	got, err := MatchV2DownloadCandidates(
		Query{Title: "Example Movie", Year: "2026", MediaType: media.TypeMovie}, resources,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Resource.Site != "First" {
		t.Fatalf("V2 duplicate projection = %#v", got)
	}
}

func TestSelectCandidatesRejectsInvalidQueries(t *testing.T) {
	t.Parallel()
	season := 1
	tests := []struct {
		name  string
		query Query
	}{
		{name: "missing title", query: Query{MediaType: media.TypeMovie}},
		{name: "invalid media type", query: Query{Title: "Example", MediaType: media.TypeUnknown}},
		{name: "negative season", query: Query{Title: "Example", MediaType: media.TypeTV, Season: candidateInt(-1)}},
		{name: "limit above maximum", query: Query{Title: "Example", MediaType: media.TypeMovie, Limit: MaxCandidateLimit + 1}},
		{name: "invalid site", query: Query{Title: "Example", MediaType: media.TypeMovie, SiteIDs: []int{0}}},
		{name: "invalid filter", query: Query{Title: "Example", MediaType: media.TypeMovie, Filter: "HDR &"}},
		{name: "missing episode outside season", query: Query{Title: "Example", MediaType: media.TypeTV, Season: &season, MissingEpisodes: []Episode{{Season: 2, Episode: 1}}}},
		{name: "invalid missing episode", query: Query{Title: "Example", MediaType: media.TypeTV, MissingEpisodes: []Episode{{Season: 1, Episode: 0}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := SelectCandidates(test.query, nil); !errors.Is(err, ErrInvalidCandidateQuery) {
				t.Fatalf("SelectCandidates() error = %v", err)
			}
		})
	}
}

func FuzzCandidateKeyIsStableAndOpaque(f *testing.F) {
	f.Add("42")
	f.Add("torrent?id=1&passkey=secret")
	f.Add("")
	f.Fuzz(func(t *testing.T, locator string) {
		resource := indexer.Resource{SiteID: 1, Site: "fixture", DownloadID: locator}
		if strings.TrimSpace(locator) == "" {
			resource.DownloadURL = "https://fixture.invalid/download"
		}
		first, firstOK := candidateKey(resource)
		second, secondOK := candidateKey(resource)
		if firstOK != secondOK || first != second {
			t.Fatalf("candidateKey() is unstable: (%q, %t) != (%q, %t)", first, firstOK, second, secondOK)
		}
		if firstOK && (!strings.HasPrefix(first, "sha256:") || len(first) != 71) {
			t.Fatalf("candidateKey() = %q", first)
		}
	})
}

func FuzzSelectCandidatesIsOrderIndependent(f *testing.F) {
	f.Add("one", "two", uint16(1), uint16(2))
	f.Add("same", "same", uint16(100), uint16(1))
	f.Fuzz(func(t *testing.T, firstID, secondID string, firstSeeders, secondSeeders uint16) {
		if strings.TrimSpace(firstID) == "" {
			firstID = "first"
		}
		if strings.TrimSpace(secondID) == "" {
			secondID = "second"
		}
		resources := []indexer.Resource{
			candidateResource(1, firstID, "Example.Movie.2026.1080p.WEB-DL", int(firstSeeders), time.Unix(1, 0)),
			candidateResource(1, secondID, "Example.Movie.2026.2160p.BluRay", int(secondSeeders), time.Unix(2, 0)),
		}
		query := Query{Title: "Example Movie", MediaType: media.TypeMovie}
		forward, forwardErr := SelectCandidates(query, resources)
		reverse, reverseErr := SelectCandidates(query, []indexer.Resource{resources[1], resources[0]})
		if forwardErr != nil || reverseErr != nil {
			t.Fatalf("SelectCandidates() errors = (%v, %v)", forwardErr, reverseErr)
		}
		if !reflect.DeepEqual(forward, reverse) {
			t.Fatalf("input order changed output: forward=%#v reverse=%#v", forward, reverse)
		}
	})
}

func BenchmarkSelectCandidates(b *testing.B) {
	resources := make([]indexer.Resource, 1_000)
	for index := range resources {
		resources[index] = candidateResource(1, fmt.Sprintf("resource-%04d", index), "Example.Movie.2026.1080p.WEB-DL.HEVC", index%100, time.Unix(int64(index), 0))
	}
	query := Query{Title: "Example Movie", MediaType: media.TypeMovie, Limit: 100}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := SelectCandidates(query, resources); err != nil {
			b.Fatal(err)
		}
	}
}

func candidateResource(siteID int, downloadID, title string, seeders int, publishedAt time.Time) indexer.Resource {
	return indexer.Resource{
		SiteID: siteID, Site: fmt.Sprintf("site-%d", siteID), Title: title,
		DetailURL: "https://fixture.invalid/details/" + downloadID, DownloadID: downloadID,
		Seeders: seeders, PublishedAt: publishedAt,
	}
}

func candidateIDs(candidates []Candidate) []string {
	result := make([]string, len(candidates))
	for index := range candidates {
		result[index] = candidates[index].Resource.DownloadID
	}
	return result
}

func candidateInt(value int) *int { return &value }
