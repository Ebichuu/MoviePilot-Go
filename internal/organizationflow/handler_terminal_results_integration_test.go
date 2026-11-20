//go:build integration

package organizationflow

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/moviepilot-go/moviepilot-go/internal/notificationflow"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/scrapeflow"
)

func TestOrganizationHandlerPostgresTerminalResultClosure(t *testing.T) {
	baseStore, pool, _, _ := newOrganizationProcessRecoveryStore(t)

	t.Run("partial success persists ordered cross-season effects and replays idempotently", func(t *testing.T) {
		fixture := createOrganizationTerminalHandlerFixture(t, baseStore, pool, []string{
			"Fixture.S01E01.mkv",
			"Fixture.S01E02.mkv",
			"Fixture.S02E03.mkv",
			"Fixture.S02E04.mkv",
		})
		mediaPlanner := &organizationTerminalMediaPlanner{}
		notificationEnqueuer := &organizationTerminalNotificationEnqueuer{}
		handler := newOrganizationTerminalHandler(
			t, fixture.store, mediaPlanner, notificationEnqueuer,
			map[string]string{
				"Fixture.S01E02.mkv": "S01 copy failed",
				"Fixture.S02E04.mkv": "S02 copy failed",
			},
		)

		if err := handler.HandleOrganization(
			t.Context(), jobs.OrganizationArgs{BatchID: fixture.batch.ID},
		); err != nil {
			t.Fatalf("HandleOrganization(): %v", err)
		}
		assertOrganizationTerminalBatchState(t, pool, fixture.batch.ID, "completed", []string{
			"completed", "terminal_failed", "completed", "terminal_failed",
		})

		planned := mediaPlanner.snapshot()
		if len(planned) != 1 || !sameOrganizationOperationSources(
			planned[0].Operations,
			[]organizer.Operation{fixture.plan.Operations[0], fixture.plan.Operations[2]},
		) {
			t.Fatalf("media-index success plan = %#v", planned)
		}

		completion := fixture.store.completionSnapshot()
		if completion.calls != 1 || len(completion.scrapePlan.Requests) != 2 {
			t.Fatalf("completion calls/scrape plan = %d/%#v", completion.calls, completion.scrapePlan)
		}
		wantSuccessPaths := []string{
			fixture.plan.Operations[0].Target,
			fixture.plan.Operations[2].Target,
		}
		gotScrapePaths := organizationTerminalScrapePaths(t, pool, fixture.batch.ID)
		if !reflect.DeepEqual(gotScrapePaths, sortedStrings(wantSuccessPaths)) {
			t.Fatalf("persisted scrape paths = %v, want %v", gotScrapePaths, sortedStrings(wantSuccessPaths))
		}

		seasonGroups := organizationTerminalSeasonGroups(t, fixture.snapshots)
		wantEvents := []organizationTerminalEvent{
			{key: organizationFailureEventKey(fixture.batch.ID, 1), mtype: string(notification.TypeManual)},
			{key: organizationSuccessEventKey(fixture.batch.ID, seasonGroups[1]), mtype: string(notification.TypeOrganize)},
			{key: organizationFailureEventKey(fixture.batch.ID, 3), mtype: string(notification.TypeManual)},
			{key: organizationSuccessEventKey(fixture.batch.ID, seasonGroups[2]), mtype: string(notification.TypeOrganize)},
		}
		gotEvents := organizationTerminalEvents(t, pool, fixture.batch.ID)
		if !reflect.DeepEqual(gotEvents, wantEvents) {
			t.Fatalf("persisted event timeline = %#v, want %#v", gotEvents, wantEvents)
		}
		if len(completion.notificationPlan.Events) != len(wantEvents) {
			t.Fatalf("persisted notification plan = %#v", completion.notificationPlan)
		}
		for index, event := range completion.notificationPlan.Events {
			if event.Event.EventKey != wantEvents[index].key || len(event.Requests) != 1 {
				t.Fatalf("notification plan event[%d] = %#v", index, event)
			}
		}
		if got := notificationEnqueuer.snapshot(); len(got) != len(wantEvents) {
			t.Fatalf("notification enqueue IDs = %v", got)
		}

		assertOrganizationNotificationReplay(
			t, baseStore, pool, fixture.batch.ID, completion,
		)
	})

	t.Run("all failures close with manual effects only", func(t *testing.T) {
		fixture := createOrganizationTerminalHandlerFixture(t, baseStore, pool, []string{
			"AllFailed.S01E01.mkv",
			"AllFailed.S02E03.mkv",
		})
		mediaPlanner := &organizationTerminalMediaPlanner{}
		notificationEnqueuer := &organizationTerminalNotificationEnqueuer{}
		handler := newOrganizationTerminalHandler(
			t, fixture.store, mediaPlanner, notificationEnqueuer,
			map[string]string{
				"AllFailed.S01E01.mkv": "first failure",
				"AllFailed.S02E03.mkv": "second failure",
			},
		)

		if err := handler.HandleOrganization(
			t.Context(), jobs.OrganizationArgs{BatchID: fixture.batch.ID},
		); err != nil {
			t.Fatalf("HandleOrganization(): %v", err)
		}
		assertOrganizationTerminalBatchState(t, pool, fixture.batch.ID, "completed", []string{
			"terminal_failed", "terminal_failed",
		})
		if planned := mediaPlanner.snapshot(); len(planned) != 0 {
			t.Fatalf("all-failure media-index plans = %#v, want none", planned)
		}
		completion := fixture.store.completionSnapshot()
		if completion.calls != 1 || len(completion.scrapePlan.Requests) != 0 {
			t.Fatalf("all-failure completion = %#v", completion)
		}
		wantEvents := []organizationTerminalEvent{
			{key: organizationFailureEventKey(fixture.batch.ID, 0), mtype: string(notification.TypeManual)},
			{key: organizationFailureEventKey(fixture.batch.ID, 1), mtype: string(notification.TypeManual)},
		}
		if got := organizationTerminalEvents(t, pool, fixture.batch.ID); !reflect.DeepEqual(got, wantEvents) {
			t.Fatalf("all-failure events = %#v, want %#v", got, wantEvents)
		}
		if paths := organizationTerminalScrapePaths(t, pool, fixture.batch.ID); len(paths) != 0 {
			t.Fatalf("all-failure scrape paths = %v", paths)
		}
		if got := notificationEnqueuer.snapshot(); len(got) != 2 {
			t.Fatalf("all-failure notification enqueue IDs = %v", got)
		}
	})
}

