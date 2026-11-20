package subscriptionflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/indexer"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestRSSHandlerPollOnlyPersistsCache(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.V2State = "R"
	store := newRSSFlowStore(target)
	store.cursor.NextPollAt = cutoff.Add(-time.Second)
	adapter := rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{
			id: 7, name: "rss", kind: "declarative-rss", version: 1,
		},
		key: "sha256:poll-only",
	}
	submission := &subscriptionFlowSubmission{}
	base := mustSubscriptionFlowHandler(
		t, store, &subscriptionFlowSearcher{}, subscriptionFlowAdapters{adapter}, submission,
	)
	handler, err := NewRSSHandler(store, base, subscriptionFlowAdapters{adapter}, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return cutoff }

	if err := handler.HandleRSSPoll(t.Context(), jobs.RSSPollArgs{SiteID: 7, Cutoff: cutoff}); err != nil {
		t.Fatal(err)
	}
	if submission.calls != 0 {
		t.Fatalf("HandleRSSPoll() submissions = %d, want 0", submission.calls)
	}
	if len(store.committed.Items) != 1 {
		t.Fatalf("HandleRSSPoll() committed items = %d, want 1", len(store.committed.Items))
	}
	if store.completedID != 0 || store.completedState != "" {
		t.Fatalf(
			"HandleRSSPoll() completed item = (%d, %q), want no matching side effect",
			store.completedID, store.completedState,
		)
	}
}

func TestRSSHandlerMatchWaitsForPollRoundBarrier(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 8, 15, 0, 0, time.UTC)
	target := rssActiveMovieTarget(cutoff)
	store := newRSSMatchFlowStore(target)
	store.ready = false
	store.snapshots[7] = []database.RSSItem{
		rssCachedItem(7, 71, "pending-barrier", "Example.Movie.2026.1080p.WEB-DL", 10, cutoff.Add(-time.Minute), "pending"),
	}
	adapter := rssMatchAdapter(7, "barrier")
	submission := &subscriptionFlowSubmission{}
	handler := mustRSSMatchHandler(t, store, []rssFlowAdapter{adapter}, submission)

	err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7},
	})
	if !errors.Is(err, jobs.ErrRSSMatchBarrierPending) {
		t.Fatalf("HandleRSSMatch() error = %v, want ErrRSSMatchBarrierPending", err)
	}
	if submission.calls != 0 || len(store.snapshotCalls) != 0 || store.listCalls != 0 {
		t.Fatalf(
			"pending barrier side effects = submissions:%d snapshots:%v subscription_lists:%d",
			submission.calls, store.snapshotCalls, store.listCalls,
		)
	}
	if !store.barrierCutoff.Equal(cutoff) || !reflect.DeepEqual(store.barrierSiteIDs, []int64{7}) {
		t.Fatalf("barrier query = cutoff:%s sites:%v", store.barrierCutoff, store.barrierSiteIDs)
	}
}

