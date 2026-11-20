//go:build integration

package organizationflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	appconfig "github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/database"
	"github.com/moviepilot-go/moviepilot-go/internal/jobs"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/organization"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

const (
	organizationProcessRecoveryDatabaseEnv = "MOVIEPILOT_TEST_DATABASE_URL"
	organizationProcessRecoveryChildEnv    = "MOVIEPILOT_ORGANIZATION_RECOVERY_CHILD"
)

func TestOrganizationWorkerProcessKillRecovery(t *testing.T) {
	if os.Getenv(organizationProcessRecoveryChildEnv) == "1" {
		runOrganizationRecoveryChild(t)
		return
	}

	store, pool, childDSN, childSchema := newOrganizationProcessRecoveryStore(t)
	fixture := createOrganizationProcessRecoveryFixture(t, store, pool)
	readyFile := filepath.Join(t.TempDir(), "child-ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestOrganizationWorkerProcessKillRecovery$", "-test.v")
	childOutput := filepath.Join(t.TempDir(), "child-output.log")
	outputFile, err := os.Create(childOutput)
	if err != nil {
		t.Fatalf("create child output file: %v", err)
	}
	defer outputFile.Close()
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile
	cmd.Env = append(os.Environ(),
		organizationProcessRecoveryChildEnv+"=1",
		"MOVIEPILOT_ORGANIZATION_RECOVERY_DSN="+childDSN,
		"MOVIEPILOT_ORGANIZATION_RECOVERY_SCHEMA="+childSchema,
		"MOVIEPILOT_ORGANIZATION_RECOVERY_BATCH_ID="+fixture.batchID.String(),
		"MOVIEPILOT_ORGANIZATION_RECOVERY_READY_FILE="+readyFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start organization child worker: %v", err)
	}
	childWait := make(chan error, 1)
	childDone := make(chan struct{})
	go func() {
		defer close(childDone)
		childWait <- cmd.Wait()
	}()
	t.Cleanup(func() {
		select {
		case <-childDone:
			return
		default:
		}
		_ = cmd.Process.Kill()
		select {
		case <-childDone:
		case <-time.After(5 * time.Second):
			t.Errorf("organization child did not exit during cleanup")
		}
	})
	waitForOrganizationRecoveryReady(t, readyFile, childOutput, childWait)

	assertOrganizationProcessState(
		t, pool, fixture,
		"organizing", "organizing", "organizing", "organizing", true, 1,
	)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill organization child worker: %v", err)
	}
	if err := <-childWait; err == nil {
		t.Fatal("killed organization child worker exited successfully")
	}
	assertOrganizationProcessState(
		t, pool, fixture,
		"organizing", "organizing", "organizing", "organizing", true, 1,
	)

	if _, err := pool.Exec(t.Context(), `
		UPDATE organization_batches
		SET lease_expires_at=now()-interval '1 second'
		WHERE id=$1`, fixture.batchID); err != nil {
		t.Fatalf("expire killed worker lease: %v", err)
	}
	recovered, err := store.RecoverExpiredOrganizationLeases(t.Context(), time.Now().UTC(), 1)
	if err != nil || recovered != 1 {
		t.Fatalf("recover killed worker lease = %d, %v; want 1", recovered, err)
	}
	assertOrganizationProcessState(
		t, pool, fixture,
		"failed", "failed", "pending_organization", "pending_organization", false, 1,
	)

	handler := newOrganizationProcessRecoveryHandler(t, store, nil)
	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: fixture.batchID}); err != nil {
		t.Fatalf("replay organization after process kill: %v", err)
	}
	assertOrganizationProcessState(
		t, pool, fixture,
		"completed", "completed", "completed", "indexed", false, 2,
	)
	assertOrganizationProcessFiles(t, fixture)
}

