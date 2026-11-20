package subscriptionflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/media"
	"github.com/moviepilot-go/moviepilot-go/internal/metadata"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestFilterV2SubscriptionCandidatesUsesV2OwnAndDefaultGroupSelection(t *testing.T) {
	baseGroups := []subscription.V2FilterRuleGroup{{Name: "default", RuleString: "4K"}}
	tests := []struct {
		name          string
		config        map[string]any
		settings      map[string]any
		wantPriority  int
		wantCalls     []string
		wantCandidate bool
	}{
		{
			name:   "empty own list falls back to subscribe groups",
			config: map[string]any{"filter_groups": []any{}},
			settings: map[string]any{
				v2SubscribeFilterRuleGroupsSetting: []string{"default"},
				v2UserFilterRuleGroupsSetting:      baseGroups,
				v2CustomFilterRulesSetting:         []subscription.V2CustomFilterRule{},
			},
			wantPriority: 100, wantCandidate: true,
			wantCalls: []string{
				v2SubscribeFilterRuleGroupsSetting,
				v2UserFilterRuleGroupsSetting,
				v2CustomFilterRulesSetting,
			},
		},
		{
			name:   "best version uses its own default setting",
			config: map[string]any{"best_version": float64(1)},
			settings: map[string]any{
				v2BestVersionFilterRuleGroupsSetting: []string{"default"},
				v2UserFilterRuleGroupsSetting:        baseGroups,
				v2CustomFilterRulesSetting:           []subscription.V2CustomFilterRule{},
			},
			wantPriority: 100, wantCandidate: true,
			wantCalls: []string{
				v2BestVersionFilterRuleGroupsSetting,
				v2UserFilterRuleGroupsSetting,
				v2CustomFilterRulesSetting,
			},
		},
		{
			name:   "truthy empty name blocks global fallback",
			config: map[string]any{"filter_groups": []any{""}},
			settings: map[string]any{
				v2SubscribeFilterRuleGroupsSetting: []string{"default"},
				v2UserFilterRuleGroupsSetting:      baseGroups,
				v2CustomFilterRulesSetting:         []subscription.V2CustomFilterRule{},
			},
			wantCandidate: true,
			wantCalls:     []string{v2UserFilterRuleGroupsSetting, v2CustomFilterRulesSetting},
		},
		{
			name:   "group names are not trimmed",
			config: map[string]any{"filter_groups": []any{" default "}},
			settings: map[string]any{
				v2UserFilterRuleGroupsSetting: baseGroups,
				v2CustomFilterRulesSetting:    []subscription.V2CustomFilterRule{},
			},
			wantCandidate: true,
			wantCalls:     []string{v2UserFilterRuleGroupsSetting, v2CustomFilterRulesSetting},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &v2FilterSettingsStore{
				subscriptionFlowStore: &subscriptionFlowStore{},
				settings:              tt.settings,
			}
			handler := &Handler{store: store}
			candidate := v2FlowFilterCandidate("Example 2160p")
			candidate.Resource.PriOrder = 7
			got, err := handler.filterV2SubscriptionCandidates(
				t.Context(),
				persistedSubscription{MediaType: string(media.TypeMovie), Config: tt.config},
				[]subscription.Candidate{candidate},
			)
			if err != nil {
				t.Fatalf("filterV2SubscriptionCandidates() error = %v", err)
			}
			if (len(got) == 1) != tt.wantCandidate {
				t.Fatalf("candidate present = %t, want %t", len(got) == 1, tt.wantCandidate)
			}
			if len(got) == 1 {
				wantPriority := tt.wantPriority
				if wantPriority == 0 {
					wantPriority = 7
				}
				if got[0].Resource.PriOrder != wantPriority {
					t.Fatalf("priority = %d, want %d", got[0].Resource.PriOrder, wantPriority)
				}
			}
			if !reflect.DeepEqual(store.calls, tt.wantCalls) {
				t.Fatalf("setting calls = %v, want %v", store.calls, tt.wantCalls)
			}
		})
	}
}