func TestRSSHandlerMatchUsesV2PriorityRegardlessOfPollCompletionOrder(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 8, 30, 0, 0, time.UTC)
	tests := []struct {
		name         string
		site7SeenAt  time.Time
		site8SeenAt  time.Time
		wantResource string
	}{
		{
			name:         "lower quality site finishes last",
			site7SeenAt:  cutoff.Add(-time.Minute),
			site8SeenAt:  cutoff.Add(-2 * time.Minute),
			wantResource: "ordinary-1080p",
		},
		{
			name:         "higher quality site finishes last",
			site7SeenAt:  cutoff.Add(-2 * time.Minute),
			site8SeenAt:  cutoff.Add(-time.Minute),
			wantResource: "ordinary-1080p",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := rssActiveMovieTarget(cutoff)
			delete(target.Config, "resolution")
			target.Config["sites"] = []any{}
			store := newRSSMatchFlowStore(target)
			store.ready = true
			store.snapshots[7] = []database.RSSItem{
				rssCachedItem(
					7, 71, "ordinary-1080p", "Example.Movie.2026.1080p.WEB-DL",
					100, tt.site7SeenAt, "pending",
				),
			}
			store.snapshots[8] = []database.RSSItem{
				rssCachedItem(
					8, 81, tt.wantResource, "Example.Movie.2026.2160p.BluRay",
					1, tt.site8SeenAt, "pending",
				),
			}
			submission := &subscriptionFlowSubmission{}
			handler := mustRSSMatchHandler(
				t, store, []rssFlowAdapter{rssMatchAdapter(7, "first"), rssMatchAdapter(8, "second")}, submission,
			)

			err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
				Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7, 8},
			})
			if err != nil {
				t.Fatal(err)
			}
			if submission.calls != 1 || submittedRSSLocator(submission.plan) != tt.wantResource {
				t.Fatalf(
					"global match submission = calls:%d resource:%q, want one %q",
					submission.calls, submittedRSSLocator(submission.plan), tt.wantResource,
				)
			}
			if !reflect.DeepEqual(store.snapshotCalls, []int64{7, 8}) {
				t.Fatalf("cache snapshot calls = %v, want [7 8]", store.snapshotCalls)
			}
		})
	}
}

func TestRSSHandlerMatchUsesPersistedV2SiteOrder(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 8, 35, 0, 0, time.UTC)
	target := rssActiveMovieTarget(cutoff)
	target.Config["sites"] = []any{}
	store := newRSSMatchFlowStore(target)
	store.ready = true
	store.sortRules = []string{"site"}
	store.siteOrders = map[int]int{7: 9, 8: 1}
	store.snapshots[7] = []database.RSSItem{
		rssCachedItem(7, 71, "low-site-priority", "Example.Movie.2026.1080p.WEB-DL", 100, cutoff.Add(-time.Minute), "pending"),
	}
	store.snapshots[8] = []database.RSSItem{
		rssCachedItem(8, 81, "high-site-priority", "Example.Movie.2026.1080p.WEB-DL", 1, cutoff.Add(-2*time.Minute), "pending"),
	}
	submission := &subscriptionFlowSubmission{}
	handler := mustRSSMatchHandler(
		t,
		store,
		[]rssFlowAdapter{rssMatchAdapter(7, "lower-priority"), rssMatchAdapter(8, "higher-priority")},
		submission,
	)

	if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7, 8},
	}); err != nil {
		t.Fatal(err)
	}
	if store.sortCalls != 1 {
		t.Fatalf("LoadV2CandidateSortInputs() calls = %d, want 1", store.sortCalls)
	}
	if submission.calls != 1 || submission.plan.SiteID != 8 ||
		submittedRSSLocator(submission.plan) != "high-site-priority" {
		t.Fatalf(
			"submission = calls:%d site:%d resource:%q, want site 8 resource high-site-priority",
			submission.calls, submission.plan.SiteID, submittedRSSLocator(submission.plan),
		)
	}
}

func TestRSSHandlerMatchMapsV2CandidateSortInputFailureToStorage(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 8, 37, 0, 0, time.UTC)
	target := rssActiveMovieTarget(cutoff)
	store := newRSSMatchFlowStore(target)
	store.ready = true
	store.sortErr = errors.New("sort inputs unavailable")
	store.snapshots[7] = []database.RSSItem{
		rssCachedItem(7, 71, "candidate", "Example.Movie.2026.1080p.WEB-DL", 1, cutoff.Add(-time.Minute), "pending"),
	}
	submission := &subscriptionFlowSubmission{}
	handler := mustRSSMatchHandler(t, store, []rssFlowAdapter{rssMatchAdapter(7, "failure")}, submission)

	err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7},
	})
	if !errors.Is(err, ErrRSSStorage) {
		t.Fatalf("HandleRSSMatch() error = %v, want ErrRSSStorage", err)
	}
	if submission.calls != 0 {
		t.Fatalf("submissions = %d, want 0", submission.calls)
	}
}

