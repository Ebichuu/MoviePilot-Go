//go:build integration

package database

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestDownloadReconciliationPostgresKeysetAndStateFilter(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "download-reconcile-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "download-reconcile-site")

	cutoff := time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)
	type fixture struct {
		key       string
		state     downloader.IntentState
		updatedAt time.Time
	}
	fixtures := []fixture{
		{key: "planned", state: downloader.IntentPlanned, updatedAt: cutoff.Add(-4 * time.Minute)},
		{key: "submitting", state: downloader.IntentSubmitting, updatedAt: cutoff.Add(-3 * time.Minute)},
		{key: "downloading", state: downloader.IntentDownloading, updatedAt: cutoff.Add(-2 * time.Minute)},
		{key: "completed", state: downloader.IntentCompleted, updatedAt: cutoff.Add(-time.Minute)},
	}
	targets := make(map[string]downloader.Intent)
	for _, fixture := range fixtures {
		intent := mustDatabaseDownloadIntent(t, username, "reconciliation-"+fixture.key)
		if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
			t.Fatalf("创建 %s 意图: %v", fixture.key, err)
		}
		plan := DownloadIntentPlan{
			IntentID: intent.ID, SiteID: siteID,
			DownloaderID: "2cf24282-78e7-4b32-9f07-761efc29280c",
			ResolverKind: "fixture", ResolverVersion: 1,
			ResourceID: fixture.key, SavePath: "/downloads",
		}
		if _, err := store.CreateDownloadIntentPlan(t.Context(), username, plan); err != nil {
			t.Fatalf("创建 %s 执行计划: %v", fixture.key, err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE download_intents
			SET state=$1, torrent_hash=$2, updated_at=$3
			WHERE id=$4`,
			fixture.state,
			databaseIntentV1Hash,
			fixture.updatedAt,
			intent.ID,
		); err != nil {
			t.Fatalf("准备 %s 状态: %v", fixture.key, err)
		}
		intent.State = fixture.state
		intent.TorrentHash = databaseIntentV1Hash
		targets[fixture.key] = intent
	}

	first, err := store.ListDownloadReconciliationTargets(t.Context(), cutoff, time.Time{}, uuid.Nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != targets["submitting"].ID || first[0].State != downloader.IntentSubmitting {
		t.Fatalf("first page = %#v", first)
	}
	second, err := store.ListDownloadReconciliationTargets(
		t.Context(), cutoff, first[0].UpdatedAt, first[0].ID, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID != targets["downloading"].ID ||
		second[0].State != downloader.IntentDownloading || second[1].ID != targets["completed"].ID ||
		second[1].State != downloader.IntentCompleted {
		t.Fatalf("second page = %#v", second)
	}
}

func TestDownloadReconciliationPostgresFailedHashPolicy(t *testing.T) {
	t.Run("empty hash can be listed and recovered", func(t *testing.T) {
		store := newDownloadIntentPlanIntegrationStore(t)
		const username = "failed-empty-hash-reconciliation-owner"
		insertSubscriptionIntegrationUsers(t, store, username)
		siteID := insertDownloadIntentPlanIntegrationSite(t, store, "failed-empty-hash-reconciliation-site")
		fixture := prepareSubscriptionDownloadRecoveryFixture(
			t,
			store,
			username,
			"failed-empty-hash-reconciliation",
			downloader.IntentFailed,
			SubscriptionEpisodeDownloading,
			subscription.Downloading,
		)
		if _, err := store.CreateDownloadIntentPlan(
			t.Context(),
			username,
			downloadIntentPlanIntegrationFixture(fixture.intentID, siteID, "failed-empty-hash-reconciliation"),
		); err != nil {
			t.Fatalf("create failed intent plan: %v", err)
		}

		targets, err := store.ListDownloadReconciliationTargets(
			t.Context(), fixture.intentUpdatedAt.Add(time.Second), time.Time{}, uuid.Nil, 10,
		)
		if err != nil {
			t.Fatalf("list failed intent with empty hash: %v", err)
		}
		if len(targets) != 1 || targets[0].ID != fixture.intentID ||
			targets[0].State != downloader.IntentFailed || targets[0].TorrentHash != "" {
			t.Fatalf("failed reconciliation targets = %#v", targets)
		}

		if err := store.RecoverSubscriptionDownloadIntent(
			t.Context(), username, fixture.intentID,
			[]downloader.IntentState{downloader.IntentFailed}, &targets[0].UpdatedAt,
		); err != nil {
			t.Fatalf("recover failed intent with empty hash: %v", err)
		}
		assertSubscriptionDownloadRecoveryState(
			t,
			store,
			fixture,
			downloader.IntentFailed,
			SubscriptionEpisodeMissing,
			uuid.Nil,
			subscription.PendingSearch,
			"N",
		)
	})

	t.Run("corrupt non-empty hash rejects page", func(t *testing.T) {
		store := newDownloadIntentPlanIntegrationStore(t)
		const username = "failed-corrupt-hash-reconciliation-owner"
		insertSubscriptionIntegrationUsers(t, store, username)
		siteID := insertDownloadIntentPlanIntegrationSite(t, store, "failed-corrupt-hash-reconciliation-site")
		fixture := prepareSubscriptionDownloadRecoveryFixture(
			t,
			store,
			username,
			"failed-corrupt-hash-reconciliation",
			downloader.IntentFailed,
			SubscriptionEpisodeAcquired,
			subscription.Acquired,
		)
		if _, err := store.CreateDownloadIntentPlan(
			t.Context(),
			username,
			downloadIntentPlanIntegrationFixture(fixture.intentID, siteID, "failed-corrupt-hash-reconciliation"),
		); err != nil {
			t.Fatalf("create failed intent plan: %v", err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE download_intents SET torrent_hash='not-a-hash' WHERE id=$1`,
			fixture.intentID,
		); err != nil {
			t.Fatalf("corrupt failed intent hash: %v", err)
		}

		_, err := store.ListDownloadReconciliationTargets(
			t.Context(), fixture.intentUpdatedAt.Add(time.Second), time.Time{}, uuid.Nil, 10,
		)
		if !errors.Is(err, ErrDownloadReconciliationQuery) {
			t.Fatalf("corrupt failed intent list error = %v, want ErrDownloadReconciliationQuery", err)
		}
	})
}

