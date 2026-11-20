package subscriptionflow

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestRSSSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		adapters    []indexer.Adapter
		wantIDs     []int
		wantKeys    map[int]string
		wantNames   map[int]string
		wantNoCache map[int]bool
	}{
		{
			name: "missing capabilities are ignored",
			adapters: []indexer.Adapter{
				rssSourceTestAdapter{name: "plain"},
				rssSourceTestNoFetcherAdapter{
					rssSourceTestSiteAdapter: rssSourceTestSiteAdapter{
						rssSourceTestAdapter: rssSourceTestAdapter{name: "no-fetcher"},
						siteID:               1,
					},
					kind:    "test",
					version: 1,
				},
				rssSourceTestNoResolverAdapter{
					rssSourceTestSiteAdapter: rssSourceTestSiteAdapter{
						rssSourceTestAdapter: rssSourceTestAdapter{name: "no-resolver"},
						siteID:               2,
					},
					key: "cursor-2",
				},
			},
			wantIDs: []int{},
		},
		{
			name: "invalid source metadata is ignored",
			adapters: []indexer.Adapter{
				newRSSSourceTestAdapter("zero-site", 0, "cursor-1", "test", 1),
				newRSSSourceTestAdapter("blank-cursor", 1, " \t", "test", 1),
				newRSSSourceTestAdapter("blank-kind", 2, "cursor-2", " \t", 1),
				newRSSSourceTestAdapter("zero-version", 3, "cursor-3", "test", 0),
			},
			wantIDs: []int{},
		},
		{
			name: "valid sources are retained and the first duplicate wins",
			adapters: []indexer.Adapter{
				newRSSSourceTestAdapter("site-two-first", 2, "cursor-2-first", "test", 1),
				newRSSSourceTestAdapter("site-one", 1, "cursor-1", "test", 1),
				newRSSSourceTestAdapter("site-two-second", 2, "cursor-2-second", "test", 1),
			},
			wantIDs:   []int{1, 2},
			wantKeys:  map[int]string{1: "cursor-1", 2: "cursor-2-first"},
			wantNames: map[int]string{1: "site-one", 2: "site-two-first"},
		},
		{
			name: "optional no-cache policy is captured",
			adapters: []indexer.Adapter{func() rssSourceTestFullAdapter {
				adapter := newRSSSourceTestAdapter("m-team", 9, "cursor-9", "test", 1)
				adapter.noCache = true
				return adapter
			}()},
			wantIDs: []int{9}, wantNoCache: map[int]bool{9: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := rssSources(tt.adapters)
			if got == nil {
				t.Fatal("rssSources() returned a nil map")
			}
			if gotIDs := rssSourceIDs(got); !slices.Equal(gotIDs, tt.wantIDs) {
				t.Fatalf("rssSources() IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
			for siteID, wantKey := range tt.wantKeys {
				source := got[siteID]
				if source.siteID != siteID || source.cursorKey != wantKey {
					t.Errorf("rssSources()[%d] = {siteID:%d cursorKey:%q}", siteID, source.siteID, source.cursorKey)
				}
				if source.adapter == nil || source.adapter.Name() != tt.wantNames[siteID] {
					t.Errorf("rssSources()[%d] adapter name = %v, want %q", siteID, source.adapter, tt.wantNames[siteID])
				}
				if source.fetcher == nil || source.fetcher.RSSCursorKey() != wantKey {
					t.Errorf("rssSources()[%d] fetcher does not preserve cursor key %q", siteID, wantKey)
				}
			}
			for siteID, want := range tt.wantNoCache {
				if got[siteID].noCache != want {
					t.Errorf("rssSources()[%d].noCache = %t, want %t", siteID, got[siteID].noCache, want)
				}
			}
		})
	}
}

func TestOrderedRSSSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sources map[int]rssSource
		want    []int
	}{
		{name: "empty", sources: nil, want: []int{}},
		{
			name: "site ID order",
			sources: map[int]rssSource{
				3: {siteID: 3},
				1: {siteID: 1},
				2: {siteID: 2},
			},
			want: []int{1, 2, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := orderedRSSSources(tt.sources)
			gotIDs := make([]int, len(got))
			for index, source := range got {
				gotIDs[index] = source.siteID
			}
			if !slices.Equal(gotIDs, tt.want) {
				t.Fatalf("orderedRSSSources() IDs = %v, want %v", gotIDs, tt.want)
			}
		})
	}
}