func TestRSSHandlerMatchLoadsCacheOncePerSubscriptionPageAndSite(t *testing.T) {
	t.Parallel()
	const targetCount = rssTargetPageSize + 1
	cutoff := time.Date(2026, 8, 16, 8, 40, 0, 0, time.UTC)
	targets := make([]database.Subscription, 0, targetCount)
	wantIDs := make([]int64, 0, targetCount)
	for id := int64(1); id <= targetCount; id++ {
		target := rssActiveMovieTarget(cutoff)
		target.ID = id
		target.Username = "owner"
		delete(target.Config, "resolution")
		target.Config["sites"] = []any{}
		targets = append(targets, target)
		wantIDs = append(wantIDs, id)
	}

	store := newRSSMatchFlowStore(targets[0])
	store.ready = true
	store.subscriptionFlowStore.list = func(
		_ context.Context,
		_ time.Time,
		_ time.Time,
		afterID int64,
		limit int,
	) ([]database.Subscription, error) {
		store.listCalls++
		if limit != rssTargetPageSize {
			t.Fatalf("ListSubscriptionsForRSS() limit = %d, want %d", limit, rssTargetPageSize)
		}
		if afterID >= int64(len(targets)) {
			return nil, nil
		}
		lastID := min(afterID+int64(limit), int64(len(targets)))
		return append([]database.Subscription(nil), targets[afterID:lastID]...), nil
	}
	store.snapshots[7] = []database.RSSItem{
		rssCachedItem(
			7, 71, "page-ordinary-1080p", "Example.Movie.2026.1080p.WEB-DL",
			100, cutoff.Add(-time.Minute), "pending",
		),
	}
	store.snapshots[8] = []database.RSSItem{
		rssCachedItem(
			8, 81, "page-best-2160p", "Example.Movie.2026.2160p.BluRay",
			1, cutoff.Add(-2*time.Minute), "pending",
		),
	}
	submission := &subscriptionFlowSubmission{}
	handler := mustRSSMatchHandler(
		t,
		store,
		[]rssFlowAdapter{rssMatchAdapter(7, "page-first"), rssMatchAdapter(8, "page-second")},
		submission,
	)

	if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
		Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7, 8},
	}); err != nil {
		t.Fatal(err)
	}
	if store.listCalls != 2 {
		t.Fatalf("subscription page calls = %d, want 2", store.listCalls)
	}
	if submission.calls != targetCount || !reflect.DeepEqual(submission.rssIDs, wantIDs) {
		t.Fatalf(
			"full subscription scan = calls:%d ids:%v, want calls:%d ids:%v",
			submission.calls, submission.rssIDs, targetCount, wantIDs,
		)
	}
	if submission.plan.SiteID != 7 || submittedRSSLocator(submission.plan) != "page-ordinary-1080p" {
		t.Fatalf(
			"cross-site selection = site:%d resource:%q, want site 7 resource %q",
			submission.plan.SiteID, submittedRSSLocator(submission.plan), "page-ordinary-1080p",
		)
	}
	if want := []int64{7, 8, 7, 8}; !reflect.DeepEqual(store.snapshotCalls, want) {
		gotPrefix := store.snapshotCalls
		if len(gotPrefix) > len(want)+2 {
			gotPrefix = gotPrefix[:len(want)+2]
		}
		t.Fatalf(
			"cache snapshot calls = %d (prefix %v), want %d calls %v (subscription pages x sites)",
			len(store.snapshotCalls), gotPrefix, len(want), want,
		)
	}
}