func TestOrganizationHandlerPostgresSnapshotlessLegacyFallback(t *testing.T) {
	baseStore, pool, _, _ := newOrganizationProcessRecoveryStore(t)

	t.Run("all success uses legacy completion", func(t *testing.T) {
		fixture := createSnapshotlessLegacyHandlerFixture(t, baseStore, pool)
		mediaPlanner := &organizationTerminalMediaPlanner{}
		handler := newOrganizationTerminalHandler(t, fixture.store, mediaPlanner, nil, nil)

		if err := handler.HandleOrganization(
			t.Context(), jobs.OrganizationArgs{BatchID: fixture.batch.ID},
		); err != nil {
			t.Fatalf("HandleOrganization(): %v", err)
		}
		assertOrganizationTerminalBatchState(t, pool, fixture.batch.ID, "completed", []string{"completed"})
		if planned := mediaPlanner.snapshot(); len(planned) != 1 ||
			!sameOrganizationOperationSources(planned[0].Operations, fixture.plan.Operations) {
			t.Fatalf("legacy media-index plan = %#v", planned)
		}
		want := []organizationTerminalEvent{{
			key:   "organization-completed:" + fixture.batch.ID.String(),
			mtype: string(notification.TypeOrganize),
		}}
		if got := organizationTerminalEvents(t, pool, fixture.batch.ID); !reflect.DeepEqual(got, want) {
			t.Fatalf("legacy events = %#v, want %#v", got, want)
		}
	})

	t.Run("terminal failure never guesses legacy effects", func(t *testing.T) {
		fixture := createSnapshotlessLegacyHandlerFixture(t, baseStore, pool)
		handler := newOrganizationTerminalHandler(
			t, fixture.store, &organizationTerminalMediaPlanner{}, nil,
			map[string]string{filepath.Base(fixture.plan.Operations[0].Source): "legacy copy failed"},
		)

		err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: fixture.batch.ID})
		if !errors.Is(err, ErrOrganizationIndexPlanning) {
			t.Fatalf("HandleOrganization() error = %v, want ErrOrganizationIndexPlanning", err)
		}
		assertOrganizationTerminalBatchState(t, pool, fixture.batch.ID, "failed", []string{"terminal_failed"})
		if got := organizationTerminalEvents(t, pool, fixture.batch.ID); len(got) != 0 {
			t.Fatalf("snapshotless failed legacy events = %#v, want none", got)
		}
		if completion := fixture.store.completionSnapshot(); completion.calls != 0 {
			t.Fatalf("snapshotless failed legacy completion calls = %d", completion.calls)
		}
	})
}