func runOrganizationRecoveryChild(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("MOVIEPILOT_ORGANIZATION_RECOVERY_DSN"))
	batchID, err := uuid.Parse(strings.TrimSpace(os.Getenv("MOVIEPILOT_ORGANIZATION_RECOVERY_BATCH_ID")))
	if err != nil || dsn == "" {
		t.Fatalf("invalid child recovery configuration: %v", err)
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse child PostgreSQL DSN: %v", err)
	}
	poolConfig.MaxConns = 4
	poolConfig.MinConns = 0
	poolConfig.ConnConfig.RuntimeParams["search_path"] = strings.TrimSpace(
		os.Getenv("MOVIEPILOT_ORGANIZATION_RECOVERY_SCHEMA"),
	)
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "moviepilot-organization-process-recovery-child"
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatalf("open child PostgreSQL pool: %v", err)
	}
	defer pool.Close()
	store := database.NewStore(&database.DB{Pool: pool}, nil)
	readyFile := os.Getenv("MOVIEPILOT_ORGANIZATION_RECOVERY_READY_FILE")
	block := make(chan struct{})
	handler := newOrganizationProcessRecoveryHandler(t, store, func(context.Context, string, organizer.Plan) error {
		if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
			return err
		}
		<-block
		return nil
	})
	if err := handler.HandleOrganization(t.Context(), jobs.OrganizationArgs{BatchID: batchID}); err != nil {
		t.Fatalf("child organization handler: %v", err)
	}
}

type organizationProcessRecoveryFixture struct {
	batchID        uuid.UUID
	intentID       uuid.UUID
	subscriptionID int64
	sourcePath     string
	targetPath     string
	content        []byte
	digest         string
}

func createOrganizationProcessRecoveryFixture(
	t *testing.T,
	store *database.Store,
	pool *pgxpool.Pool,
) organizationProcessRecoveryFixture {
	t.Helper()
	const username = "organization-process-recovery-owner"
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO users (username, password_hash)
		VALUES ($1, 'organization-process-recovery-password')`, username); err != nil {
		t.Fatalf("insert recovery user: %v", err)
	}
	var subscriptionID int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO subscriptions (
			username, title, media_type, year, media_source, media_id,
			internal_state, v2_state, config
		) VALUES ($1, 'Recovery Movie', 'movie', 2026, 'tmdb', $2, 'pending_search', 'N', '{}'::jsonb)
		RETURNING id`, username, "process-recovery-"+uuid.NewString()).Scan(&subscriptionID); err != nil {
		t.Fatalf("insert recovery subscription: %v", err)
	}
	intentID := uuid.New()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, subscription_id, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, $5, 'completed')`, intentID, username, subscriptionID,
		"sha256"+strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", ""), "mp-"+uuid.NewString()); err != nil {
		t.Fatalf("insert recovery intent: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='pending_organization', v2_state='R'
		WHERE id=$2`, intentID, subscriptionID); err != nil {
		t.Fatalf("bind recovery intent: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO subscription_episodes (subscription_id, season, episode, state, download_intent_id)
		VALUES ($1, 1, 1, 'pending_organization', $2)`, subscriptionID, intentID); err != nil {
		t.Fatalf("insert recovery episode: %v", err)
	}

	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	targetRoot := filepath.Join(root, "library")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatalf("create recovery source root: %v", err)
	}
	content := []byte("moviepilot organization process recovery\n")
	sourcePath := filepath.Join(sourceRoot, "movie.mkv")
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatalf("write recovery source: %v", err)
	}
	digestBytes := sha256.Sum256(content)
	digest := hex.EncodeToString(digestBytes[:])
	targetPath := filepath.Join(targetRoot, "movie.mkv")
	plan, err := organizer.BuildPlan(targetRoot, []organizer.Operation{{
		Source: sourcePath, Target: targetPath, Mode: organizer.Copy,
		ExpectedSize: int64(len(content)), ExpectedSHA256: digest,
	}})
	if err != nil {
		t.Fatalf("build recovery organization plan: %v", err)
	}
	var siteID int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, domain, url, adapter, enabled, config)
		VALUES ($1,$2,$3,'declarative',TRUE,'{}'::jsonb)
		RETURNING id`, "process-recovery-"+intentID.String(),
		"process-recovery-"+intentID.String()+".invalid",
		"https://process-recovery-"+intentID.String()+".invalid").Scan(&siteID); err != nil {
		t.Fatalf("insert recovery site: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO download_intent_plans (
			intent_id, site_id, downloader_id, resolver_kind, resolver_version,
			resource_id, resource_secret, save_path, paused
		) VALUES ($1,$2,$3,'legacy',1,$4,'','',FALSE)`,
		intentID, siteID, uuid.NewString(), "recovery-"+intentID.String()); err != nil {
		t.Fatalf("insert recovery download plan: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO download_intent_success_inputs (
			intent_id, version, legacy, username, manual, background,
			media_source, media_id, title_year, media_type, category,
			vote_average, resource_term, poster_url, backdrop_url, episodes
		) VALUES ($1,0,TRUE,'',FALSE,FALSE,'','','','','',0,'','','','[]'::jsonb)`, intentID); err != nil {
		t.Fatalf("insert recovery success input: %v", err)
	}
	snapshots, err := buildSuccessSnapshots(organization.SuccessInput{
		IntentID: intentID, Version: 0, Legacy: true, Username: username,
	}, true, plan)
	if err != nil {
		t.Fatalf("build recovery success snapshots: %v", err)
	}
	batch, err := store.CreateOrganizationBatch(t.Context(), username, intentID, sourceRoot, plan, snapshots)
	if err != nil {
		t.Fatalf("create recovery organization batch: %v", err)
	}
	return organizationProcessRecoveryFixture{
		batchID: batch.ID, intentID: intentID, subscriptionID: subscriptionID,
		sourcePath: sourcePath, targetPath: targetPath, content: content, digest: digest,
	}
}