func TestDownloadReconciliationPostgresPlannedIntentCanBeListedAndRecovered(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "planned-empty-hash-reconciliation-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "planned-empty-hash-reconciliation-site")
	fixture := prepareSubscriptionDownloadRecoveryFixture(
		t,
		store,
		username,
		"planned-empty-hash-reconciliation",
		downloader.IntentPlanned,
		SubscriptionEpisodeAcquired,
		subscription.Acquired,
	)
	if _, err := store.CreateDownloadIntentPlan(
		t.Context(),
		username,
		downloadIntentPlanIntegrationFixture(fixture.intentID, siteID, "planned-empty-hash-reconciliation"),
	); err != nil {
		t.Fatalf("create planned intent plan: %v", err)
	}

	targets, err := store.ListDownloadReconciliationTargets(
		t.Context(), fixture.intentUpdatedAt.Add(time.Second), time.Time{}, uuid.Nil, 10,
	)
	if err != nil {
		t.Fatalf("list planned intent with empty hash: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != fixture.intentID ||
		targets[0].State != downloader.IntentPlanned || targets[0].TorrentHash != "" {
		t.Fatalf("planned reconciliation targets = %#v", targets)
	}

	if err := store.RecoverSubscriptionDownloadIntent(
		t.Context(), username, fixture.intentID,
		[]downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed},
		&targets[0].UpdatedAt,
	); err != nil {
		t.Fatalf("recover planned intent with empty hash: %v", err)
	}
	assertSubscriptionDownloadRecoveryState(
		t,
		store,
		fixture,
		downloader.IntentFailed,
		SubscriptionEpisodeMissing,
		uuid.Nil,
		subscription.PendingSearch,
		"N",
	)
}

func TestDownloadReconciliationPostgresRecoversCompletedSubscriptionState(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "completed-recovery-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "completed-recovery-site")
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "completed-recovery")
	episode := subscription.Episode{Season: 1, Episode: 1}
	if err := store.UpsertSubscriptionEpisodes(t.Context(), username, subscriptionID, []subscription.Episode{episode}); err != nil {
		t.Fatalf("initialize episode: %v", err)
	}
	intent := mustDatabaseDownloadIntent(t, username, "completed-recovery-intent")
	stored, err := store.CreateDownloadIntent(t.Context(), username, intent)
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}
	if _, err := store.CreateDownloadIntentPlan(t.Context(), username, DownloadIntentPlan{
		IntentID: stored.ID, SiteID: siteID,
		DownloaderID: "2cf24282-78e7-4b32-9f07-761efc29280c",
		ResolverKind: "fixture", ResolverVersion: 1, ResourceID: "completed-recovery",
	}); err != nil {
		t.Fatalf("create intent plan: %v", err)
	}
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindDownloadIntentSubscriptionTx(t.Context(), tx, username, stored.ID, subscriptionID); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatalf("bind intent: %v", err)
	}
	if err := store.BindSubscriptionEpisodesTx(
		t.Context(), tx, username, subscriptionID, stored.ID, []subscription.Episode{episode},
	); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatalf("bind episode: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(), tx, username, subscriptionID, subscription.Acquired,
	); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatalf("acquire subscription: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), username, stored.ID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeAcquired},
		SubscriptionEpisodeDownloading,
	); err != nil {
		t.Fatalf("advance episode to downloading: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(), username, stored.ID, subscription.Acquired, subscription.Downloading,
	); err != nil {
		t.Fatalf("advance parent to downloading: %v", err)
	}

	cutoff := time.Now().UTC()
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET state='completed', torrent_hash=$1, updated_at=$2
		WHERE id=$3`, databaseIntentV1Hash, cutoff.Add(-time.Minute), stored.ID); err != nil {
		t.Fatalf("prepare completed intent: %v", err)
	}
	assertCompletedRecoveryTarget(t, store, cutoff, stored.ID, true)

	if _, err := store.TransitionSubscriptionEpisodesByIntent(
		t.Context(), username, stored.ID,
		[]SubscriptionEpisodeState{SubscriptionEpisodeDownloading},
		SubscriptionEpisodePendingOrganization,
	); err != nil {
		t.Fatalf("repair episode: %v", err)
	}
	assertCompletedRecoveryTarget(t, store, cutoff, stored.ID, true)

	if err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(), username, stored.ID, subscription.Downloading, subscription.PendingOrganization,
	); err != nil {
		t.Fatalf("repair parent: %v", err)
	}
	assertCompletedRecoveryTarget(t, store, cutoff, stored.ID, true)

	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R'
		WHERE id=$2 AND username=$3`, subscription.Organizing, subscriptionID, username); err != nil {
		t.Fatalf("advance parent to organizing: %v", err)
	}
	assertCompletedRecoveryTarget(t, store, cutoff, stored.ID, true)

	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R'
		WHERE id=$2 AND username=$3`, subscription.Indexed, subscriptionID, username); err != nil {
		t.Fatalf("advance parent to indexed: %v", err)
	}
	assertCompletedRecoveryTarget(t, store, cutoff, stored.ID, true)

	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='S'
		WHERE id=$2 AND username=$3`, subscription.Paused, subscriptionID, username); err != nil {
		t.Fatalf("pause parent: %v", err)
	}
	assertCompletedRecoveryTarget(t, store, cutoff, stored.ID, true)

	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state=$1, v2_state='R'
		WHERE id=$2 AND username=$3`, subscription.PendingOrganization, subscriptionID, username); err != nil {
		t.Fatalf("restore parent for organization batch: %v", err)
	}
	root := t.TempDir()
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(
			filepath.Join(root, "downloads", "episode.mkv"),
			filepath.Join(root, "library", "episode.mkv"),
			organizer.Copy,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrganizationBatch(
		t.Context(), username, stored.ID, filepath.Join(root, "downloads"), plan,
		organizationLegacySuccessSnapshotsIntegration(t, store, username, stored.ID, plan),
	); err != nil {
		t.Fatalf("create organization batch: %v", err)
	}
	assertCompletedRecoveryTarget(t, store, cutoff, stored.ID, false)
}

func TestDownloadReconciliationPostgresCompletedPendingOrganizationKeyset(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const (
		username      = "completed-pending-keyset-owner"
		otherUsername = "completed-pending-keyset-other"
	)
	insertDownloadIntentIntegrationUsers(t, store, username, otherUsername)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "completed-pending-keyset-site")

	cutoff := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	updatedAt := cutoff.Add(-time.Minute)
	intentIDs := make([]uuid.UUID, 0, 2)
	for _, key := range []string{"one", "two"} {
		subscriptionID := createSubscriptionIntegrationFixture(
			t, store, username, "completed-pending-keyset-"+key,
		)
		intent := mustDatabaseDownloadIntent(
			t, username, "completed-pending-keyset-intent-"+key,
		)
		if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
			t.Fatalf("create pending organization intent %s: %v", key, err)
		}
		if _, err := store.CreateDownloadIntentPlan(
			t.Context(), username,
			downloadIntentPlanIntegrationFixture(intent.ID, siteID, intent.ID.String()),
		); err != nil {
			t.Fatalf("create pending organization plan %s: %v", key, err)
		}
		tx, err := store.db.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := store.BindDownloadIntentSubscriptionTx(
			t.Context(), tx, username, intent.ID, subscriptionID,
		); err != nil {
			_ = tx.Rollback(t.Context())
			t.Fatalf("bind pending organization intent %s: %v", key, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE subscriptions
			SET internal_state=$1, v2_state='R'
			WHERE id=$2 AND username=$3`,
			subscription.PendingOrganization, subscriptionID, username,
		); err != nil {
			t.Fatalf("prepare pending organization parent %s: %v", key, err)
		}
		if _, err := store.db.Pool.Exec(t.Context(), `
			UPDATE download_intents
			SET state='completed', torrent_hash=$1, updated_at=$2
			WHERE id=$3 AND username=$4`,
			databaseIntentV1Hash, updatedAt, intent.ID, username,
		); err != nil {
			t.Fatalf("prepare completed intent %s: %v", key, err)
		}
		intentIDs = append(intentIDs, intent.ID)
	}
	if intentIDs[1].String() < intentIDs[0].String() {
		intentIDs[0], intentIDs[1] = intentIDs[1], intentIDs[0]
	}

	otherSubscriptionID := createSubscriptionIntegrationFixture(
		t, store, otherUsername, "completed-pending-keyset-cross-owner",
	)
	crossOwnerIntent := mustDatabaseDownloadIntent(t, username, "completed-pending-keyset-cross-owner-intent")
	if _, err := store.CreateDownloadIntent(t.Context(), username, crossOwnerIntent); err != nil {
		t.Fatalf("create cross-owner intent: %v", err)
	}
	if _, err := store.CreateDownloadIntentPlan(
		t.Context(), username,
		downloadIntentPlanIntegrationFixture(crossOwnerIntent.ID, siteID, crossOwnerIntent.ID.String()),
	); err != nil {
		t.Fatalf("create cross-owner plan: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET subscription_id=$1, state='completed', torrent_hash=$2, updated_at=$3
		WHERE id=$4 AND username=$5`,
		otherSubscriptionID, databaseIntentV1Hash, updatedAt, crossOwnerIntent.ID, username,
	); err != nil {
		t.Fatalf("prepare cross-owner completed intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state=$2, v2_state='R'
		WHERE id=$3 AND username=$4`,
		crossOwnerIntent.ID, subscription.PendingOrganization, otherSubscriptionID, otherUsername,
	); err != nil {
		t.Fatalf("prepare cross-owner parent: %v", err)
	}

	first, err := store.ListDownloadReconciliationTargets(
		t.Context(), cutoff, time.Time{}, uuid.Nil, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != intentIDs[0] || first[0].State != downloader.IntentCompleted {
		t.Fatalf("first completed pending page = %#v, want %s", first, intentIDs[0])
	}
	second, err := store.ListDownloadReconciliationTargets(
		t.Context(), cutoff, first[0].UpdatedAt, first[0].ID, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != intentIDs[1] || second[0].State != downloader.IntentCompleted {
		t.Fatalf("second completed pending page = %#v, want %s", second, intentIDs[1])
	}
	third, err := store.ListDownloadReconciliationTargets(
		t.Context(), cutoff, second[0].UpdatedAt, second[0].ID, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("completed pending pagination repeated or crossed owner boundary: %#v", third)
	}
}

func TestDownloadReconciliationPostgresOrganizationPendingTrigger(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "completed-pending-trigger-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "completed-pending-trigger-site")
	intent := mustDatabaseDownloadIntent(t, username, "completed-pending-trigger-intent")
	if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
		t.Fatalf("create completed pending intent: %v", err)
	}
	if _, err := store.CreateDownloadIntentPlan(
		t.Context(), username,
		downloadIntentPlanIntegrationFixture(intent.ID, siteID, "completed-pending-trigger"),
	); err != nil {
		t.Fatalf("create completed pending plan: %v", err)
	}

	cutoff := time.Now().UTC()
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET state='completed', torrent_hash=$1, updated_at=$2
		WHERE id=$3`, databaseIntentV1Hash, cutoff.Add(-time.Minute), intent.ID); err != nil {
		t.Fatalf("prepare completed pending intent: %v", err)
	}
	assertOrganizationReconciliationPending(t, store, intent.ID, true)
	assertCompletedRecoveryTarget(t, store, cutoff, intent.ID, true)

	batchID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO organization_batches (
			id, intent_id, state, source_root, target_root, plan
		) VALUES ($1, $2, 'planned', '/source', '/target', '[{}]'::jsonb)`,
		batchID, intent.ID); err != nil {
		t.Fatalf("insert organization batch: %v", err)
	}
	assertOrganizationReconciliationPending(t, store, intent.ID, false)
	assertCompletedRecoveryTarget(t, store, cutoff, intent.ID, false)

	command, err := store.db.Pool.Exec(
		t.Context(), `DELETE FROM organization_batches WHERE id=$1`, batchID,
	)
	if err != nil || command.RowsAffected() != 1 {
		t.Fatalf("delete organization batch: rows=%d error=%v", command.RowsAffected(), err)
	}
	assertOrganizationReconciliationPending(t, store, intent.ID, true)
	assertCompletedRecoveryTarget(t, store, cutoff, intent.ID, true)
}

