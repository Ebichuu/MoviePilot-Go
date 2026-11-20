//go:build integration

package downloadflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	appconfig "github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

const (
	submissionIntegrationDatabaseEnv = "MOVIEPILOT_TEST_DATABASE_URL"
	submissionIntegrationSchema      = "mp_submission_it_"
	submissionIntegrationOwner       = "submission-integration-owner"
	submissionIntegrationDownloader  = "9a227682-0367-4e24-b3e4-f9d7f34468fb"
)

func TestSubmissionServicePostgresSubscriptionEpisodeReplayIsIdempotent(t *testing.T) {
	store, pool := newSubmissionIntegrationStore(t)
	insertSubmissionIntegrationUser(t, pool, submissionIntegrationOwner)
	subscriptionID := createSubmissionIntegrationSubscription(
		t, store, submissionIntegrationOwner, "idempotent-replay",
	)
	episodes := []subscription.Episode{
		{Season: 1, Episode: 1},
		{Season: 1, Episode: 2},
	}
	if err := store.UpsertSubscriptionEpisodes(
		t.Context(), submissionIntegrationOwner, subscriptionID, episodes,
	); err != nil {
		t.Fatalf("initialize subscription episodes: %v", err)
	}

	siteID := insertSubmissionIntegrationSite(t, pool, "submission-idempotent-site")
	candidate, intent, plan := submissionIntegrationDownload(t, siteID, "101", "/downloads/idempotent")
	_ = candidate
	enqueuer := &submissionIntegrationEnqueuer{}
	service, err := NewSubmissionService(pool, store, enqueuer)
	if err != nil {
		t.Fatalf("NewSubmissionService() error = %v", err)
	}
	storedID, err := service.SubmitSubscriptionEpisodes(
		t.Context(), submissionIntegrationOwner, subscriptionID, intent, plan,
		submissionIntegrationSuccessInput(submissionIntegrationOwner, intent.ID, episodes), episodes,
	)
	if err != nil {
		t.Fatalf("first SubmitSubscriptionEpisodes() error = %v", err)
	}
	if storedID != intent.ID {
		t.Fatalf("first stored intent = %s, want %s", storedID, intent.ID)
	}
	if got := enqueuer.Count(); got != 1 {
		t.Fatalf("first enqueue count = %d, want 1", got)
	}

	updated, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), submissionIntegrationOwner, storedID,
		[]subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeAcquired},
		subscription.SubscriptionEpisodeDownloading,
	)
	if err != nil {
		t.Fatalf("advance bound episodes to downloading: %v", err)
	}
	if updated != int64(len(episodes)) {
		t.Fatalf("advanced episodes = %d, want %d", updated, len(episodes))
	}

	replayIntent := intent
	replayIntent.ID = uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	replayIntent.Label = "mp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	replayPlan := plan
	replayPlan.IntentID = replayIntent.ID
	replayedID, err := service.SubmitSubscriptionEpisodes(
		t.Context(), submissionIntegrationOwner, subscriptionID,
		replayIntent, replayPlan,
		submissionIntegrationSuccessInput(submissionIntegrationOwner, replayIntent.ID, episodes), episodes,
	)
	if err != nil {
		t.Fatalf("replayed SubmitSubscriptionEpisodes() error = %v", err)
	}
	if replayedID != storedID {
		t.Fatalf("replayed stored intent = %s, want %s", replayedID, storedID)
	}
	if got := enqueuer.Count(); got != 2 {
		t.Fatalf("replayed enqueue count = %d, want 2", got)
	}

	assertSubmissionIntegrationCounts(t, pool, submissionIntegrationOwner, 1, 1)
	assertSubmissionIntegrationEpisodes(
		t, store, submissionIntegrationOwner, subscriptionID, episodes,
		subscription.SubscriptionEpisodeDownloading, storedID,
	)
	if _, err := store.GetDownloadIntent(
		t.Context(), submissionIntegrationOwner, replayIntent.ID,
	); !errors.Is(err, database.ErrDownloadIntentNotFound) {
		t.Fatalf("replay input UUID persisted unexpectedly: %v", err)
	}
	storedPlan, err := store.GetDownloadIntentPlan(
		t.Context(), submissionIntegrationOwner, storedID,
	)
	if err != nil {
		t.Fatalf("read stored immutable plan: %v", err)
	}
	if storedPlan.IntentID != storedID || storedPlan.ResourceID != plan.ResourceID ||
		storedPlan.DownloaderID != plan.DownloaderID || storedPlan.SavePath != plan.SavePath {
		t.Fatalf("stored immutable plan changed on replay: %#v", storedPlan)
	}
}