func TestFilterV2SubscriptionCandidatesUsesTMDBOriginalLanguage(t *testing.T) {
	store := &v2FilterSettingsStore{
		subscriptionFlowStore: &subscriptionFlowStore{},
		settings: map[string]any{
			v2UserFilterRuleGroupsSetting: []subscription.V2FilterRuleGroup{{Name: "language", RuleString: "CNVOI"}},
			v2CustomFilterRulesSetting:    []subscription.V2CustomFilterRule{},
		},
	}
	resolver := &subscriptionFlowMediaDetails{item: metadata.Media{OriginalLanguage: "zh"}}
	handler := &Handler{store: store, mediaDetails: resolver}
	target := persistedSubscription{
		MediaType: string(media.TypeMovie), MediaSource: "themoviedb", MediaID: "42",
		Config: map[string]any{"filter_groups": []any{"language"}},
	}

	got, err := handler.filterV2SubscriptionCandidates(
		t.Context(), target, []subscription.Candidate{v2FlowFilterCandidate("Example")},
	)
	if err != nil {
		t.Fatalf("filterV2SubscriptionCandidates() error = %v", err)
	}
	if len(got) != 1 || got[0].Resource.PriOrder != 100 {
		t.Fatalf("result = %#v, want language bypass priority 100", got)
	}
	if resolver.calls != 1 || resolver.source != "themoviedb" || resolver.id != "movie:42" {
		t.Fatalf("detail call = (%d, %q, %q)", resolver.calls, resolver.source, resolver.id)
	}
}