type organizationTerminalHandlerFixture struct {
	store     *organizationTerminalIntegrationStore
	batch     database.OrganizationBatch
	plan      organizer.Plan
	snapshots []organization.SuccessSnapshot
}

func createOrganizationTerminalHandlerFixture(
	t *testing.T,
	baseStore *database.Store,
	pool *pgxpool.Pool,
	names []string,
) organizationTerminalHandlerFixture {
	t.Helper()
	username, intentID := createOrganizationHandlerIdentity(t, pool, false)
	input := organization.SuccessInput{
		IntentID: intentID, Version: organization.CurrentSuccessVersion, Username: username,
		Background: true,
		Media: organization.MediaContext{
			Source: "tmdb", MediaID: "8080", TitleYear: "Integration Fixture (2026)",
			MediaType: "电视剧", Category: "Drama", VoteAverage: 8.8,
		},
		ResourceTerm: "2160p WEB-DL",
		Episodes: []organization.Episode{
			{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
			{Season: 2, Episode: 3}, {Season: 2, Episode: 4},
		},
	}
	createOrganizationHandlerSuccessInput(t, baseStore, pool, username, input)

	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	operations := make([]organizer.Operation, len(names))
	for ordinal, name := range names {
		operations[ordinal] = organizer.Operation{
			Source: filepath.Join(sourceRoot, name), Target: filepath.Join(targetRoot, name),
			Mode: organizer.Copy, ExpectedSize: int64(ordinal + 1), ExpectedSHA256: handlerTestSHA256,
		}
	}
	plan, err := organizer.BuildPlan(targetRoot, operations)
	if err != nil {
		t.Fatalf("BuildPlan(): %v", err)
	}
	snapshots, err := buildSuccessSnapshots(input, true, plan)
	if err != nil {
		t.Fatalf("buildSuccessSnapshots(): %v", err)
	}
	batch, err := baseStore.CreateOrganizationBatch(
		t.Context(), username, intentID, sourceRoot, plan, snapshots,
	)
	if err != nil {
		t.Fatalf("CreateOrganizationBatch(): %v", err)
	}
	return organizationTerminalHandlerFixture{
		store: &organizationTerminalIntegrationStore{Store: baseStore},
		batch: batch, plan: plan, snapshots: snapshots,
	}
}

func createSnapshotlessLegacyHandlerFixture(
	t *testing.T,
	baseStore *database.Store,
	pool *pgxpool.Pool,
) organizationTerminalHandlerFixture {
	t.Helper()
	username, intentID := createOrganizationHandlerIdentity(t, pool, true)
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	plan, err := organizer.BuildPlan(targetRoot, []organizer.Operation{{
		Source: filepath.Join(sourceRoot, "Legacy.Movie.mkv"),
		Target: filepath.Join(targetRoot, "Legacy.Movie.mkv"),
		Mode:   organizer.Copy, ExpectedSize: 1, ExpectedSHA256: handlerTestSHA256,
	}})
	if err != nil {
		t.Fatalf("BuildPlan(): %v", err)
	}
	batchID := uuid.NewSHA1(intentID, []byte(plan.ID))
	encoded, err := json.Marshal(plan.Operations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO organization_batches (
			id, intent_id, state, source_root, target_root, plan, last_error, next_attempt_at
		) VALUES ($1,$2,'planned',$3,$4,$5::jsonb,'',now())`,
		batchID, intentID, sourceRoot, targetRoot, encoded,
	); err != nil {
		t.Fatalf("insert snapshotless legacy batch: %v", err)
	}
	operation := plan.Operations[0]
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO organization_files (
			batch_id, ordinal, source_path, target_path, mode,
			expected_size, expected_sha256, state, last_error
		) VALUES ($1,0,$2,$3,$4,$5,$6,'planned','')`,
		batchID, operation.Source, operation.Target, operation.Mode,
		operation.ExpectedSize, operation.ExpectedSHA256,
	); err != nil {
		t.Fatalf("insert snapshotless legacy file: %v", err)
	}
	batch, err := baseStore.GetOrganizationBatch(t.Context(), username, batchID)
	if err != nil {
		t.Fatalf("GetOrganizationBatch(): %v", err)
	}
	if _, err := baseStore.ListOrganizationSuccessSnapshots(
		t.Context(), username, batchID,
	); !errors.Is(err, database.ErrOrganizationSuccessSnapshotNotFound) {
		t.Fatalf("snapshotless fixture snapshots error = %v", err)
	}
	return organizationTerminalHandlerFixture{
		store: &organizationTerminalIntegrationStore{Store: baseStore},
		batch: batch, plan: plan,
	}
}

func createOrganizationHandlerIdentity(
	t *testing.T,
	pool *pgxpool.Pool,
	legacy bool,
) (string, uuid.UUID) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	username := "organization-handler-" + suffix
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO users (username, password_hash) VALUES ($1, 'integration-password')`,
		username,
	); err != nil {
		t.Fatalf("insert organization handler user: %v", err)
	}
	intentID := uuid.New()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, candidate_key, label, state)
		VALUES ($1,$2,$3,$4,'completed')`,
		intentID, username, "sha256"+suffix+suffix, "mp-"+suffix,
	); err != nil {
		t.Fatalf("insert organization handler intent: %v", err)
	}
	var siteID int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, domain, url, adapter, enabled, config)
		VALUES ($1,$2,$3,'declarative',TRUE,'{}'::jsonb)
		RETURNING id`,
		"organization-handler-"+suffix,
		"organization-handler-"+suffix+".invalid",
		"https://organization-handler-"+suffix+".invalid",
	).Scan(&siteID); err != nil {
		t.Fatalf("insert organization handler site: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO download_intent_plans (
			intent_id, site_id, downloader_id, resolver_kind, resolver_version,
			resource_id, resource_secret, save_path, paused
		) VALUES ($1,$2,$3,'legacy',1,$4,'','',FALSE)`,
		intentID, siteID, uuid.NewString(), "organization-handler-"+suffix,
	); err != nil {
		t.Fatalf("insert organization handler download plan: %v", err)
	}
	if legacy {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO download_intent_success_inputs (
				intent_id, version, legacy, username, manual, background,
				media_source, media_id, title_year, media_type, category,
				vote_average, resource_term, poster_url, backdrop_url, episodes
			) VALUES ($1,0,TRUE,'',FALSE,FALSE,'','','','','',0,'','','','[]'::jsonb)`,
			intentID,
		); err != nil {
			t.Fatalf("insert legacy success input: %v", err)
		}
	}
	return username, intentID
}

func createOrganizationHandlerSuccessInput(
	t *testing.T,
	store *database.Store,
	pool *pgxpool.Pool,
	username string,
	input organization.SuccessInput,
) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := store.CreateDownloadIntentSuccessInputTx(
		t.Context(), tx, username, input,
	); err != nil {
		t.Fatalf("CreateDownloadIntentSuccessInputTx(): %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func newOrganizationTerminalHandler(
	t *testing.T,
	store *organizationTerminalIntegrationStore,
	mediaPlanner *organizationTerminalMediaPlanner,
	notificationEnqueuer *organizationTerminalNotificationEnqueuer,
	failures map[string]string,
) *Handler {
	t.Helper()
	if notificationEnqueuer == nil {
		notificationEnqueuer = &organizationTerminalNotificationEnqueuer{}
	}
	scrapePlanner, err := scrapeflow.NewPlanner(store, false)
	if err != nil {
		t.Fatalf("scrapeflow.NewPlanner(): %v", err)
	}
	notificationPlanner, err := notificationflow.NewPlanner(organizationTerminalTargets{})
	if err != nil {
		t.Fatalf("notificationflow.NewPlanner(): %v", err)
	}
	handler, err := NewHandler(
		store,
		organizationProcessRecoveryEnqueuer{},
		mediaPlanner,
		organizationProcessRecoveryMediaEnqueuer{},
	)
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}
	handler.SetAppDomain("https://mp.example")
	handler.SetDurableSideEffects(
		store,
		scrapePlanner,
		notificationPlanner,
		organizationTerminalScrapeEnqueuer{},
		notificationEnqueuer,
	)
	handler.execute = func(_ context.Context, _ string, plan organizer.Plan) error {
		if len(plan.Operations) != 1 {
			return errors.New("unexpected organization operation page")
		}
		if message := failures[filepath.Base(plan.Operations[0].Source)]; message != "" {
			return errors.New(message)
		}
		return nil
	}
	return handler
}

type organizationTerminalIntegrationStore struct {
	*database.Store
	mu         sync.Mutex
	claimed    database.OrganizationBatch
	completion organizationTerminalCompletion
}

func (s *organizationTerminalIntegrationStore) ClaimOrganizationBatch(
	ctx context.Context,
	batchID uuid.UUID,
	now time.Time,
	leaseDuration time.Duration,
) (database.OrganizationBatch, bool, error) {
	batch, claimed, err := s.Store.ClaimOrganizationBatch(ctx, batchID, now, leaseDuration)
	if err == nil && claimed {
		s.mu.Lock()
		s.claimed = batch
		s.mu.Unlock()
	}
	return batch, claimed, err
}

func (s *organizationTerminalIntegrationStore) CompleteOrganizationBatchWithSideEffects(
	ctx context.Context,
	batchID uuid.UUID,
	token uuid.UUID,
	indexPlan mediaindex.CompletionPlan,
	scrapePlan scrapeflow.Plan,
	notificationPlan notificationflow.Plan,
) (uuid.UUID, bool, []uuid.UUID, []uuid.UUID, error) {
	s.mu.Lock()
	s.completion.calls++
	s.completion.token = token
	s.completion.indexPlan = indexPlan.Clone()
	s.completion.scrapePlan = scrapePlan.Clone()
	s.completion.notificationPlan = notificationPlan.Clone()
	s.mu.Unlock()
	return s.Store.CompleteOrganizationBatchWithSideEffects(
		ctx, batchID, token, indexPlan, scrapePlan, notificationPlan,
	)
}

func (s *organizationTerminalIntegrationStore) completionSnapshot() organizationTerminalCompletion {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.completion
	result.indexPlan = result.indexPlan.Clone()
	result.scrapePlan = result.scrapePlan.Clone()
	result.notificationPlan = result.notificationPlan.Clone()
	return result
}

type organizationTerminalCompletion struct {
	calls            int
	token            uuid.UUID
	indexPlan        mediaindex.CompletionPlan
	scrapePlan       scrapeflow.Plan
	notificationPlan notificationflow.Plan
}

type organizationTerminalMediaPlanner struct {
	mu    sync.Mutex
	plans []organizer.Plan
}

func (p *organizationTerminalMediaPlanner) MediaIndexCompletionPlan(
	_ context.Context,
	plan organizer.Plan,
) (mediaindex.CompletionPlan, error) {
	p.mu.Lock()
	p.plans = append(p.plans, cloneOrganizationTerminalPlan(plan))
	p.mu.Unlock()
	return mediaindex.CompletionPlan{}, nil
}

func (p *organizationTerminalMediaPlanner) snapshot() []organizer.Plan {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]organizer.Plan, len(p.plans))
	for index := range p.plans {
		result[index] = cloneOrganizationTerminalPlan(p.plans[index])
	}
	return result
}

type organizationTerminalTargets struct{}

func (organizationTerminalTargets) Targets(message notification.Message) []notification.Target {
	source := "organization-success"
	if message.Type == notification.TypeManual {
		source = "organization-manual"
	}
	return []notification.Target{{Channel: notification.ChannelTelegram, Source: source}}
}

type organizationTerminalScrapeEnqueuer struct{}

func (organizationTerminalScrapeEnqueuer) EnqueueScrape(context.Context, uuid.UUID) error {
	return nil
}

type organizationTerminalNotificationEnqueuer struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (e *organizationTerminalNotificationEnqueuer) EnqueueNotification(
	_ context.Context,
	id uuid.UUID,
) error {
	e.mu.Lock()
	e.ids = append(e.ids, id)
	e.mu.Unlock()
	return nil
}

func (e *organizationTerminalNotificationEnqueuer) snapshot() []uuid.UUID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]uuid.UUID(nil), e.ids...)
}

type organizationTerminalEvent struct {
	key   string
	mtype string
}

func organizationTerminalEvents(
	t *testing.T,
	pool *pgxpool.Pool,
	batchID uuid.UUID,
) []organizationTerminalEvent {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT event_key, COALESCE(mtype, '')
		FROM notification_events
		WHERE batch_id=$1
		ORDER BY id`, batchID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []organizationTerminalEvent
	for rows.Next() {
		var event organizationTerminalEvent
		if err := rows.Scan(&event.key, &event.mtype); err != nil {
			t.Fatal(err)
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func organizationTerminalScrapePaths(
	t *testing.T,
	pool *pgxpool.Pool,
	batchID uuid.UUID,
) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT media_path
		FROM scrape_requests
		WHERE batch_id=$1
		ORDER BY media_path`, batchID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatal(err)
		}
		result = append(result, path)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertOrganizationTerminalBatchState(
	t *testing.T,
	pool *pgxpool.Pool,
	batchID uuid.UUID,
	wantBatch string,
	wantFiles []string,
) {
	t.Helper()
	var batchState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM organization_batches WHERE id=$1`, batchID,
	).Scan(&batchState); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(t.Context(), `
		SELECT state FROM organization_files WHERE batch_id=$1 ORDER BY ordinal`, batchID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var fileStates []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
		fileStates = append(fileStates, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if batchState != wantBatch || !reflect.DeepEqual(fileStates, wantFiles) {
		t.Fatalf("batch/files state = %q/%v, want %q/%v",
			batchState, fileStates, wantBatch, wantFiles)
	}
}

func assertOrganizationNotificationReplay(
	t *testing.T,
	store *database.Store,
	pool *pgxpool.Pool,
	batchID uuid.UUID,
	completion organizationTerminalCompletion,
) {
	t.Helper()
	if len(completion.notificationPlan.Events) == 0 ||
		len(completion.notificationPlan.Events[0].Requests) == 0 {
		t.Fatal("notification replay fixture has no request")
	}
	sentID := completion.notificationPlan.Events[0].Requests[0].ID
	now := time.Now().UTC().Add(time.Second)
	lease, claimed, err := store.ClaimNotification(t.Context(), sentID, now, time.Minute)
	if err != nil || !claimed || !lease.Valid(sentID) {
		t.Fatalf("ClaimNotification() = %#v, %v, %v", lease, claimed, err)
	}
	if err := store.CompleteNotification(t.Context(), sentID, lease.Token, now); err != nil {
		t.Fatalf("CompleteNotification(): %v", err)
	}

	requestID, created, scrapeDue, notificationDue, err :=
		store.CompleteOrganizationBatchWithSideEffects(
			t.Context(), batchID, completion.token, completion.indexPlan,
			completion.scrapePlan, completion.notificationPlan,
		)
	if err != nil || requestID != uuid.Nil || created ||
		len(scrapeDue) != 0 || len(notificationDue) != 0 {
		t.Fatalf("replayed completion = %s/%v/%v/%v/%v",
			requestID, created, scrapeDue, notificationDue, err)
	}

	var eventCount, notificationCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM notification_events WHERE batch_id=$1`, batchID,
	).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM notifications WHERE batch_id=$1`, batchID,
	).Scan(&notificationCount); err != nil {
		t.Fatal(err)
	}
	expectedRequests := 0
	for _, event := range completion.notificationPlan.Events {
		expectedRequests += len(event.Requests)
	}
	if eventCount != len(completion.notificationPlan.Events) || notificationCount != expectedRequests {
		t.Fatalf("replay row counts = %d/%d, want %d/%d",
			eventCount, notificationCount,
			len(completion.notificationPlan.Events), expectedRequests)
	}
	var sentCount, pendingCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE state='sent'),
			count(*) FILTER (WHERE state='pending')
		FROM notifications WHERE batch_id=$1`, batchID,
	).Scan(&sentCount, &pendingCount); err != nil {
		t.Fatal(err)
	}
	if sentCount != 1 || pendingCount != expectedRequests-1 {
		t.Fatalf("notification states after replay = sent %d pending %d, want 1/%d",
			sentCount, pendingCount, expectedRequests-1)
	}
}

func organizationTerminalSeasonGroups(
	t *testing.T,
	snapshots []organization.SuccessSnapshot,
) map[int]string {
	t.Helper()
	result := make(map[int]string, len(snapshots))
	for _, snapshot := range snapshots {
		if len(snapshot.Files) == 0 {
			t.Fatalf("empty success snapshot: %#v", snapshot)
		}
		result[snapshot.Files[0].Season] = snapshot.GroupKey
	}
	return result
}

func organizationFailureEventKey(batchID uuid.UUID, ordinal int) string {
	return "organization-file-failed:" + batchID.String() + ":" + strconv.Itoa(ordinal)
}

func organizationSuccessEventKey(batchID uuid.UUID, groupKey string) string {
	return "organization-completed:" + batchID.String() + ":" + groupKey
}

func sameOrganizationOperationSources(left, right []organizer.Operation) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Source != right[index].Source {
			return false
		}
	}
	return true
}

func cloneOrganizationTerminalPlan(plan organizer.Plan) organizer.Plan {
	plan.Operations = append([]organizer.Operation(nil), plan.Operations...)
	return plan
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

var _ ExecutionStore = (*organizationTerminalIntegrationStore)(nil)
var _ SideEffectCompletionStore = (*organizationTerminalIntegrationStore)(nil)
var _ OrganizationOutcomeStore = (*organizationTerminalIntegrationStore)(nil)
var _ TerminalOrganizationFileStore = (*organizationTerminalIntegrationStore)(nil)