func TestSubmissionServicePostgresRSSStateContract(t *testing.T) {
	tests := []struct {
		name          string
		initialState  subscription.State
		initialV2     string
		expectedState subscription.State
		wantSuccess   bool
		wantState     subscription.State
		wantV2        string
	}{
		{
			name: "resolved target becomes acquired", initialState: subscription.Active,
			initialV2: "R", expectedState: subscription.Active, wantSuccess: true,
			wantState: subscription.Acquired, wantV2: "R",
		},
		{
			name: "pending-identification target stays pending", initialState: subscription.PendingIdentification,
			initialV2: "P", expectedState: subscription.PendingIdentification, wantSuccess: true,
			wantState: subscription.PendingIdentification, wantV2: "P",
		},
		{
			name: "new target is excluded", initialState: subscription.PendingSearch,
			initialV2: "N", expectedState: subscription.Active,
			wantState: subscription.PendingSearch, wantV2: "N",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, pool := newSubmissionIntegrationStore(t)
			username := "rss-state-" + strings.ReplaceAll(tt.name, " ", "-")
			insertSubmissionIntegrationUser(t, pool, username)
			subscriptionID := createSubmissionIntegrationSubscription(t, store, username, tt.name)
			episodes := []subscription.Episode{{Season: 1, Episode: 1}}
			if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, episodes); err != nil {
				t.Fatalf("initialize RSS subscription episodes: %v", err)
			}
			if _, err := pool.Exec(t.Context(), `
				UPDATE subscriptions
				SET internal_state=$1, v2_state=$2, updated_at=now()
				WHERE id=$3`, tt.initialState, tt.initialV2, subscriptionID); err != nil {
				t.Fatalf("prepare RSS subscription state: %v", err)
			}
			siteID := insertSubmissionIntegrationSite(t, pool, "rss-state-"+uuid.NewString())
			_, intent, plan := submissionIntegrationDownload(t, siteID, uuid.NewString(), "/downloads/rss-state")
			enqueuer := &submissionIntegrationEnqueuer{}
			service, err := NewSubmissionService(pool, store, enqueuer)
			if err != nil {
				t.Fatal(err)
			}

			storedID, err := service.SubmitRSSSubscriptionEpisodes(
				t.Context(), username, subscriptionID, tt.expectedState,
				intent, plan, submissionIntegrationSuccessInput(username, intent.ID, episodes), episodes,
			)
			if tt.wantSuccess {
				if err != nil || storedID != intent.ID {
					t.Fatalf("SubmitRSSSubscriptionEpisodes() = (%s, %v)", storedID, err)
				}
				if enqueuer.Count() != 1 {
					t.Fatalf("RSS enqueue count = %d, want 1", enqueuer.Count())
				}
				assertSubmissionIntegrationCounts(t, pool, username, 1, 1)
				assertSubmissionIntegrationEpisodes(
					t, store, username, subscriptionID, episodes,
					subscription.SubscriptionEpisodeAcquired, storedID,
				)
			} else {
				if storedID != uuid.Nil || !errors.Is(err, ErrDownloadSubmission) {
					t.Fatalf("excluded RSS submission = (%s, %v)", storedID, err)
				}
				if enqueuer.Count() != 0 {
					t.Fatalf("excluded RSS enqueue count = %d, want 0", enqueuer.Count())
				}
				assertSubmissionIntegrationCounts(t, pool, username, 0, 0)
				assertSubmissionIntegrationEpisodes(
					t, store, username, subscriptionID, episodes,
					subscription.SubscriptionEpisodeMissing, uuid.Nil,
				)
			}

			var (
				state   subscription.State
				v2State string
				current *uuid.UUID
			)
			if err := pool.QueryRow(t.Context(), `
				SELECT internal_state, v2_state, current_download_intent_id
				FROM subscriptions WHERE id=$1`, subscriptionID,
			).Scan(&state, &v2State, &current); err != nil {
				t.Fatalf("read RSS subscription state: %v", err)
			}
			if state != tt.wantState || v2State != tt.wantV2 {
				t.Fatalf("RSS subscription state = (%q, %q), want (%q, %q)", state, v2State, tt.wantState, tt.wantV2)
			}
			if tt.wantSuccess {
				if current == nil || *current != storedID {
					t.Fatalf("RSS current intent = %v, want %s", current, storedID)
				}
			} else if current != nil {
				t.Fatalf("excluded RSS current intent = %s, want nil", *current)
			}
		})
	}
}

