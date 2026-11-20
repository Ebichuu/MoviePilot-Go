package organizationflow

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

func TestHandlerPersistsDurableSideEffectsBeforeBestEffortEnqueue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 20, 0, 0, 0, time.UTC)
	batchID := uuid.New()
	intentID := uuid.New()
	token := uuid.New()
	mediaIndexRequestID := uuid.New()
	username := "durable-side-effect-owner"
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	targetPath := filepath.Join(targetRoot, "Movie.mkv")
	plan, err := organizer.BuildPlan(targetRoot, []organizer.Operation{{
		Source:         filepath.Join(sourceRoot, "Movie.mkv"),
		Target:         targetPath,
		Mode:           organizer.Copy,
		ExpectedSize:   101,
		ExpectedSHA256: handlerTestSHA256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	groupKey, err := organization.BuildGroupKey(organization.MediaContext{TitleYear: "Movie (2026)", MediaType: "电影"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []organization.SuccessSnapshot{{
		BatchID: batchID, IntentID: intentID, GroupKey: groupKey,
		Version: organization.CurrentSuccessVersion, Username: username, NeedNotify: true,
		Media: organization.MediaContext{TitleYear: "Movie (2026)", MediaType: "电影"},
		Files: []organization.FileFact{{Ordinal: 0, Role: "video", SizeBytes: 101, Season: 0}},
	}}
	completionPlan := mediaindex.CompletionPlan{
		RequiredServer: &mediaindex.ServerRef{
			ID: uuid.NewString(), Name: "home", Type: "emby",
			Fingerprint: handlerTestServerFingerprint,
		},
		Files: []mediaindex.TargetFile{{
			LocalPath: targetPath, RemotePath: "/media/Movie.mkv", PathStyle: "posix",
			CaseSensitive: true, ExpectedSize: 101, ExpectedSHA256: handlerTestSHA256,
			MediaType: "movie", ProviderID: "tmdb:42",
		}},
	}
	scrapeRequestID := uuid.NewSHA1(batchID, []byte("movie\x00"+targetPath))
	scrapePlan := scrapeflow.Plan{
		BatchID: batchID, IntentID: intentID, Username: username,
		Requests: []scrapeflow.RequestPlan{{
			ID: scrapeRequestID, Kind: scrapeflow.KindMovie,
			MediaPath: targetPath, TMDBID: 42,
		}},
	}
	if !scrapePlan.Valid() {
		t.Fatalf("invalid scrape fixture: %#v", scrapePlan)
	}
	notificationPlan, err := notificationflow.BuildPlan(
		batchID, username, len(plan.Operations),
		[]notification.Channel{notification.ChannelTelegram},
	)
	if err != nil {
		t.Fatal(err)
	}
	notificationRequestID := notificationPlan.Events[0].Requests[0].ID

	store := &durableCompletionStoreFake{
		executionStoreFake: &executionStoreFake{
			batch: database.OrganizationBatch{
				ID: batchID, IntentID: intentID,
				State:      database.OrganizationBatchOrganizing,
				LeaseToken: token, SourceRoot: sourceRoot, Plan: plan,
			},
			claimed:   true,
			filePages: [][]database.OrganizationFile{nil},
		},
		requestID:       mediaIndexRequestID,
		scrapeIDs:       []uuid.UUID{scrapeRequestID},
		notificationIDs: []uuid.UUID{notificationRequestID},
		snapshots:       snapshots,
	}
	scrapePlanner := &durableScrapePlannerFake{plan: scrapePlan}
	notificationPlanner := &durableNotificationPlannerFake{successPlan: notificationPlan}
	scrapeEnqueuer := &durableScrapeEnqueuerFake{err: errors.New("river scrape unavailable: token=secret")}
	notificationEnqueuer := &durableNotificationEnqueuerFake{err: errors.New("river notification unavailable: token=secret")}
	mediaIndexEnqueuer := &mediaIndexEnqueuerFake{err: errors.New("river media index unavailable")}
	legacyNFO := &legacyNFOScraperSpy{}
	legacyNotification := &legacyNotificationSenderSpy{}

	handler, err := NewHandler(
		store,
		&organizationEnqueuerFake{},
		&mediaIndexCompletionPlannerFake{plan: completionPlan},
		mediaIndexEnqueuer,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.now = func() time.Time { return now }
	handler.execute = func(context.Context, string, organizer.Plan) error { return nil }
	handler.SetNFOScraper(legacyNFO)
	handler.SetNotificationSender(legacyNotification)
	handler.SetAppDomain("https://moviepilot.example")
	handler.SetDurableSideEffects(
		store, scrapePlanner, notificationPlanner, scrapeEnqueuer, notificationEnqueuer,
	)

	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID}); err != nil {
		t.Fatalf("HandleOrganization() = %v, want committed completion despite enqueue failures", err)
	}

	store.mu.Lock()
	completionCalls := store.completionCalls
	gotBatchID := store.gotBatchID
	gotToken := store.gotToken
	gotIndexPlan := store.gotIndexPlan.Clone()
	gotScrapePlan := store.gotScrapePlan.Clone()
	gotNotificationPlan := store.gotNotificationPlan.Clone()
	legacyCompletionBatch := store.completedBatch
	failBatchCalls := store.failBatchCalls
	store.mu.Unlock()
	if completionCalls != 1 || gotBatchID != batchID || gotToken != token {
		t.Fatalf("durable completion calls=%d batch=%s token=%s", completionCalls, gotBatchID, gotToken)
	}
	if !reflect.DeepEqual(gotIndexPlan, completionPlan) ||
		!reflect.DeepEqual(gotScrapePlan, scrapePlan) ||
		!reflect.DeepEqual(gotNotificationPlan, notificationPlan) {
		t.Fatalf("persisted plans differ: index=%#v scrape=%#v notification=%#v",
			gotIndexPlan, gotScrapePlan, gotNotificationPlan)
	}
	if legacyCompletionBatch != uuid.Nil {
		t.Fatalf("legacy CompleteOrganizationBatchForIndexing called for batch %s", legacyCompletionBatch)
	}
	if failBatchCalls != 0 {
		t.Fatalf("enqueue failure released committed batch %d times", failBatchCalls)
	}

	if scrapePlanner.gotBatchID != batchID || scrapePlanner.gotIntentID != intentID ||
		!reflect.DeepEqual(scrapePlanner.gotOrganizerPlan, plan) {
		t.Fatalf("scrape planner inputs = %s/%s/%#v", scrapePlanner.gotBatchID, scrapePlanner.gotIntentID, scrapePlanner.gotOrganizerPlan)
	}
	if notificationPlanner.successCalls != 1 || notificationPlanner.legacyCalls != 0 ||
		notificationPlanner.gotBatchID != batchID || notificationPlanner.gotUsername != username ||
		notificationPlanner.gotAppDomain != "https://moviepilot.example" ||
		!reflect.DeepEqual(notificationPlanner.gotSnapshots, snapshots) {
		t.Fatalf("notification planner inputs = calls %d/%d, %s/%q/%q/%#v",
			notificationPlanner.successCalls, notificationPlanner.legacyCalls,
			notificationPlanner.gotBatchID, notificationPlanner.gotUsername,
			notificationPlanner.gotAppDomain, notificationPlanner.gotSnapshots)
	}
	if got := scrapeEnqueuer.snapshot(); !reflect.DeepEqual(got, []uuid.UUID{scrapeRequestID}) {
		t.Fatalf("scrape enqueue IDs = %v", got)
	}
	if got := notificationEnqueuer.snapshot(); !reflect.DeepEqual(got, []uuid.UUID{notificationRequestID}) {
		t.Fatalf("notification enqueue IDs = %v", got)
	}
	mediaIndexEnqueuer.mu.Lock()
	mediaIndexIDs := append([]uuid.UUID(nil), mediaIndexEnqueuer.ids...)
	mediaIndexEnqueuer.mu.Unlock()
	if !reflect.DeepEqual(mediaIndexIDs, []uuid.UUID{mediaIndexRequestID}) {
		t.Fatalf("media index enqueue IDs = %v", mediaIndexIDs)
	}
	if legacyNFO.calls.Load() != 0 || legacyNotification.calls.Load() != 0 {
		t.Fatalf("legacy synchronous side effects called: NFO=%d notification=%d",
			legacyNFO.calls.Load(), legacyNotification.calls.Load())
	}
}

func TestHandlerCompleteBatchSelectsSuccessSnapshotPlanner(t *testing.T) {
	t.Parallel()

	batchID := uuid.New()
	intentID := uuid.New()
	username := "snapshot-owner"
	groupKey, err := organization.BuildGroupKey(organization.MediaContext{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	newSnapshot := organization.SuccessSnapshot{
		BatchID: batchID, IntentID: intentID, GroupKey: groupKey,
		Version: organization.CurrentSuccessVersion, Username: username, NeedNotify: true,
		Files: []organization.FileFact{{Ordinal: 0, Role: "video", SizeBytes: 1, Season: 0}},
	}
	legacySnapshot := newSnapshot.Clone()
	legacySnapshot.Version = 0
	legacySnapshot.Legacy = true
	secondLegacySnapshot := legacySnapshot.Clone()
	secondLegacySnapshot.Files[0].Season = 1
	secondLegacySnapshot.GroupKey, err = organization.BuildGroupKey(secondLegacySnapshot.Media, 1)
	if err != nil {
		t.Fatal(err)
	}
	mixedNewSnapshot := newSnapshot.Clone()
	mixedNewSnapshot.Files[0].Season = 1
	mixedNewSnapshot.GroupKey, err = organization.BuildGroupKey(mixedNewSnapshot.Media, 1)
	if err != nil {
		t.Fatal(err)
	}
	legacyInput := organization.SuccessInput{IntentID: intentID, Version: 0, Legacy: true}
	newInput := organization.SuccessInput{
		IntentID: intentID, Version: organization.CurrentSuccessVersion, Username: username,
	}
	organizerPlan := organizer.Plan{
		ID: "sha256:" + strings.Repeat("a", 64), TargetRoot: t.TempDir(),
		Operations: []organizer.Operation{{ExpectedSize: 1}},
	}
	batch := database.OrganizationBatch{ID: batchID, IntentID: intentID, LeaseToken: uuid.New(), Plan: organizerPlan}
	scrapePlan := scrapeflow.Plan{BatchID: batchID, IntentID: intentID, Username: username}
	legacyPlan, err := notificationflow.BuildPlan(batchID, username, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	successPlan := legacyPlan.Clone()
	plannerFailure := errors.New("V2 planner failure: path=C:\\secret")
	storageFailure := errors.New("snapshot storage failure: token=secret")

	tests := []struct {
		name                  string
		snapshots             []organization.SuccessSnapshot
		snapshotErr           error
		successInput          organization.SuccessInput
		successInputErr       error
		successErr            error
		wantErr               bool
		wantSuccessInputCalls int
		wantSuccessCalls      int
		wantLegacyCalls       int
		wantCompletionCalls   int
	}{
		{name: "新快照走 V2 planner", snapshots: []organization.SuccessSnapshot{newSnapshot}, wantSuccessCalls: 1, wantCompletionCalls: 1},
		{name: "显式 legacy 快照走兼容 planner", snapshots: []organization.SuccessSnapshot{legacySnapshot}, wantLegacyCalls: 1, wantCompletionCalls: 1},
		{name: "缺快照且明确 legacy input 走兼容 planner", snapshotErr: database.ErrOrganizationSuccessSnapshotNotFound, successInput: legacyInput, wantSuccessInputCalls: 1, wantLegacyCalls: 1, wantCompletionCalls: 1},
		{name: "新版本缺快照视为损坏", snapshotErr: database.ErrOrganizationSuccessSnapshotNotFound, successInput: newInput, wantSuccessInputCalls: 1, wantErr: true},
		{name: "缺快照且 input 读取失败不回退", snapshotErr: database.ErrOrganizationSuccessSnapshotNotFound, successInputErr: database.ErrDownloadIntentSuccessInputNotFound, wantSuccessInputCalls: 1, wantErr: true},
		{name: "空集合不是兼容缺失", wantErr: true},
		{name: "快照持久化失败不回退", snapshotErr: storageFailure, wantErr: true},
		{name: "V2 planner 失败不回退", snapshots: []organization.SuccessSnapshot{newSnapshot}, successErr: plannerFailure, wantErr: true, wantSuccessCalls: 1},
		{name: "多个 legacy 快照视为损坏", snapshots: []organization.SuccessSnapshot{legacySnapshot, secondLegacySnapshot}, wantErr: true},
		{name: "混合版本不是显式 legacy", snapshots: []organization.SuccessSnapshot{legacySnapshot, mixedNewSnapshot}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &durableCompletionStoreFake{
				executionStoreFake: &executionStoreFake{}, snapshots: tt.snapshots, snapshotErr: tt.snapshotErr,
				successInput: tt.successInput, successInputErr: tt.successInputErr,
			}
			planner := &durableNotificationPlannerFake{
				plan: legacyPlan, successPlan: successPlan, successErr: tt.successErr,
			}
			handler := &Handler{
				sideEffectStore:      store,
				scrapePlanner:        &durableScrapePlannerFake{plan: scrapePlan},
				notificationPlanner:  planner,
				scrapeEnqueuer:       &durableScrapeEnqueuerFake{},
				notificationEnqueuer: &durableNotificationEnqueuerFake{},
			}
			handler.SetAppDomain("https://moviepilot.example")

			_, _, _, gotErr := handler.completeBatch(t.Context(), batch, mediaindex.CompletionPlan{})
			if (gotErr != nil) != tt.wantErr {
				t.Fatalf("completeBatch() error = %v, wantErr %t", gotErr, tt.wantErr)
			}
			if gotErr != nil && !errors.Is(gotErr, ErrOrganizationIndexPlanning) {
				t.Fatalf("completeBatch() error = %v, want ErrOrganizationIndexPlanning", gotErr)
			}
			if store.snapshotCalls != 1 || store.gotSnapshotBatchID != batchID || store.gotSnapshotUsername != username {
				t.Fatalf("snapshot reads = %d, input=%s/%q", store.snapshotCalls, store.gotSnapshotBatchID, store.gotSnapshotUsername)
			}
			if store.successInputCalls != tt.wantSuccessInputCalls {
				t.Fatalf("success input reads = %d, want %d", store.successInputCalls, tt.wantSuccessInputCalls)
			}
			if store.successInputCalls > 0 &&
				(store.gotSuccessInputID != intentID || store.gotSuccessInputUser != username) {
				t.Fatalf("success input read = %s/%q", store.gotSuccessInputID, store.gotSuccessInputUser)
			}
			if planner.successCalls != tt.wantSuccessCalls || planner.legacyCalls != tt.wantLegacyCalls ||
				store.completionCalls != tt.wantCompletionCalls {
				t.Fatalf("calls success/legacy/completion = %d/%d/%d, want %d/%d/%d",
					planner.successCalls, planner.legacyCalls, store.completionCalls,
					tt.wantSuccessCalls, tt.wantLegacyCalls, tt.wantCompletionCalls)
			}
			if planner.successCalls > 0 && (planner.gotAppDomain != "https://moviepilot.example" ||
				!reflect.DeepEqual(planner.gotSnapshots, tt.snapshots)) {
				t.Fatalf("PlanSuccess inputs = domain %q snapshots %#v", planner.gotAppDomain, planner.gotSnapshots)
			}
		})
	}
}

type durableCompletionStoreFake struct {
	*executionStoreFake

	requestID       uuid.UUID
	scrapeIDs       []uuid.UUID
	notificationIDs []uuid.UUID
	completionErr   error
	snapshots       []organization.SuccessSnapshot
	snapshotErr     error
	successInput    organization.SuccessInput
	successInputErr error

	completionCalls     int
	gotBatchID          uuid.UUID
	gotToken            uuid.UUID
	gotIndexPlan        mediaindex.CompletionPlan
	gotScrapePlan       scrapeflow.Plan
	gotNotificationPlan notificationflow.Plan
	snapshotCalls       int
	gotSnapshotBatchID  uuid.UUID
	gotSnapshotUsername string
	successInputCalls   int
	gotSuccessInputID   uuid.UUID
	gotSuccessInputUser string
}

func (s *durableCompletionStoreFake) ListOrganizationSuccessSnapshots(
	_ context.Context,
	username string,
	batchID uuid.UUID,
) ([]organization.SuccessSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotCalls++
	s.gotSnapshotBatchID = batchID
	s.gotSnapshotUsername = username
	items := make([]organization.SuccessSnapshot, len(s.snapshots))
	for index := range s.snapshots {
		items[index] = s.snapshots[index].Clone()
	}
	return items, s.snapshotErr
}

func (s *durableCompletionStoreFake) GetDownloadIntentSuccessInput(
	_ context.Context,
	username string,
	intentID uuid.UUID,
) (organization.SuccessInput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.successInputCalls++
	s.gotSuccessInputID = intentID
	s.gotSuccessInputUser = username
	return s.successInput.Clone(), s.successInputErr
}

func (s *durableCompletionStoreFake) CompleteOrganizationBatchWithSideEffects(
	_ context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	indexPlan mediaindex.CompletionPlan,
	scrapePlan scrapeflow.Plan,
	notificationPlan notificationflow.Plan,
) (uuid.UUID, bool, []uuid.UUID, []uuid.UUID, error) {
	s.mu.Lock()
	s.completionCalls++
	s.gotBatchID = batchID
	s.gotToken = token
	s.gotIndexPlan = indexPlan.Clone()
	s.gotScrapePlan = scrapePlan.Clone()
	s.gotNotificationPlan = notificationPlan.Clone()
	requestID := s.requestID
	scrapeIDs := append([]uuid.UUID(nil), s.scrapeIDs...)
	notificationIDs := append([]uuid.UUID(nil), s.notificationIDs...)
	err := s.completionErr
	s.mu.Unlock()
	return requestID, requestID != uuid.Nil, scrapeIDs, notificationIDs, err
}

type durableScrapePlannerFake struct {
	plan scrapeflow.Plan
	err  error

	gotBatchID       uuid.UUID
	gotIntentID      uuid.UUID
	gotOrganizerPlan organizer.Plan
}

func (p *durableScrapePlannerFake) Plan(
	_ context.Context,
	batchID uuid.UUID,
	intentID uuid.UUID,
	plan organizer.Plan,
) (scrapeflow.Plan, error) {
	p.gotBatchID = batchID
	p.gotIntentID = intentID
	p.gotOrganizerPlan = plan
	return p.plan.Clone(), p.err
}

type durableNotificationPlannerFake struct {
	plan        notificationflow.Plan
	successPlan notificationflow.Plan
	err         error
	successErr  error

	gotBatchID       uuid.UUID
	gotUsername      string
	gotOrganizerPlan organizer.Plan
	gotSnapshots     []organization.SuccessSnapshot
	gotAppDomain     string
	legacyCalls      int
	successCalls     int
}

func (p *durableNotificationPlannerFake) Plan(
	batchID uuid.UUID,
	username string,
	plan organizer.Plan,
) (notificationflow.Plan, error) {
	p.legacyCalls++
	p.gotBatchID = batchID
	p.gotUsername = username
	p.gotOrganizerPlan = plan
	return p.plan.Clone(), p.err
}

func (p *durableNotificationPlannerFake) PlanSuccess(
	batchID uuid.UUID,
	username string,
	snapshots []organization.SuccessSnapshot,
	appDomain string,
) (notificationflow.Plan, error) {
	p.successCalls++
	p.gotBatchID = batchID
	p.gotUsername = username
	p.gotSnapshots = make([]organization.SuccessSnapshot, len(snapshots))
	for index := range snapshots {
		p.gotSnapshots[index] = snapshots[index].Clone()
	}
	p.gotAppDomain = appDomain
	return p.successPlan.Clone(), p.successErr
}

type durableScrapeEnqueuerFake struct {
	mu  sync.Mutex
	ids []uuid.UUID
	err error
}

func (e *durableScrapeEnqueuerFake) EnqueueScrape(_ context.Context, id uuid.UUID) error {
	e.mu.Lock()
	e.ids = append(e.ids, id)
	err := e.err
	e.mu.Unlock()
	return err
}

func (e *durableScrapeEnqueuerFake) snapshot() []uuid.UUID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]uuid.UUID(nil), e.ids...)
}

type durableNotificationEnqueuerFake struct {
	mu  sync.Mutex
	ids []uuid.UUID
	err error
}

func (e *durableNotificationEnqueuerFake) EnqueueNotification(_ context.Context, id uuid.UUID) error {
	e.mu.Lock()
	e.ids = append(e.ids, id)
	err := e.err
	e.mu.Unlock()
	return err
}

func (e *durableNotificationEnqueuerFake) snapshot() []uuid.UUID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]uuid.UUID(nil), e.ids...)
}

type legacyNFOScraperSpy struct{ calls atomic.Int32 }

func (s *legacyNFOScraperSpy) ScrapeMovieNFO(context.Context, int, string) error {
	s.calls.Add(1)
	return nil
}

func (s *legacyNFOScraperSpy) ScrapeTVShowNFO(context.Context, int, string) error {
	s.calls.Add(1)
	return nil
}

func (s *legacyNFOScraperSpy) ScrapeEpisodeNFO(context.Context, int, int, int, string) error {
	s.calls.Add(1)
	return nil
}

type legacyNotificationSenderSpy struct{ calls atomic.Int32 }

func (s *legacyNotificationSenderSpy) Send(context.Context, NotificationMessage) error {
	s.calls.Add(1)
	return nil
}