func TestRSSHandlerMatchIncludesV2CachedTerminalItems(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 8, 45, 0, 0, time.UTC)
	tests := []struct {
		name  string
		state string
	}{
		{name: "matched item", state: "matched"},
		{name: "ignored item", state: "ignored"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := rssActiveMovieTarget(cutoff)
			store := newRSSMatchFlowStore(target)
			store.ready = true
			store.snapshots[7] = []database.RSSItem{
				rssCachedItem(
					7, 71, "old-"+tt.state, "Example.Movie.2026.1080p.WEB-DL",
					10, cutoff.Add(-time.Hour), tt.state,
				),
			}
			submission := &subscriptionFlowSubmission{}
			handler := mustRSSMatchHandler(t, store, []rssFlowAdapter{rssMatchAdapter(7, tt.state)}, submission)

			if err := handler.HandleRSSMatch(t.Context(), jobs.RSSMatchArgs{
				Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7},
			}); err != nil {
				t.Fatal(err)
			}
			if submission.calls != 1 || submittedRSSLocator(submission.plan) != "old-"+tt.state {
				t.Fatalf(
					"terminal cache match = calls:%d resource:%q",
					submission.calls, submittedRSSLocator(submission.plan),
				)
			}
		})
	}
}

func TestRSSHandlerMatchRetryExcludesSuccessfulOwner(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	target := rssActiveMovieTarget(cutoff)
	store := newRSSMatchFlowStore(target)
	store.ready = true
	store.snapshots[7] = []database.RSSItem{
		rssCachedItem(7, 71, "retry-owner", "Example.Movie.2026.1080p.WEB-DL", 10, cutoff.Add(-time.Minute), "pending"),
	}
	submission := &subscriptionFlowSubmission{}
	store.subscriptionFlowStore.list = func(
		context.Context, time.Time, time.Time, int64, int,
	) ([]database.Subscription, error) {
		if submission.calls > 0 {
			return []database.Subscription{}, nil
		}
		return []database.Subscription{target}, nil
	}
	handler := mustRSSMatchHandler(t, store, []rssFlowAdapter{rssMatchAdapter(7, "retry")}, submission)
	args := jobs.RSSMatchArgs{Key: "global", Cutoff: cutoff, ExpectedSiteIDs: []int{7}}

	if err := handler.HandleRSSMatch(t.Context(), args); err != nil {
		t.Fatalf("first HandleRSSMatch() error = %v", err)
	}
	if err := handler.HandleRSSMatch(t.Context(), args); err != nil {
		t.Fatalf("retry HandleRSSMatch() error = %v", err)
	}
	if submission.calls != 1 || submittedRSSLocator(submission.plan) != "retry-owner" {
		t.Fatalf(
			"retry submissions = calls:%d resource:%q, want one successful owner",
			submission.calls, submittedRSSLocator(submission.plan),
		)
	}
}

func rssActiveMovieTarget(cutoff time.Time) database.Subscription {
	target := subscriptionFlowTarget()
	target.InternalState = subscription.Active
	target.V2State = "R"
	target.UpdatedAt = cutoff.Add(-time.Minute)
	return target
}

func submittedRSSLocator(plan database.DownloadIntentPlan) string {
	if plan.ResourceSecret != "" {
		return plan.ResourceSecret
	}
	return plan.ResourceID
}

func rssMatchAdapter(siteID int, suffix string) rssFlowAdapter {
	return rssFlowAdapter{
		subscriptionFlowAdapter: subscriptionFlowAdapter{
			id: siteID, name: "rss-" + suffix, kind: "declarative-rss", version: 1,
		},
		key: "sha256:cursor-" + suffix,
	}
}