func TestSubmissionServicePostgresSubscriptionEpisodeReplayConflictsRollback(t *testing.T) {
	t.Run("different intent cannot replace bound episodes", func(t *testing.T) {
		store, pool := newSubmissionIntegrationStore(t)
		const username = "submission-different-intent-owner"
		insertSubmissionIntegrationUser(t, pool, username)
		subscriptionID := createSubmissionIntegrationSubscription(t, store, username, "different-intent")
		episodes := []subscription.Episode{{Season: 2, Episode: 3}}
		if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, episodes); err != nil {
			t.Fatalf("initialize subscription episodes: %v", err)
		}
		siteID := insertSubmissionIntegrationSite(t, pool, "submission-conflict-site")
		_, originalIntent, originalPlan := submissionIntegrationDownload(t, siteID, "201", "/downloads/original")
		enqueuer := &submissionIntegrationEnqueuer{}
		service, err := NewSubmissionService(pool, store, enqueuer)
		if err != nil {
			t.Fatal(err)
		}
		storedID, err := service.SubmitSubscriptionEpisodes(
			t.Context(), username, subscriptionID, originalIntent, originalPlan,
			submissionIntegrationSuccessInput(username, originalIntent.ID, episodes), episodes,
		)
		if err != nil {
			t.Fatalf("submit original episode intent: %v", err)
		}

		_, conflictingIntent, conflictingPlan := submissionIntegrationDownload(
			t, siteID, "202", "/downloads/conflicting",
		)
		gotID, err := service.SubmitSubscriptionEpisodes(
			t.Context(), username, subscriptionID,
			conflictingIntent, conflictingPlan,
			submissionIntegrationSuccessInput(username, conflictingIntent.ID, episodes), episodes,
		)
		if gotID != uuid.Nil || !errors.Is(err, ErrDownloadSubmission) {
			t.Fatalf("conflicting replay = (%s, %v), want nil ID and ErrDownloadSubmission", gotID, err)
		}
		if got := enqueuer.Count(); got != 1 {
			t.Fatalf("conflicting replay enqueue count = %d, want 1", got)
		}
		assertSubmissionIntegrationCounts(t, pool, username, 1, 1)
		assertSubmissionIntegrationEpisodes(
			t, store, username, subscriptionID, episodes,
			subscription.SubscriptionEpisodeAcquired, storedID,
		)
		if _, err := store.GetDownloadIntent(
			t.Context(), username, conflictingIntent.ID,
		); !errors.Is(err, database.ErrDownloadIntentNotFound) {
			t.Fatalf("conflicting intent was not rolled back: %v", err)
		}
	})

	t.Run("unbound acquired replay rolls back intent and plan", func(t *testing.T) {
		store, pool := newSubmissionIntegrationStore(t)
		const username = "submission-unbound-owner"
		insertSubmissionIntegrationUser(t, pool, username)
		subscriptionID := createSubmissionIntegrationSubscription(t, store, username, "unbound-replay")
		episodes := []subscription.Episode{{Season: 3, Episode: 4}}
		if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, episodes); err != nil {
			t.Fatalf("initialize unbound episode: %v", err)
		}
		if err := store.CompareAndSwapSubscriptionPreciseSearchState(
			t.Context(), username, subscriptionID, subscription.Acquired,
		); err != nil {
			t.Fatalf("prepare acquired subscription: %v", err)
		}

		siteID := insertSubmissionIntegrationSite(t, pool, "submission-unbound-site")
		_, intent, plan := submissionIntegrationDownload(t, siteID, "301", "/downloads/unbound")
		enqueuer := &submissionIntegrationEnqueuer{}
		service, err := NewSubmissionService(pool, store, enqueuer)
		if err != nil {
			t.Fatal(err)
		}
		gotID, err := service.SubmitSubscriptionEpisodes(
			t.Context(), username, subscriptionID, intent, plan,
			submissionIntegrationSuccessInput(username, intent.ID, episodes), episodes,
		)
		if gotID != uuid.Nil || !errors.Is(err, ErrDownloadSubmission) {
			t.Fatalf("unbound replay = (%s, %v), want nil ID and ErrDownloadSubmission", gotID, err)
		}
		if got := enqueuer.Count(); got != 0 {
			t.Fatalf("unbound replay enqueue count = %d, want 0", got)
		}
		assertSubmissionIntegrationCounts(t, pool, username, 0, 0)
		assertSubmissionIntegrationEpisodes(
			t, store, username, subscriptionID, episodes,
			subscription.SubscriptionEpisodeMissing, uuid.Nil,
		)
	})
}

