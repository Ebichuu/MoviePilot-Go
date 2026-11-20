package subscriptionflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
)

func TestRSSHandlerSweepKeepsControllerActiveUntilPollBarrier(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 15, 0, 0, time.UTC)
	target := rssActiveMovieTarget(cutoff)
	base := newRSSMatchFlowStore(target)
	base.ready = false
	store := &rssControllerStore{
		rssMatchFlowStore: base,
		due: []database.RSSCursor{{
			SiteID: 7, Endpoint: "sha256:cursor-controller", NextPollAt: cutoff,
		}},
	}
	adapter := rssMatchAdapter(7, "controller")
	submission := &subscriptionFlowSubmission{}
	enqueuer := &rssFlowEnqueuer{}
	handler := mustRSSControllerHandler(t, store, []rssFlowAdapter{adapter}, submission, enqueuer)
	handler.now = func() time.Time { return cutoff }

	state, err := handler.HandleRSSSweep(t.Context(), cutoff, jobs.RSSSweepState{})
	if !errors.Is(err, jobs.ErrRSSSweepPending) {
		t.Fatalf("HandleRSSSweep() error = %v, want ErrRSSSweepPending", err)
	}
	if !reflect.DeepEqual(state, jobs.RSSSweepState{Initialized: true, SiteIDs: []int{7}}) {
		t.Fatalf("HandleRSSSweep() state = %+v, want initialized site 7", state)
	}
	if len(enqueuer.args) != 1 || enqueuer.args[0].SiteID != 7 || !enqueuer.args[0].Cutoff.Equal(cutoff) {
		t.Fatalf("poll jobs = %+v, want site 7 at fixed cutoff", enqueuer.args)
	}
	if enqueuer.matchCalls != 0 {
		t.Fatalf("legacy match jobs = %d, want 0", enqueuer.matchCalls)
	}
	if !reflect.DeepEqual(base.barrierSiteIDs, []int64{7}) || !base.barrierCutoff.Equal(cutoff) {
		t.Fatalf("controller barrier = cutoff:%s sites:%v", base.barrierCutoff, base.barrierSiteIDs)
	}
	if submission.calls != 0 || len(base.snapshotCalls) != 0 {
		t.Fatalf("pending controller side effects = submissions:%d snapshots:%v", submission.calls, base.snapshotCalls)
	}
}

func TestRSSHandlerSweepMatchesInsideControllerAfterBarrier(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 16, 30, 0, 0, time.UTC)
	target := rssActiveMovieTarget(cutoff)
	base := newRSSMatchFlowStore(target)
	base.ready = true
	base.snapshots[7] = []database.RSSItem{
		rssCachedItem(
			7, 71, "controller-match", "Example.Movie.2026.1080p.WEB-DL",
			10, cutoff.Add(-time.Minute), "pending",
		),
	}
	store := &rssControllerStore{rssMatchFlowStore: base}
	adapter := rssMatchAdapter(7, "controller-ready")
	submission := &subscriptionFlowSubmission{}
	enqueuer := &rssFlowEnqueuer{}
	handler := mustRSSControllerHandler(t, store, []rssFlowAdapter{adapter}, submission, enqueuer)
	handler.now = func() time.Time { return cutoff }

	state, err := handler.HandleRSSSweep(t.Context(), cutoff, jobs.RSSSweepState{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, jobs.RSSSweepState{Initialized: true, SiteIDs: []int{7}}) {
		t.Fatalf("HandleRSSSweep() state = %+v, want initialized site 7", state)
	}
	if len(enqueuer.args) != 0 || enqueuer.matchCalls != 0 {
		t.Fatalf("ready controller enqueued jobs = polls:%v match:%d", enqueuer.args, enqueuer.matchCalls)
	}
	if submission.calls != 1 || submittedRSSLocator(submission.plan) != "controller-match" {
		t.Fatalf(
			"controller submission = calls:%d resource:%q",
			submission.calls,
			submittedRSSLocator(submission.plan),
		)
	}
	if !reflect.DeepEqual(base.snapshotCalls, []int64{7}) || !reflect.DeepEqual(base.trimCalls, []int64{7}) {
		t.Fatalf("controller cache calls = snapshots:%v trims:%v", base.snapshotCalls, base.trimCalls)
	}
}

func TestV2RSSRefreshSiteIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot database.SubscriptionRSSSitesSnapshot
		defaults []int
		want     []int
		wantRun  bool
	}{
		{
			name:     "没有订阅不刷新",
			snapshot: database.SubscriptionRSSSitesSnapshot{SiteSelections: [][]int{}},
		},
		{
			name: "只有 N S 且无全局站点时刷新全部",
			snapshot: database.SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{},
			},
			want:    []int{},
			wantRun: true,
		},
		{
			name: "只有 N S 时回退全局站点",
			snapshot: database.SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{},
			},
			defaults: []int{2, 1, 2},
			want:     []int{1, 2},
			wantRun:  true,
		},
		{
			name: "全局和订阅均为空时刷新全部",
			snapshot: database.SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{}},
			},
			want:    []int{},
			wantRun: true,
		},
		{
			name: "全局为空时使用订阅站点并集",
			snapshot: database.SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{3}, {1, 3}},
			},
			want:    []int{1, 3},
			wantRun: true,
		},
		{
			name: "两者非空时使用交集",
			snapshot: database.SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{1, 3}},
			},
			defaults: []int{1, 2},
			want:     []int{1},
			wantRun:  true,
		},
		{
			name: "完全不相交时回退全局站点",
			snapshot: database.SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{3}},
			},
			defaults: []int{1, 2},
			want:     []int{1, 2},
			wantRun:  true,
		},
		{
			name: "全部站点订阅与指定订阅并存时只刷新指定站点",
			snapshot: database.SubscriptionRSSSitesSnapshot{
				HasSubscriptions: true,
				SiteSelections:   [][]int{{}, {3}},
			},
			want:    []int{3},
			wantRun: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, gotRun := v2RSSRefreshSiteIDs(tt.snapshot, tt.defaults)
			if gotRun != tt.wantRun || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("v2RSSRefreshSiteIDs() = (%v, %t), want (%v, %t)", got, gotRun, tt.want, tt.wantRun)
			}
		})
	}
}