func mustRSSMatchHandler(
	t *testing.T,
	store *rssMatchFlowStore,
	adapters []rssFlowAdapter,
	submission *subscriptionFlowSubmission,
) *RSSHandler {
	t.Helper()
	provided := make(subscriptionFlowAdapters, 0, len(adapters))
	for _, adapter := range adapters {
		provided = append(provided, adapter)
	}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, provided, submission)
	handler, err := NewRSSHandler(store, base, provided, &rssFlowEnqueuer{})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func rssCachedItem(
	siteID int64,
	id int64,
	downloadID string,
	title string,
	seeders int,
	firstSeenAt time.Time,
	state string,
) database.RSSItem {
	resource, err := json.Marshal(indexer.Resource{
		SiteID: int(siteID), Title: title, DownloadID: downloadID,
		Seeders: seeders, PublishedAt: firstSeenAt,
	})
	if err != nil {
		panic(err)
	}
	publishedAt := firstSeenAt
	return database.RSSItem{
		ID: id, SiteID: siteID, ItemKey: "sha256:item-" + downloadID,
		DownloadID: downloadID, Title: title, PublishedAt: &publishedAt,
		Resource: resource, FirstSeenAt: firstSeenAt, LastSeenAt: firstSeenAt,
		State: state, UpdatedAt: firstSeenAt,
	}
}

type rssMatchFlowStore struct {
	*rssFlowStore
	ready          bool
	snapshots      map[int64][]database.RSSItem
	barrierCutoff  time.Time
	barrierSiteIDs []int64
	snapshotCalls  []int64
	trimCalls      []int64
	listCalls      int
}

func newRSSMatchFlowStore(target database.Subscription) *rssMatchFlowStore {
	base := newRSSFlowStore(target)
	originalList := base.subscriptionFlowStore.list
	store := &rssMatchFlowStore{
		rssFlowStore: base,
		snapshots:    make(map[int64][]database.RSSItem),
	}
	base.subscriptionFlowStore.list = func(
		ctx context.Context, cutoff, after time.Time, afterID int64, limit int,
	) ([]database.Subscription, error) {
		store.listCalls++
		return originalList(ctx, cutoff, after, afterID, limit)
	}
	return store
}

func (s *rssMatchFlowStore) RSSPollRoundReady(
	_ context.Context,
	cutoff time.Time,
	expectedSiteIDs []int64,
) (bool, error) {
	s.barrierCutoff = cutoff
	s.barrierSiteIDs = append([]int64(nil), expectedSiteIDs...)
	return s.ready, nil
}

func (s *rssMatchFlowStore) ListRSSCacheSnapshot(
	_ context.Context,
	siteID int64,
	_ time.Time,
) ([]database.RSSItem, error) {
	s.snapshotCalls = append(s.snapshotCalls, siteID)
	return append([]database.RSSItem(nil), s.snapshots[siteID]...), nil
}

func (s *rssMatchFlowStore) TrimRSSCacheItems(_ context.Context, siteID int64, _ int) (int, error) {
	s.trimCalls = append(s.trimCalls, siteID)
	return 0, nil
}

// These defaults keep older RSSHandler tests focused on their original
// contract while RSSStore grows the global matching snapshot methods.
func (s *rssFlowStore) RSSPollRoundReady(context.Context, time.Time, []int64) (bool, error) {
	return true, nil
}

func (s *rssFlowStore) ListRSSCacheSnapshot(
	_ context.Context,
	_ int64,
	_ time.Time,
) ([]database.RSSItem, error) {
	return append([]database.RSSItem(nil), s.items...), nil
}

func (s *rssFlowStore) ListRSSCacheSignatures(_ context.Context, _ int64) (map[string]struct{}, error) {
	signatures := make(map[string]struct{}, len(s.items))
	for _, item := range s.items {
		var resource struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(item.Resource, &resource); err != nil {
			continue
		}
		signatures[item.Title+resource.Description] = struct{}{}
	}
	return signatures, nil
}

func (s *rssFlowStore) TrimRSSCacheItems(context.Context, int64, int) (int, error) {
	s.trimCalls++
	deleted := 0
	if len(s.trimDeletes) > 0 {
		deleted = s.trimDeletes[0]
		s.trimDeletes = s.trimDeletes[1:]
	}
	err := s.cleanupErr
	if len(s.trimErrors) > 0 {
		err = s.trimErrors[0]
		s.trimErrors = s.trimErrors[1:]
	}
	return deleted, err
}