type submissionIntegrationEnqueuer struct {
	mu    sync.Mutex
	count int
}

func (e *submissionIntegrationEnqueuer) EnqueueDownloadTx(
	context.Context,
	pgx.Tx,
	jobs.DownloadArgs,
) error {
	e.mu.Lock()
	e.count++
	e.mu.Unlock()
	return nil
}

func (e *submissionIntegrationEnqueuer) Count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count
}

func newSubmissionIntegrationStore(t *testing.T) (*database.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv(submissionIntegrationDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("%s is not configured", submissionIntegrationDatabaseEnv)
	}
	setupCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", submissionIntegrationDatabaseEnv, err)
	}
	adminConfig.MaxConns = 2
	adminConfig.MinConns = 0
	adminConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-downloadflow-submission-integration-admin"
	adminPool, err := pgxpool.NewWithConfig(setupCtx, adminConfig)
	if err != nil {
		t.Fatalf("create PostgreSQL admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if err := adminPool.Ping(setupCtx); err != nil {
		t.Fatalf("connect PostgreSQL integration database: %v", err)
	}

	schema := submissionIntegrationSchema + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(setupCtx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create isolated schema %q: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("drop isolated schema %q: %v", schema, err)
		}
	})

	migrationConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse migration connection: %v", err)
	}
	migrationConfig.RuntimeParams["search_path"] = schema
	migrationConfig.RuntimeParams["application_name"] = "moviepilot-downloadflow-submission-integration-migrate"
	migrationDSN := stdlib.RegisterConnConfig(migrationConfig)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(migrationDSN) })
	if err := database.Migrate(setupCtx, appconfig.Config{DatabaseURL: migrationDSN}); err != nil {
		t.Fatalf("migrate isolated schema %q: %v", schema, err)
	}

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse store pool: %v", err)
	}
	poolConfig.MaxConns = 8
	poolConfig.MinConns = 0
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-downloadflow-submission-integration-store"
	storePool, err := pgxpool.NewWithConfig(setupCtx, poolConfig)
	if err != nil {
		t.Fatalf("create isolated store pool: %v", err)
	}
	t.Cleanup(storePool.Close)
	if err := storePool.Ping(setupCtx); err != nil {
		t.Fatalf("connect isolated schema %q: %v", schema, err)
	}
	var currentSchema string
	if err := storePool.QueryRow(setupCtx, "SELECT current_schema()").Scan(&currentSchema); err != nil {
		t.Fatalf("read current schema: %v", err)
	}
	if currentSchema != schema {
		t.Fatalf("current schema = %q, want %q", currentSchema, schema)
	}

	secretBox, err := auth.NewSecretBox("downloadflow-submission-integration-master-key")
	if err != nil {
		t.Fatalf("create integration secret box: %v", err)
	}
	return database.NewStore(&database.DB{Pool: storePool}, secretBox), storePool
}