func TestSelectedRSSSources(t *testing.T) {
	t.Parallel()

	sources := map[int]rssSource{
		1: {siteID: 1, cursorKey: "cursor-1"},
		2: {siteID: 2, cursorKey: "cursor-2"},
		3: {siteID: 3, cursorKey: "cursor-3"},
	}
	tests := []struct {
		name     string
		selected []int
		all      bool
		want     []int
	}{
		{name: "all ignores selection", selected: []int{99}, all: true, want: []int{1, 2, 3}},
		{name: "selected existing sites", selected: []int{3, 99, 1, 3}, want: []int{1, 3}},
		{name: "empty selection", want: []int{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := selectedRSSSources(sources, tt.selected, tt.all)
			if got == nil {
				t.Fatal("selectedRSSSources() returned a nil map")
			}
			if gotIDs := rssSourceIDs(got); !slices.Equal(gotIDs, tt.want) {
				t.Fatalf("selectedRSSSources() IDs = %v, want %v", gotIDs, tt.want)
			}
		})
	}

	selected := selectedRSSSources(sources, nil, true)
	delete(selected, 1)
	if _, ok := sources[1]; !ok {
		t.Fatal("selectedRSSSources() returned the source map instead of a copy")
	}
}

func TestRSSSourceIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sources map[int]rssSource
		want    []int
	}{
		{name: "nil map returns non-nil empty slice", sources: nil, want: []int{}},
		{name: "sorted IDs", sources: map[int]rssSource{9: {}, 2: {}, 5: {}}, want: []int{2, 5, 9}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := rssSourceIDs(tt.sources)
			if got == nil || !slices.Equal(got, tt.want) {
				t.Fatalf("rssSourceIDs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRSSSiteIDIntersection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		user     []int
		defaults []int
		initial  map[int]struct{}
		want     map[int]struct{}
		found    bool
	}{
		{name: "empty", want: map[int]struct{}{}},
		{
			name:     "intersection is accumulated",
			user:     []int{3, 1, 3, 9},
			defaults: []int{1, 2, 3},
			initial:  map[int]struct{}{7: {}},
			want:     map[int]struct{}{1: {}, 3: {}, 7: {}},
			found:    true,
		},
		{
			name:     "no intersection retains target",
			user:     []int{8, 9},
			defaults: []int{1, 2},
			initial:  map[int]struct{}{7: {}},
			want:     map[int]struct{}{7: {}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := maps.Clone(tt.initial)
			if target == nil {
				target = make(map[int]struct{})
			}
			found := rssSiteIDIntersection(tt.user, tt.defaults, target)
			if found != tt.found || !maps.Equal(target, tt.want) {
				t.Fatalf("rssSiteIDIntersection() = (%t, %v), want (%t, %v)", found, target, tt.found, tt.want)
			}
		})
	}
}

func TestNormalizedRSSSiteIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ids  []int
		want []int
	}{
		{name: "empty", ids: nil, want: []int{}},
		{name: "positive unique sorted", ids: []int{3, -1, 2, 3, 0, 1, 2}, want: []int{1, 2, 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := normalizedRSSSiteIDs(tt.ids); !slices.Equal(got, tt.want) {
				t.Fatalf("normalizedRSSSiteIDs(%v) = %v, want %v", tt.ids, got, tt.want)
			}
		})
	}
}

func TestValidExpectedRSSSiteIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ids  []int
		want bool
	}{
		{name: "empty", want: true},
		{name: "strictly increasing", ids: []int{1, 2, 9}, want: true},
		{name: "zero", ids: []int{0}, want: false},
		{name: "negative", ids: []int{-1, 2}, want: false},
		{name: "duplicate", ids: []int{1, 1}, want: false},
		{name: "descending", ids: []int{2, 1}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := validExpectedRSSSiteIDs(tt.ids); got != tt.want {
				t.Fatalf("validExpectedRSSSiteIDs(%v) = %t, want %t", tt.ids, got, tt.want)
			}
		})
	}
}