func TestFilterV2SubscriptionCandidatesUsesV2DerivedCategory(t *testing.T) {
	t.Parallel()
	store := &v2FilterSettingsStore{
		subscriptionFlowStore: &subscriptionFlowStore{},
		settings: map[string]any{
			v2UserFilterRuleGroupsSetting: []subscription.V2FilterRuleGroup{{
				Name: "anime", RuleString: "4K", MediaType: string(media.TypeTV), Category: "国漫",
			}},
			v2CustomFilterRulesSetting: []subscription.V2CustomFilterRule{},
		},
	}
	resolver := &subscriptionFlowMediaDetails{item: metadata.Media{
		GenreIDs: []int{16}, OriginCountries: []string{"CN"},
	}}
	handler := &Handler{store: store, mediaDetails: resolver}
	target := persistedSubscription{
		MediaType: string(media.TypeTV), MediaSource: "themoviedb", MediaID: "42",
		Config: map[string]any{"filter_groups": []any{"anime"}, "media_category": "用户覆盖不参与过滤"},
	}

	got, err := handler.filterV2SubscriptionCandidates(
		t.Context(), target, []subscription.Candidate{v2FlowFilterCandidate("Example S01 2160p")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Resource.PriOrder != 100 {
		t.Fatalf("category-filtered result = %#v, want derived 国漫 match", got)
	}
}

func TestFilterV2SubscriptionCandidatesUsesConfiguredCategoryResolver(t *testing.T) {
	t.Parallel()
	store := &v2FilterSettingsStore{
		subscriptionFlowStore: &subscriptionFlowStore{},
		settings: map[string]any{
			v2UserFilterRuleGroupsSetting: []subscription.V2FilterRuleGroup{{
				Name: "custom", RuleString: "4K", MediaType: string(media.TypeMovie), Category: "自定义分类",
			}},
			v2CustomFilterRulesSetting: []subscription.V2CustomFilterRule{},
		},
	}
	resolver := &subscriptionFlowMediaDetails{item: metadata.Media{OriginalLanguage: "en"}}
	handler := &Handler{store: store, mediaDetails: resolver, categoryResolver: fixedCategoryResolver("自定义分类")}
	target := persistedSubscription{
		MediaType: string(media.TypeMovie), MediaSource: "themoviedb", MediaID: "42",
		Config: map[string]any{"filter_groups": []any{"custom"}},
	}
	got, err := handler.filterV2SubscriptionCandidates(
		t.Context(), target, []subscription.Candidate{v2FlowFilterCandidate("Example 2160p")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Resource.PriOrder != 100 {
		t.Fatalf("custom category result = %#v", got)
	}
}

type fixedCategoryResolver string

func (r fixedCategoryResolver) Category(string, metadata.Media) string { return string(r) }

func TestFilterV2SubscriptionCandidatesRejectsInvalidOwnGroupsAndPropagatesContext(t *testing.T) {
	handler := &Handler{store: &v2FilterSettingsStore{subscriptionFlowStore: &subscriptionFlowStore{}}}
	_, err := handler.filterV2SubscriptionCandidates(
		t.Context(),
		persistedSubscription{Config: map[string]any{"filter_groups": []any{1}}},
		[]subscription.Candidate{v2FlowFilterCandidate("Example")},
	)
	if !errors.Is(err, subscription.ErrInvalidV2FilterRules) {
		t.Fatalf("invalid groups error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &v2FilterSettingsStore{
		subscriptionFlowStore: &subscriptionFlowStore{},
		settings: map[string]any{
			v2UserFilterRuleGroupsSetting: []subscription.V2FilterRuleGroup{{Name: "g", RuleString: "4K"}},
			v2CustomFilterRulesSetting:    []subscription.V2CustomFilterRule{},
		},
	}
	resolver := &subscriptionFlowMediaDetails{err: context.Canceled}
	handler = &Handler{store: store, mediaDetails: resolver}
	_, err = handler.filterV2SubscriptionCandidates(
		ctx,
		persistedSubscription{
			MediaType: string(media.TypeMovie), MediaSource: "themoviedb", MediaID: "42",
			Config: map[string]any{"filter_groups": []any{"g"}},
		},
		[]subscription.Candidate{v2FlowFilterCandidate("Example 2160p")},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled detail error = %v, want context.Canceled", err)
	}
}

func TestWithV2DefaultCandidateFiltersMatchesSubscribeDefaults(t *testing.T) {
	t.Parallel()
	store := &v2FilterSettingsStore{
		subscriptionFlowStore: &subscriptionFlowStore{},
		settings: map[string]any{
			v2SubscribeDefaultParamsSetting: map[string]any{
				"include": "DEFAULT-INCLUDE", "exclude": "DEFAULT-EXCLUDE",
				"quality": "DEFAULT-QUALITY", "resolution": "DEFAULT-RESOLUTION",
				"effect": "DEFAULT-EFFECT",
			},
		},
	}
	handler := &Handler{store: store}
	tests := []struct {
		name   string
		config map[string]any
		want   subscription.Query
	}{
		{
			name: "empty subscription values use system defaults",
			config: map[string]any{
				"include": "", "exclude": nil, "quality": " ", "resolution": "", "effect": "",
			},
			want: subscription.Query{
				Include: []string{"DEFAULT-INCLUDE"}, Exclude: []string{"DEFAULT-EXCLUDE"},
				Quality: "DEFAULT-QUALITY", Resolution: "DEFAULT-RESOLUTION", Effect: "DEFAULT-EFFECT",
			},
		},
		{
			name: "truthy subscription values override system defaults",
			config: map[string]any{
				"include": "OWN-INCLUDE", "exclude": "OWN-EXCLUDE", "quality": "OWN-QUALITY",
				"resolution": "OWN-RESOLUTION", "effect": "OWN-EFFECT",
			},
			want: subscription.Query{
				Include: []string{"OWN-INCLUDE"}, Exclude: []string{"OWN-EXCLUDE"},
				Quality: "OWN-QUALITY", Resolution: "OWN-RESOLUTION", Effect: "OWN-EFFECT",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := handler.withV2DefaultCandidateFilters(t.Context(), tt.config, subscription.Query{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("query = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHandlerAppliesV2FilterPriorityBeforeCandidateSort(t *testing.T) {
	target := subscriptionFlowTarget()
	target.MediaSource = "douban"
	target.MediaID = "7"
	target.Config["resolution"] = ""
	target.Config["filter_groups"] = []any{}
	baseStore := &subscriptionFlowStore{
		list: func(context.Context, time.Time, time.Time, int64, int) ([]database.Subscription, error) {
			return []database.Subscription{target}, nil
		},
		downloaders: []downloadconfig.Downloader{{
			ID: subscriptionFlowDownloaderID, Name: "Primary", Type: "qbittorrent", Enabled: true, Default: true,
		}},
	}
	store := &v2FilterSettingsStore{
		subscriptionFlowStore: baseStore,
		settings: map[string]any{
			v2SubscribeFilterRuleGroupsSetting: []string{"quality"},
			v2UserFilterRuleGroupsSetting: []subscription.V2FilterRuleGroup{{
				Name: "quality", RuleString: "4K > 1080P",
				MediaType: string(media.TypeMovie), Category: "动画电影",
			}},
			v2CustomFilterRulesSetting: []subscription.V2CustomFilterRule{},
		},
	}
	searcher := &subscriptionFlowSearcher{search: func(
		_ context.Context,
		_ []indexer.Adapter,
		_ indexer.Query,
		onEvent func(indexer.Event),
	) ([]indexer.Resource, error) {
		resources := []indexer.Resource{
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "1080", Seeders: 100},
			{SiteID: 7, Site: "fixture", Title: "Example.Movie.2026.2160p.WEB-DL", DownloadID: "4k", Seeders: 1},
		}
		onEvent(indexer.Event{Site: "fixture", Resources: resources})
		return resources, nil
	}}
	submission := &subscriptionFlowSubmission{}
	handler := mustSubscriptionFlowHandler(
		t,
		store,
		searcher,
		subscriptionFlowAdapters{subscriptionFlowAdapter{id: 7, name: "fixture", kind: "fixture", version: 1}},
		submission,
	)
	handler.mediaDetails = &subscriptionFlowMediaDetails{item: metadata.Media{
		Source: "douban", ID: "7", Type: "movie", Title: "Example Movie", Year: 2026,
	}}
	handler.mediaSupplementer = &subscriptionFlowMediaSupplementer{item: metadata.Media{
		Source: "douban", ID: "7", TMDBID: "42", Type: "movie", Title: "Example Movie", Year: 2026,
		GenreIDs: []int{16},
	}, found: true}

	if err := handler.HandleSubscriptionSearch(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 1 || submission.plan.ResourceSecret != "4k" {
		t.Fatalf("submission = calls:%d plan:%+v, want V2 priority to select 4k", submission.calls, submission.plan)
	}
}

func TestRSSHandlerAppliesSameV2FilterPriorityBeforeCandidateSort(t *testing.T) {
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.MediaSource = "douban"
	target.MediaID = "7"
	target.Config["resolution"] = ""
	target.Config["filter_groups"] = []any{}
	baseStore := newRSSFlowStore(target)
	store := &v2FilterRSSSettingsStore{
		rssFlowStore: baseStore,
		settings: map[string]any{
			v2SubscribeFilterRuleGroupsSetting: []string{"quality"},
			v2UserFilterRuleGroupsSetting: []subscription.V2FilterRuleGroup{{
				Name: "quality", RuleString: "4K > 1080P",
				MediaType: string(media.TypeMovie), Category: "动画电影",
			}},
			v2CustomFilterRulesSetting: []subscription.V2CustomFilterRule{},
		},
	}
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{id: 7, name: "rss", kind: "declarative-rss", version: 1},
		key:                     "sha256:v2-filter",
	}
	submission := &subscriptionFlowSubmission{}
	baseHandler := mustSubscriptionFlowHandler(
		t,
		store,
		&subscriptionFlowSearcher{},
		subscriptionFlowAdapters{adapter},
		submission,
	)
	baseHandler.mediaDetails = &subscriptionFlowMediaDetails{item: metadata.Media{
		Source: "douban", ID: "7", Type: "movie", Title: "Example Movie", Year: 2026,
	}}
	baseHandler.mediaSupplementer = &subscriptionFlowMediaSupplementer{item: metadata.Media{
		Source: "douban", ID: "7", TMDBID: "42", Type: "movie", Title: "Example Movie", Year: 2026,
		GenreIDs: []int{16},
	}, found: true}
	handler, err := NewRSSHandler(
		store,
		baseHandler,
		subscriptionFlowAdapters{adapter},
		&rssFlowEnqueuer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	resources := map[int][]indexer.Resource{7: {
		{SiteID: 7, Site: "rss", Title: "Example.Movie.2026.1080p.WEB-DL", DownloadID: "1080", Seeders: 100},
		{SiteID: 7, Site: "rss", Title: "Example.Movie.2026.2160p.WEB-DL", DownloadID: "4k", Seeders: 1},
	}}

	matched, err := handler.matchRSSTarget(
		t.Context(), rssSources([]indexer.Adapter{adapter}), resources, target,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || submission.calls != 1 || submission.plan.ResourceSecret != "4k" {
		t.Fatalf(
			"result = matched:%t calls:%d plan:%+v, want RSS V2 priority to select 4k",
			matched, submission.calls, submission.plan,
		)
	}
}

type v2FilterSettingsStore struct {
	*subscriptionFlowStore
	settings map[string]any
	calls    []string
	err      error
}

type v2FilterRSSSettingsStore struct {
	*rssFlowStore
	settings map[string]any
}

func (s *v2FilterRSSSettingsStore) GetSetting(ctx context.Context, key string, target any) (bool, error) {
	value, found := s.settings[key]
	if !found {
		return s.rssFlowStore.GetSetting(ctx, key, target)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return false, err
	}
	return true, nil
}

func (s *v2FilterSettingsStore) GetSetting(_ context.Context, key string, target any) (bool, error) {
	s.calls = append(s.calls, key)
	if s.err != nil {
		return false, s.err
	}
	value, found := s.settings[key]
	if !found {
		return false, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return false, err
	}
	return true, nil
}

func v2FlowFilterCandidate(title string) subscription.Candidate {
	return subscription.Candidate{
		Key: "candidate",
		Resource: indexer.Resource{
			SiteID: 1, Site: "Fixture", Title: title, DownloadID: "candidate",
			PublishedAt: time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
		},
		Parsed: media.Parse(title, media.Options{}),
	}
}