func insertSubmissionIntegrationUser(t *testing.T, pool *pgxpool.Pool, username string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO users (username, password_hash)
		VALUES ($1, 'submission-integration-password-hash')`, username); err != nil {
		t.Fatalf("insert integration user %q: %v", username, err)
	}
}

func createSubmissionIntegrationSubscription(
	t *testing.T,
	store *database.Store,
	username string,
	mediaID string,
) int64 {
	t.Helper()
	id, err := store.CreateSubscription(t.Context(), database.NewSubscription{
		Username: username, Name: mediaID, MediaType: "tv", Year: "2026",
		MediaSource: "tmdb", MediaID: mediaID, State: subscription.PendingSearch,
		Config: map[string]any{"fixture": mediaID},
	})
	if err != nil {
		t.Fatalf("create integration subscription: %v", err)
	}
	return id
}

func submissionIntegrationDownload(
	t *testing.T,
	siteID int64,
	resourceID string,
	savePath string,
) (downloader.Candidate, downloader.Intent, database.DownloadIntentPlan) {
	t.Helper()
	candidate, err := (SourceReference{
		SiteID: siteID, ResolverKind: "fixture", ResolverVersion: 1,
		ResourceID: resourceID, DownloaderID: submissionIntegrationDownloader,
	}).Candidate(savePath, false)
	if err != nil {
		t.Fatalf("build integration candidate: %v", err)
	}
	intent, err := downloader.NewIntent(submissionIntegrationOwner, candidate)
	if err != nil {
		t.Fatalf("build integration intent: %v", err)
	}
	plan := database.DownloadIntentPlan{
		IntentID: intent.ID, SiteID: siteID, DownloaderID: submissionIntegrationDownloader,
		ResolverKind: "fixture", ResolverVersion: 1, ResourceID: resourceID,
		SavePath: savePath,
	}
	return candidate, intent, plan
}

func submissionIntegrationSuccessInput(
	username string,
	intentID uuid.UUID,
	episodes []subscription.Episode,
) organization.SuccessInput {
	input := submissionTestSuccessInput(username, intentID)
	input.Episodes = make([]organization.Episode, len(episodes))
	for index, episode := range episodes {
		input.Episodes[index] = organization.Episode{
			Season:  episode.Season,
			Episode: episode.Episode,
		}
	}
	return input
}

func insertSubmissionIntegrationSite(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var siteID int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, url, adapter)
		VALUES ($1, 'https://submission.invalid', 'fixture')
		RETURNING id`, name).Scan(&siteID); err != nil {
		t.Fatalf("insert integration site: %v", err)
	}
	return siteID
}

func assertSubmissionIntegrationCounts(
	t *testing.T,
	pool *pgxpool.Pool,
	username string,
	wantIntents int,
	wantPlans int,
) {
	t.Helper()
	var intentCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM download_intents WHERE username=$1`, username,
	).Scan(&intentCount); err != nil {
		t.Fatalf("count download intents: %v", err)
	}
	var planCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM download_intent_plans AS p
		JOIN download_intents AS di ON di.id=p.intent_id
		WHERE di.username=$1`, username,
	).Scan(&planCount); err != nil {
		t.Fatalf("count download intent plans: %v", err)
	}
	var successInputCount int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM download_intent_success_inputs AS s
		JOIN download_intents AS di ON di.id=s.intent_id
		WHERE di.username=$1`, username,
	).Scan(&successInputCount); err != nil {
		t.Fatalf("count download intent success inputs: %v", err)
	}
	if intentCount != wantIntents || planCount != wantPlans || successInputCount != wantIntents {
		t.Fatalf(
			"intent/plan/success-input counts = %d/%d/%d, want %d/%d/%d",
			intentCount, planCount, successInputCount, wantIntents, wantPlans, wantIntents,
		)
	}
}

func assertSubmissionIntegrationEpisodes(
	t *testing.T,
	store *database.Store,
	username string,
	subscriptionID int64,
	wantEpisodes []subscription.Episode,
	wantState subscription.SubscriptionEpisodeState,
	wantIntentID uuid.UUID,
) {
	t.Helper()
	items, err := store.ListSubscriptionEpisodes(
		t.Context(), username, subscriptionID,
		[]subscription.SubscriptionEpisodeState{
			subscription.SubscriptionEpisodeMissing,
			subscription.SubscriptionEpisodeAcquired,
			subscription.SubscriptionEpisodeDownloading,
			subscription.SubscriptionEpisodePendingOrganization,
			subscription.SubscriptionEpisodeOrganizing,
			subscription.SubscriptionEpisodeIndexed,
		},
		10_000,
	)
	if err != nil {
		t.Fatalf("list subscription episodes: %v", err)
	}
	if len(items) != len(wantEpisodes) {
		t.Fatalf("episode count = %d, want %d; items=%#v", len(items), len(wantEpisodes), items)
	}
	for index, item := range items {
		want := wantEpisodes[index]
		if item.Season != want.Season || item.Episode != want.Episode || item.State != wantState {
			t.Fatalf("episode[%d] = S%dE%d/%s, want S%dE%d/%s", index, item.Season, item.Episode, item.State, want.Season, want.Episode, wantState)
		}
		if wantIntentID == uuid.Nil {
			if item.DownloadIntentID != nil {
				t.Fatalf("episode[%d] binding = %s, want NULL", index, *item.DownloadIntentID)
			}
		} else if item.DownloadIntentID == nil || *item.DownloadIntentID != wantIntentID {
			t.Fatalf("episode[%d] binding = %v, want %s", index, item.DownloadIntentID, wantIntentID)
		}
	}
}