func TestActiveExpectedRSSSiteIDs(t *testing.T) {
	t.Parallel()

	sources := map[int]rssSource{1: {}, 3: {}}
	tests := []struct {
		name string
		ids  []int
		want []int
	}{
		{name: "empty", want: []int{}},
		{name: "available sites preserve input order", ids: []int{3, 2, 1, 3}, want: []int{3, 1, 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := activeExpectedRSSSiteIDs(tt.ids, sources); !slices.Equal(got, tt.want) {
				t.Fatalf("activeExpectedRSSSiteIDs(%v) = %v, want %v", tt.ids, got, tt.want)
			}
		})
	}
}

func TestRSSSiteIDs64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ids  []int
		want []int64
	}{
		{name: "empty", want: []int64{}},
		{name: "preserves order", ids: []int{3, 1, 2}, want: []int64{3, 1, 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := rssSiteIDs64(tt.ids); !slices.Equal(got, tt.want) {
				t.Fatalf("rssSiteIDs64(%v) = %v, want %v", tt.ids, got, tt.want)
			}
		})
	}
}

func TestRSSQueryAllowsSite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		query  subscription.Query
		siteID int
		want   bool
	}{
		{name: "nil whitelist allows all", siteID: 99, want: true},
		{name: "empty whitelist allows all", query: subscription.Query{SiteIDs: []int{}}, siteID: 99, want: true},
		{name: "listed site is allowed", query: subscription.Query{SiteIDs: []int{3, 1}}, siteID: 1, want: true},
		{name: "unlisted site is rejected", query: subscription.Query{SiteIDs: []int{3, 1}}, siteID: 2, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := rssQueryAllowsSite(tt.query, tt.siteID); got != tt.want {
				t.Fatalf("rssQueryAllowsSite(%v, %d) = %t, want %t", tt.query.SiteIDs, tt.siteID, got, tt.want)
			}
		})
	}
}

type rssSourceTestAdapter struct {
	name string
}

func (a rssSourceTestAdapter) Name() string { return a.name }
func (rssSourceTestAdapter) Search(context.Context, indexer.Query) ([]indexer.Resource, error) {
	return nil, nil
}

type rssSourceTestSiteAdapter struct {
	rssSourceTestAdapter
	siteID int
}

func (a rssSourceTestSiteAdapter) SiteID() int { return a.siteID }

type rssSourceTestNoFetcherAdapter struct {
	rssSourceTestSiteAdapter
	kind    string
	version int16
}

func (rssSourceTestNoFetcherAdapter) ResolveDownload(context.Context, string) (indexer.ResolvedDownload, error) {
	return indexer.ResolvedDownload{}, nil
}
func (a rssSourceTestNoFetcherAdapter) ResolverKind() string   { return a.kind }
func (a rssSourceTestNoFetcherAdapter) ResolverVersion() int16 { return a.version }

type rssSourceTestNoResolverAdapter struct {
	rssSourceTestSiteAdapter
	key string
}

func (rssSourceTestNoResolverAdapter) FetchRSS(context.Context, indexer.RSSRequest) (indexer.RSSResponse, error) {
	return indexer.RSSResponse{}, nil
}
func (a rssSourceTestNoResolverAdapter) RSSCursorKey() string { return a.key }

type rssSourceTestFullAdapter struct {
	rssSourceTestNoResolverAdapter
	kind    string
	version int16
	noCache bool
}

func (rssSourceTestFullAdapter) ResolveDownload(context.Context, string) (indexer.ResolvedDownload, error) {
	return indexer.ResolvedDownload{}, nil
}
func (a rssSourceTestFullAdapter) ResolverKind() string   { return a.kind }
func (a rssSourceTestFullAdapter) ResolverVersion() int16 { return a.version }
func (a rssSourceTestFullAdapter) RSSNoCache() bool       { return a.noCache }

func newRSSSourceTestAdapter(name string, siteID int, key, kind string, version int16) rssSourceTestFullAdapter {
	return rssSourceTestFullAdapter{
		rssSourceTestNoResolverAdapter: rssSourceTestNoResolverAdapter{
			rssSourceTestSiteAdapter: rssSourceTestSiteAdapter{
				rssSourceTestAdapter: rssSourceTestAdapter{name: name},
				siteID:               siteID,
			},
			key: key,
		},
		kind:    kind,
		version: version,
	}
}