func newOrganizationProcessRecoveryHandler(
	t *testing.T,
	store *database.Store,
	execute func(context.Context, string, organizer.Plan) error,
) *Handler {
	t.Helper()
	handler, err := NewHandler(
		store,
		organizationProcessRecoveryEnqueuer{},
		organizationProcessRecoveryPlanner{},
		organizationProcessRecoveryMediaEnqueuer{},
	)
	if err != nil {
		t.Fatalf("create recovery organization handler: %v", err)
	}
	if execute != nil {
		handler.execute = execute
	}
	return handler
}

type organizationProcessRecoveryEnqueuer struct{}

func (organizationProcessRecoveryEnqueuer) EnqueueOrganization(context.Context, jobs.OrganizationArgs) error {
	return nil
}

type organizationProcessRecoveryPlanner struct{}

func (organizationProcessRecoveryPlanner) MediaIndexCompletionPlan(
	context.Context,
	organizer.Plan,
) (mediaindex.CompletionPlan, error) {
	return mediaindex.CompletionPlan{}, nil
}

type organizationProcessRecoveryMediaEnqueuer struct{}

func (organizationProcessRecoveryMediaEnqueuer) EnqueueMediaIndex(context.Context, jobs.MediaIndexArgs) error {
	return nil
}

func waitForOrganizationRecoveryReady(t *testing.T, readyFile, childOutput string, childWait <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(readyFile); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect child ready file: %v", err)
		}
		select {
		case err := <-childWait:
			output, _ := os.ReadFile(childOutput)
			t.Fatalf("organization child exited before acquiring leases: %v\n%s", err, output)
		case <-ctx.Done():
			t.Fatalf("organization child did not acquire leases: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertOrganizationProcessState(
	t *testing.T,
	pool *pgxpool.Pool,
	fixture organizationProcessRecoveryFixture,
	wantBatch, wantFile, wantParent, wantEpisode string,
	wantLease bool,
	wantAttempts int,
) {
	t.Helper()
	var (
		batchState, fileState, parentState, episodeState string
		leaseToken                                       *uuid.UUID
		leaseExpires                                     *time.Time
		currentIntent                                    *uuid.UUID
		batchAttempts, fileAttempts                      int
		batchCompletedAt, fileCompletedAt                *time.Time
	)
	if err := pool.QueryRow(t.Context(), `
		SELECT b.state, b.attempt_count, b.lease_token, b.lease_expires_at, b.completed_at,
			f.state, f.attempt_count, f.completed_at,
			s.internal_state, s.current_download_intent_id, se.state
		FROM organization_batches AS b
		JOIN organization_files AS f ON f.batch_id=b.id
		JOIN download_intents AS di ON di.id=b.intent_id
		JOIN subscriptions AS s ON s.id=di.subscription_id
		JOIN subscription_episodes AS se
			ON se.subscription_id=s.id AND se.download_intent_id=di.id
		WHERE b.id=$1`, fixture.batchID).Scan(
		&batchState, &batchAttempts, &leaseToken, &leaseExpires, &batchCompletedAt,
		&fileState, &fileAttempts, &fileCompletedAt,
		&parentState, &currentIntent, &episodeState,
	); err != nil {
		t.Fatalf("read organization process state: %v", err)
	}
	if batchState != wantBatch || fileState != wantFile || parentState != wantParent || episodeState != wantEpisode {
		t.Fatalf(
			"organization process state = batch %q file %q parent %q episode %q; want %q/%q/%q/%q",
			batchState, fileState, parentState, episodeState,
			wantBatch, wantFile, wantParent, wantEpisode,
		)
	}
	if (leaseToken != nil && leaseExpires != nil) != wantLease || (leaseToken == nil) != (leaseExpires == nil) {
		t.Fatalf("organization process lease = %v/%v, want active %v", leaseToken, leaseExpires, wantLease)
	}
	if batchAttempts != wantAttempts || fileAttempts != wantAttempts {
		t.Fatalf("organization process attempts = batch %d file %d, want %d", batchAttempts, fileAttempts, wantAttempts)
	}
	if (batchState == "completed") != (batchCompletedAt != nil) ||
		(fileState == "completed") != (fileCompletedAt != nil) {
		t.Fatalf("organization process completion timestamps = batch %v file %v", batchCompletedAt, fileCompletedAt)
	}
	if wantParent == "completed" {
		if currentIntent != nil {
			t.Fatalf("completed subscription current intent = %v, want nil", currentIntent)
		}
	} else if currentIntent == nil || *currentIntent != fixture.intentID {
		t.Fatalf("active subscription current intent = %v, want %s", currentIntent, fixture.intentID)
	}
}

func assertOrganizationProcessFiles(t *testing.T, fixture organizationProcessRecoveryFixture) {
	t.Helper()
	got, err := os.ReadFile(fixture.targetPath)
	if err != nil {
		t.Fatalf("read recovered organization target: %v", err)
	}
	if string(got) != string(fixture.content) {
		t.Fatalf("recovered target content = %q, want %q", got, fixture.content)
	}
	digestBytes := sha256.Sum256(got)
	if hex.EncodeToString(digestBytes[:]) != fixture.digest {
		t.Fatalf("recovered target digest mismatch")
	}
	if _, err := os.Stat(fixture.sourcePath); err != nil {
		t.Fatalf("copy source changed during recovery: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(fixture.targetPath), "movie.mkv*"))
	if err != nil {
		t.Fatalf("list recovered target files: %v", err)
	}
	if len(matches) != 1 || matches[0] != fixture.targetPath {
		t.Fatalf("recovered target files = %v, want only %s", matches, fixture.targetPath)
	}
}

func newOrganizationProcessRecoveryStore(t *testing.T) (*database.Store, *pgxpool.Pool, string, string) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv(organizationProcessRecoveryDatabaseEnv))
	if databaseURL == "" {
		t.Skipf("%s is not configured", organizationProcessRecoveryDatabaseEnv)
	}
	setupCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse integration database URL: %v", err)
	}
	adminConfig.MaxConns = 2
	adminConfig.MinConns = 0
	adminPool, err := pgxpool.NewWithConfig(setupCtx, adminConfig)
	if err != nil {
		t.Fatalf("open integration admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if err := adminPool.Ping(setupCtx); err != nil {
		t.Fatalf("connect integration PostgreSQL: %v", err)
	}

	schema := "moviepilot_organization_process_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	pool, err := pgxpool.NewWithConfig(setupCtx, poolConfig)
	if err != nil {
		t.Fatalf("open isolated store pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(setupCtx); err != nil {
		t.Fatalf("connect isolated schema %q: %v", schema, err)
	}

	childDSN := databaseURL
	return database.NewStore(&database.DB{Pool: pool}, nil), pool, childDSN, schema
}