func TestDownloadReconciliationPendingMigrationPostgresRoundTrip(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("read integration schema: %v", err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 28); err != nil {
		t.Fatalf("downgrade reconciliation migrations: %v", err)
	}

	const username = "reconciliation-pending-migration-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	orphanID := uuid.New()
	batchedID := uuid.New()
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO download_intents (id, username, candidate_key, label, state)
		VALUES
			($1, $3, 'reconciliation-migration-orphan', $4, 'completed'),
			($2, $3, 'reconciliation-migration-batched', $5, 'completed')`,
		orphanID, batchedID, username, "mp-"+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatalf("insert pre-migration intents: %v", err)
	}
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO organization_batches (
			id, intent_id, state, source_root, target_root, plan
		) VALUES ($1, $2, 'planned', '/source', '/target', '[{}]'::jsonb)`,
		uuid.New(), batchedID); err != nil {
		t.Fatalf("insert pre-migration organization batch: %v", err)
	}

	if err := goose.UpToContext(ctx, migrationDB, "migrations", 30); err != nil {
		t.Fatalf("upgrade reconciliation migrations: %v", err)
	}
	assertOrganizationReconciliationPending(t, store, orphanID, true)
	assertOrganizationReconciliationPending(t, store, batchedID, false)
	assertDownloadReconciliationCandidateIndexReady(t, store)

	if err := goose.DownToContext(ctx, migrationDB, "migrations", 28); err != nil {
		t.Fatalf("second downgrade reconciliation migrations: %v", err)
	}
	var columnExists bool
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema=current_schema()
				AND table_name='download_intents'
				AND column_name='organization_reconciliation_pending'
		)`).Scan(&columnExists); err != nil {
		t.Fatalf("check pending column after downgrade: %v", err)
	}
	if columnExists {
		t.Fatal("organization reconciliation pending column survived downgrade")
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 30); err != nil {
		t.Fatalf("second upgrade reconciliation migrations: %v", err)
	}
	assertOrganizationReconciliationPending(t, store, orphanID, true)
	assertOrganizationReconciliationPending(t, store, batchedID, false)
	assertDownloadReconciliationCandidateIndexReady(t, store)
}

func TestDownloadReconciliationPlannedMigrationPostgresRoundTrip(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("read integration schema: %v", err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}

	if err := goose.DownToContext(ctx, migrationDB, "migrations", 30); err != nil {
		t.Fatalf("downgrade planned reconciliation migration: %v", err)
	}
	assertDownloadReconciliationCandidateIndexPredicate(t, store, false)
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 31); err != nil {
		t.Fatalf("upgrade planned reconciliation migration: %v", err)
	}
	assertDownloadReconciliationCandidateIndexPredicate(t, store, true)
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 30); err != nil {
		t.Fatalf("second downgrade planned reconciliation migration: %v", err)
	}
	assertDownloadReconciliationCandidateIndexPredicate(t, store, false)
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 31); err != nil {
		t.Fatalf("second upgrade planned reconciliation migration: %v", err)
	}
	assertDownloadReconciliationCandidateIndexPredicate(t, store, true)
}

func TestDownloadReconciliationPostgresPlanAvoidsCompletedHistoryScan(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const (
		username     = "reconciliation-plan-owner"
		historyCount = 5000
		activeCount  = 200
	)
	insertDownloadIntentIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "reconciliation-plan-site")

	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (
			id, username, candidate_key, torrent_hash, label, state, updated_at
		)
		SELECT md5('reconciliation-history-' || fixture.id::text)::uuid,
			$1, 'reconciliation-history-' || fixture.id::text, repeat('a', 40),
			'mp-reconciliation-history-' || fixture.id::text, 'completed',
			TIMESTAMPTZ '2020-01-01 00:00:00+00' + fixture.id * INTERVAL '1 second'
		FROM generate_series(1, $2) AS fixture(id)`, username, historyCount); err != nil {
		t.Fatalf("insert completed reconciliation history: %v", err)
	}
	orphanID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (
			id, username, candidate_key, torrent_hash, label, state, updated_at
		) VALUES ($1, $2, 'reconciliation-plan-orphan', repeat('a', 40), $3,
			'completed', TIMESTAMPTZ '2029-01-01 00:00:00+00')`,
		orphanID, username, "mp-"+uuid.NewString()); err != nil {
		t.Fatalf("insert completed reconciliation orphan: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (
			id, username, candidate_key, torrent_hash, label, state, updated_at
		)
		SELECT md5('reconciliation-active-' || fixture.id::text)::uuid,
			$1, 'reconciliation-active-' || fixture.id::text, repeat('b', 40),
			'mp-reconciliation-active-' || fixture.id::text, 'downloading',
			TIMESTAMPTZ '2030-01-01 00:00:00+00' + fixture.id * INTERVAL '1 second'
		FROM generate_series(1, $2) AS fixture(id)`, username, activeCount); err != nil {
		t.Fatalf("insert active reconciliation candidates: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intent_plans (
			intent_id, site_id, downloader_id, resolver_kind, resolver_version,
			resource_id, resource_secret, save_path, paused
		)
		SELECT id, $2, $3, 'fixture', 1, 'plan-' || replace(id::text, '-', ''), '', '', FALSE
		FROM download_intents
		WHERE username=$1`, username, siteID, downloadIntentPlanTestDownloaderID); err != nil {
		t.Fatalf("insert reconciliation plans: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO organization_batches (
			id, intent_id, state, source_root, target_root, plan
		)
		SELECT md5('reconciliation-batch-' || id::text)::uuid,
			id, 'planned', '/source', '/target', '[{}]'::jsonb
		FROM download_intents
		WHERE username=$1 AND candidate_key LIKE 'reconciliation-history-%'`, username); err != nil {
		t.Fatalf("insert organization batch history: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		ANALYZE download_intents;
		ANALYZE download_intent_plans;
		ANALYZE subscriptions;
		ANALYZE organization_batches`); err != nil {
		t.Fatalf("analyze reconciliation plan fixtures: %v", err)
	}

	var encoded []byte
	if err := store.db.Pool.QueryRow(
		t.Context(),
		"EXPLAIN (ANALYZE, FORMAT JSON, COSTS FALSE, TIMING FALSE, SUMMARY FALSE) "+
			listDownloadReconciliationTargetsSQL,
		time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}, uuid.Nil, activeCount,
	).Scan(&encoded); err != nil {
		t.Fatalf("explain reconciliation query: %v", err)
	}
	var envelopes []downloadReconciliationExplainEnvelope
	if err := json.Unmarshal(encoded, &envelopes); err != nil || len(envelopes) != 1 {
		t.Fatalf("decode reconciliation plan: envelopes=%d error=%v plan=%s", len(envelopes), err, encoded)
	}
	assertDownloadReconciliationPlanBounded(t, envelopes[0].Plan)
}

func TestDownloadReconciliationPostgresSelectsHistoricalCompletedIntentWithoutTouchingNewCurrent(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "completed-history-owner"
	insertDownloadIntentIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "completed-history")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "completed-history-site")

	oldIntent := mustDatabaseDownloadIntent(t, username, "completed-history-old")
	newIntent := mustDatabaseDownloadIntent(t, username, "completed-history-new")
	for _, intent := range []downloader.Intent{oldIntent, newIntent} {
		if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
			t.Fatalf("create intent %s: %v", intent.ID, err)
		}
		if _, err := store.CreateDownloadIntentPlan(
			t.Context(), username,
			downloadIntentPlanIntegrationFixture(intent.ID, siteID, intent.ID.String()),
		); err != nil {
			t.Fatalf("create intent plan %s: %v", intent.ID, err)
		}
	}
	tx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := store.BindDownloadIntentSubscriptionTx(
		t.Context(), tx, username, oldIntent.ID, subscriptionID,
	); err != nil {
		t.Fatalf("bind old intent: %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchStateTx(
		t.Context(), tx, username, subscriptionID, subscription.Acquired,
	); err != nil {
		t.Fatalf("acquire subscription: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().UTC()
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET state='completed', torrent_hash=$1, updated_at=$2
		WHERE id=$3`,
		databaseIntentV1Hash, cutoff.Add(-time.Minute), oldIntent.ID,
	); err != nil {
		t.Fatalf("complete historical intent: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET subscription_id=$1
		WHERE id=$2`, subscriptionID, newIntent.ID,
	); err != nil {
		t.Fatalf("bind current intent fixture: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1
		WHERE id=$2`, newIntent.ID, subscriptionID,
	); err != nil {
		t.Fatalf("switch current intent fixture: %v", err)
	}

	targets, err := store.ListDownloadReconciliationTargets(
		t.Context(), cutoff, time.Time{}, uuid.Nil, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, target := range targets {
		if target.ID == oldIntent.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("historical completed intent not selected: %#v", targets)
	}
	if err := store.CompareAndSwapSubscriptionStateByIntent(
		t.Context(), username, oldIntent.ID,
		subscription.Acquired, subscription.Downloading,
	); !errors.Is(err, ErrSubscriptionDownloadStateNotFound) {
		t.Fatalf("historical intent parent CAS error = %v, want not found", err)
	}
	storedSubscription, err := store.GetActiveSubscription(t.Context(), username, subscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	if storedSubscription.InternalState != subscription.Acquired {
		t.Fatalf("historical intent advanced parent to %q", storedSubscription.InternalState)
	}
}

func assertCompletedRecoveryTarget(
	t *testing.T,
	store *Store,
	cutoff time.Time,
	intentID uuid.UUID,
	want bool,
) {
	t.Helper()
	targets, err := store.ListDownloadReconciliationTargets(t.Context(), cutoff, time.Time{}, uuid.Nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, target := range targets {
		if target.ID == intentID {
			found = true
			if target.State != downloader.IntentCompleted {
				t.Fatalf("recovery target state = %q", target.State)
			}
		}
	}
	if found != want {
		t.Fatalf("completed recovery target found=%v, want %v; targets=%#v", found, want, targets)
	}
}

type downloadReconciliationExplainEnvelope struct {
	Plan downloadReconciliationExplainNode `json:"Plan"`
}

type downloadReconciliationExplainNode struct {
	NodeType     string                              `json:"Node Type"`
	RelationName string                              `json:"Relation Name"`
	IndexName    string                              `json:"Index Name"`
	ActualLoops  float64                             `json:"Actual Loops"`
	Plans        []downloadReconciliationExplainNode `json:"Plans"`
}

func assertDownloadReconciliationPlanBounded(t *testing.T, root downloadReconciliationExplainNode) {
	t.Helper()
	var candidateIndexUsed, batchIndexUsed bool
	var visit func(downloadReconciliationExplainNode)
	visit = func(node downloadReconciliationExplainNode) {
		if node.IndexName == "download_intents_reconciliation_candidates_idx" && node.ActualLoops > 0 {
			candidateIndexUsed = true
		}
		if node.IndexName == "organization_batches_intent_idx" && node.ActualLoops > 0 {
			batchIndexUsed = true
		}
		if node.NodeType == "Seq Scan" && node.ActualLoops > 0 &&
			(node.RelationName == "download_intents" || node.RelationName == "organization_batches") {
			t.Errorf("reconciliation plan executed historical sequential scan on %s", node.RelationName)
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	visit(root)
	if !candidateIndexUsed {
		t.Error("reconciliation candidate partial index was not executed")
	}
	if !batchIndexUsed {
		t.Error("organization batch intent index was not executed")
	}
}

func assertOrganizationReconciliationPending(t *testing.T, store *Store, intentID uuid.UUID, want bool) {
	t.Helper()
	var pending bool
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT organization_reconciliation_pending
		FROM download_intents
		WHERE id=$1`, intentID).Scan(&pending); err != nil {
		t.Fatalf("read organization reconciliation pending marker: %v", err)
	}
	if pending != want {
		t.Fatalf("organization reconciliation pending = %v, want %v", pending, want)
	}
}

func assertDownloadReconciliationCandidateIndexReady(t *testing.T, store *Store) {
	t.Helper()
	var valid, ready bool
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT index.indisvalid, index.indisready
		FROM pg_index AS index
		JOIN pg_class AS relation ON relation.oid=index.indexrelid
		JOIN pg_namespace AS namespace ON namespace.oid=relation.relnamespace
		WHERE namespace.nspname=current_schema()
			AND relation.relname='download_intents_reconciliation_candidates_idx'`,
	).Scan(&valid, &ready); err != nil {
		t.Fatalf("read reconciliation candidate index state: %v", err)
	}
	if !valid || !ready {
		t.Fatalf("reconciliation candidate index valid=%v ready=%v", valid, ready)
	}
}

func assertDownloadReconciliationCandidateIndexPredicate(t *testing.T, store *Store, wantPlanned bool) {
	t.Helper()
	var predicate string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT pg_get_expr(index.indpred, index.indrelid)
		FROM pg_index AS index
		JOIN pg_class AS relation ON relation.oid=index.indexrelid
		JOIN pg_namespace AS namespace ON namespace.oid=relation.relnamespace
		WHERE namespace.nspname=current_schema()
			AND relation.relname='download_intents_reconciliation_candidates_idx'`,
	).Scan(&predicate); err != nil {
		t.Fatalf("read reconciliation candidate index predicate: %v", err)
	}
	containsPlanned := strings.Contains(predicate, "'planned'")
	if containsPlanned != wantPlanned {
		t.Fatalf("reconciliation candidate predicate contains planned=%v, want %v: %s", containsPlanned, wantPlanned, predicate)
	}
	assertDownloadReconciliationCandidateIndexReady(t, store)
}