func TestRSSHandlerSweepDoesNotRefreshWithoutSubscriptions(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 17, 0, 0, 0, time.UTC)
	base := newRSSMatchFlowStore(rssActiveMovieTarget(cutoff))
	base.rssSitesSnapshot = database.SubscriptionRSSSitesSnapshot{SiteSelections: [][]int{}}
	store := &rssControllerStore{
		rssMatchFlowStore: base,
		due:               []database.RSSCursor{{SiteID: 7, Endpoint: "sha256:cursor-no-subscription", NextPollAt: cutoff}},
	}
	enqueuer := &rssFlowEnqueuer{}
	handler := mustRSSControllerHandler(
		t,
		store,
		[]rssFlowAdapter{rssMatchAdapter(7, "no-subscription")},
		&subscriptionFlowSubmission{},
		enqueuer,
	)

	state, err := handler.HandleRSSSweep(t.Context(), cutoff, jobs.RSSSweepState{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, jobs.RSSSweepState{Initialized: true, SiteIDs: []int{}}) {
		t.Fatalf("state = %+v, want initialized empty state", state)
	}
	if len(enqueuer.args) != 0 || base.ensureEndpoint != "" || !base.recoverAt.IsZero() || len(base.barrierSiteIDs) != 0 {
		t.Fatalf(
			"no-subscription side effects = polls:%v ensure:%q recover:%s barrier:%v",
			enqueuer.args,
			base.ensureEndpoint,
			base.recoverAt,
			base.barrierSiteIDs,
		)
	}
}

func TestRSSHandlerSweepRestoresPersistedSitesAcrossSnooze(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, 8, 16, 17, 15, 0, 0, time.UTC)
	target := rssActiveMovieTarget(cutoff)
	base := newRSSMatchFlowStore(target)
	base.ready = false
	base.rssSitesFound = true
	base.rssSites = []int{7, 8}
	base.rssSitesSnapshot = database.SubscriptionRSSSitesSnapshot{
		HasSubscriptions: true,
		SiteSelections:   [][]int{{8, 9}},
	}
	base.snapshots[8] = []database.RSSItem{
		rssCachedItem(8, 81, "persisted-round", "Example.Movie.2026.1080p.WEB-DL", 10, cutoff.Add(-time.Minute), "pending"),
	}
	store := &rssControllerStore{
		rssMatchFlowStore: base,
		due:               []database.RSSCursor{{SiteID: 8, Endpoint: "sha256:cursor-eight", NextPollAt: cutoff}},
	}
	enqueuer := &rssFlowEnqueuer{}
	handler := mustRSSControllerHandler(
		t,
		store,
		[]rssFlowAdapter{
			rssMatchAdapter(7, "seven"),
			rssMatchAdapter(8, "eight"),
			rssMatchAdapter(9, "nine"),
		},
		&subscriptionFlowSubmission{},
		enqueuer,
	)

	state, err := handler.HandleRSSSweep(t.Context(), cutoff, jobs.RSSSweepState{})
	if !errors.Is(err, jobs.ErrRSSSweepPending) {
		t.Fatalf("first HandleRSSSweep() error = %v, want pending", err)
	}
	if !reflect.DeepEqual(state.SiteIDs, []int{8}) {
		t.Fatalf("first state sites = %v, want [8]", state.SiteIDs)
	}

	base.rssSites = []int{7}
	base.rssSitesSnapshot = database.SubscriptionRSSSitesSnapshot{
		HasSubscriptions: true,
		SiteSelections:   [][]int{{9}},
	}
	base.ready = true
	if _, err := handler.HandleRSSSweep(t.Context(), cutoff, state); err != nil {
		t.Fatal(err)
	}
	if base.rssSnapshotCalls != 1 || base.rssSettingCalls != 1 {
		t.Fatalf("round configuration reads = snapshot:%d setting:%d, want 1 each", base.rssSnapshotCalls, base.rssSettingCalls)
	}
	if !reflect.DeepEqual(base.barrierSiteIDs, []int64{8}) || !reflect.DeepEqual(base.snapshotCalls, []int64{8}) {
		t.Fatalf("restored round sites = barrier:%v snapshots:%v, want site 8", base.barrierSiteIDs, base.snapshotCalls)
	}
}

type rssControllerStore struct {
	*rssMatchFlowStore
	due []database.RSSCursor
}

func (s *rssControllerStore) ListDueRSSCursors(
	context.Context,
	time.Time,
	int,
) ([]database.RSSCursor, error) {
	return append([]database.RSSCursor(nil), s.due...), nil
}

func mustRSSControllerHandler(
	t *testing.T,
	store *rssControllerStore,
	adapters []rssFlowAdapter,
	submission *subscriptionFlowSubmission,
	enqueuer *rssFlowEnqueuer,
) *RSSHandler {
	t.Helper()
	provided := make(subscriptionFlowAdapters, 0, len(adapters))
	for _, adapter := range adapters {
		provided = append(provided, adapter)
	}
	base := mustSubscriptionFlowHandler(t, store, &subscriptionFlowSearcher{}, provided, submission)
	handler, err := NewRSSHandler(store, base, provided, enqueuer)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
