//go:build integration

package database

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestMediaIndexPostgresRequiredConfirmationDoesNotDelaySubscriptionSettlement(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixture := createMediaIndexIntegrationFixture(t, store, "media-index-closure", 2, false)
	requestID, created, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	)
	if err != nil || !created || requestID == uuid.Nil {
		t.Fatalf("CompleteOrganizationBatchForIndexing() = %s, %v, %v", requestID, created, err)
	}
	replayedID, replayCreated, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	)
	if err != nil || replayCreated || replayedID != requestID {
		t.Fatalf("replayed completion = %s, %v, %v; want %s, false", replayedID, replayCreated, err, requestID)
	}
	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.Completed, "indexed", false, true)

	claimAt := time.Now().UTC()
	lease, found, err := store.Claim(t.Context(), requestID, claimAt, time.Minute)
	if err != nil || !found || !lease.Valid(requestID) {
		t.Fatalf("Claim() = %#v, %v, %v", lease, found, err)
	}
	if lease.Request.Server.Fingerprint != fixture.completion.RequiredServer.Fingerprint {
		t.Fatalf("claimed fingerprint = %q, want %q", lease.Request.Server.Fingerprint, fixture.completion.RequiredServer.Fingerprint)
	}
	confirmed, err := store.ConfirmRequest(t.Context(), requestID, lease.LeaseToken, claimAt)
	if !errors.Is(err, ErrMediaIndexStateConflict) || confirmed {
		t.Fatalf("ConfirmRequest before refresh/files = %v, %v; want state conflict", confirmed, err)
	}
	if err := store.RecordRefresh(t.Context(), requestID, lease.LeaseToken, claimAt, claimAt.Add(time.Second)); err != nil {
		t.Fatalf("RecordRefresh(): %v", err)
	}
	if _, found, err := store.Claim(t.Context(), requestID, claimAt, time.Minute); err != nil || found {
		t.Fatalf("Claim before retry = found %v, err %v", found, err)
	}
	confirmLease, found, err := store.Claim(t.Context(), requestID, claimAt.Add(time.Second), time.Minute)
	if err != nil || !found || !confirmLease.Valid(requestID) || confirmLease.Request.RefreshRequestedAt == nil {
		t.Fatalf("confirmation Claim() = %#v, %v, %v", confirmLease, found, err)
	}
	files, err := store.ListPendingFiles(t.Context(), requestID, confirmLease.LeaseToken, 16)
	if err != nil || len(files) != 2 {
		t.Fatalf("ListPendingFiles() = %#v, %v", files, err)
	}
	if err := store.ConfirmFile(t.Context(), requestID, lease.LeaseToken, files[0].ID, "stale"); !errors.Is(err, ErrMediaIndexLeaseConflict) {
		t.Fatalf("stale ConfirmFile() error = %v, want lease conflict", err)
	}
	for index, file := range files {
		itemID := "emby-item-" + uuid.NewString()
		if err := store.ConfirmFile(t.Context(), requestID, confirmLease.LeaseToken, file.ID, itemID); err != nil {
			t.Fatalf("ConfirmFile(%d): %v", index, err)
		}
		if err := store.ConfirmFile(t.Context(), requestID, confirmLease.LeaseToken, file.ID, itemID); err != nil {
			t.Fatalf("replayed ConfirmFile(%d): %v", index, err)
		}
		if index == 0 {
			confirmed, err = store.ConfirmRequest(t.Context(), requestID, confirmLease.LeaseToken, claimAt.Add(2*time.Second))
			if err != nil || confirmed {
				t.Fatalf("partial ConfirmRequest() = %v, %v; want false", confirmed, err)
			}
		}
	}
	confirmed, err = store.ConfirmRequest(t.Context(), requestID, confirmLease.LeaseToken, claimAt.Add(3*time.Second))
	if err != nil || !confirmed {
		t.Fatalf("final ConfirmRequest() = %v, %v", confirmed, err)
	}
	if confirmed, err = store.ConfirmRequest(t.Context(), requestID, uuid.New(), claimAt.Add(4*time.Second)); err != nil || !confirmed {
		t.Fatalf("replayed final ConfirmRequest() = %v, %v", confirmed, err)
	}
	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.Completed, "indexed", false, true)
}

func TestMediaIndexPostgresServerFingerprintPersistenceAndReplay(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixture := createMediaIndexIntegrationFixture(t, store, "media-index-fingerprint", 1, false)
	requestID, created, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	)
	if err != nil || !created {
		t.Fatalf("complete required batch = %s, %v, %v", requestID, created, err)
	}
	var fingerprint string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT server_fingerprint FROM media_index_requests WHERE id=$1`, requestID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if fingerprint != fixture.completion.RequiredServer.Fingerprint {
		t.Fatalf("persisted fingerprint = %q, want %q", fingerprint, fixture.completion.RequiredServer.Fingerprint)
	}

	mismatch := fixture.completion.Clone()
	mismatch.RequiredServer.Fingerprint = strings.Repeat("e", 64)
	if replayedID, replayCreated, replayErr := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, mismatch,
	); !errors.Is(replayErr, ErrOrganizationStateConflict) || replayCreated || replayedID != uuid.Nil {
		t.Fatalf("mismatched replay = %s, %v, %v; want state conflict", replayedID, replayCreated, replayErr)
	}
}

func TestMediaIndexPostgresNoRequiredServerCompletesLocally(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixture := createMediaIndexIntegrationFixture(t, store, "media-index-local", 1, false)
	fixture.completion.RequiredServer = nil

	requestID, created, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	)
	if err != nil || created || requestID != uuid.Nil {
		t.Fatalf("CompleteOrganizationBatchForIndexing() = %s, %v, %v; want nil, false", requestID, created, err)
	}
	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.Completed, "indexed", false, true)

	var requests int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM media_index_requests WHERE batch_id=$1`, fixture.batch.ID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("media index request count = %d, want 0", requests)
	}
	if replayedID, replayCreated, replayErr := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	); replayErr != nil || replayCreated || replayedID != uuid.Nil {
		t.Fatalf("replayed local completion = %s, %v, %v; want nil, false", replayedID, replayCreated, replayErr)
	}
}

func TestMediaIndexPostgresMissingEpisodesResumeSearch(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixture := createMediaIndexIntegrationFixture(t, store, "media-index-partial", 1, true)
	requestID, created, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	)
	if err != nil || !created {
		t.Fatalf("complete required batch = %s, %v, %v", requestID, created, err)
	}
	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.PendingSearch, "indexed", false, false)
	claimAt := time.Now().UTC()
	lease, found, err := store.Claim(t.Context(), requestID, claimAt, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim refresh = %#v, %v, %v", lease, found, err)
	}
	if err := store.RecordRefresh(t.Context(), requestID, lease.LeaseToken, claimAt, claimAt); err != nil {
		t.Fatal(err)
	}
	lease, found, err = store.Claim(t.Context(), requestID, claimAt, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim confirmation = %#v, %v, %v", lease, found, err)
	}
	files, err := store.ListPendingFiles(t.Context(), requestID, lease.LeaseToken, 16)
	if err != nil || len(files) != 1 {
		t.Fatalf("pending files = %#v, %v", files, err)
	}
	if err := store.ConfirmFile(t.Context(), requestID, lease.LeaseToken, files[0].ID, "partial-item"); err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.ConfirmRequest(t.Context(), requestID, lease.LeaseToken, claimAt)
	if err != nil || !confirmed {
		t.Fatalf("ConfirmRequest() = %v, %v", confirmed, err)
	}
	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.PendingSearch, "indexed", false, false)
	var missing int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM subscription_episodes
		WHERE subscription_id=$1 AND state='missing'`, fixture.subscriptionID).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 1 {
		t.Fatalf("missing episodes = %d, want 1", missing)
	}
}

func TestMediaIndexPostgresConcurrentClaimAndLeaseRecovery(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixture := createMediaIndexIntegrationFixture(t, store, "media-index-lease", 1, false)
	requestID, _, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	claimAt := time.Now().UTC()
	start := make(chan struct{})
	leases := make([]mediaindex.Lease, workers)
	found := make([]bool, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer group.Done()
			<-start
			leases[index], found[index], errs[index] = store.Claim(t.Context(), requestID, claimAt, time.Minute)
		}(index)
	}
	close(start)
	group.Wait()
	var claimed mediaindex.Lease
	foundCount := 0
	for index := range found {
		if errs[index] != nil {
			t.Fatalf("concurrent claim %d: %v", index, errs[index])
		}
		if found[index] {
			foundCount++
			claimed = leases[index]
		}
	}
	if foundCount != 1 || !claimed.Valid(requestID) {
		t.Fatalf("concurrent claims = %d, lease %#v", foundCount, claimed)
	}
	recoveryAt := claimAt.Add(2 * time.Minute)
	recovered, err := store.RecoverExpired(t.Context(), recoveryAt, 1)
	if err != nil || recovered != 1 {
		t.Fatalf("RecoverExpired() = %d, %v", recovered, err)
	}
	due, err := store.ListDue(t.Context(), recoveryAt, 10)
	if err != nil || len(due) != 1 || due[0] != requestID {
		t.Fatalf("ListDue() = %#v, %v", due, err)
	}
	reclaimed, foundAgain, err := store.Claim(t.Context(), requestID, recoveryAt, time.Minute)
	if err != nil || !foundAgain || reclaimed.LeaseToken == claimed.LeaseToken || reclaimed.Request.AttemptCount != 2 {
		t.Fatalf("reclaimed = %#v, %v, %v", reclaimed, foundAgain, err)
	}
	if err := store.Renew(t.Context(), requestID, reclaimed.LeaseToken, recoveryAt, recoveryAt.Add(time.Minute)); err != nil {
		t.Fatalf("Renew(): %v", err)
	}
	if err := store.RenewMediaIndexLease(t.Context(), requestID, reclaimed.LeaseToken, recoveryAt, time.Minute); err != nil {
		t.Fatalf("RenewMediaIndexLease() wrapper: %v", err)
	}
	if err := store.Renew(t.Context(), requestID, uuid.New(), recoveryAt, recoveryAt.Add(time.Minute)); !errors.Is(err, mediaindex.ErrLeaseLost) {
		t.Fatalf("stale Renew() error = %v, want ErrLeaseLost", err)
	}
}

func TestMediaIndexPostgresSchedulingContinuesBeyondThirtyTwoAttempts(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixture := createMediaIndexIntegrationFixture(t, store, "media-index-unbounded-attempts", 1, false)
	requestID, _, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 40; attempt++ {
		now := time.Now().UTC()
		lease, found, claimErr := store.Claim(t.Context(), requestID, now, time.Minute)
		if claimErr != nil || !found || lease.Request.AttemptCount != attempt {
			t.Fatalf("Claim attempt %d = %#v, %v, %v", attempt, lease, found, claimErr)
		}
		if rescheduleErr := store.Reschedule(t.Context(), requestID, lease.LeaseToken, now, "waiting_for_index"); rescheduleErr != nil {
			t.Fatalf("Reschedule attempt %d: %v", attempt, rescheduleErr)
		}
	}
	now := time.Now().UTC()
	due, err := store.ListDue(t.Context(), now, 10)
	if err != nil || len(due) != 1 || due[0] != requestID {
		t.Fatalf("ListDue after 40 attempts = %#v, %v", due, err)
	}
	lease, found, err := store.Claim(t.Context(), requestID, now, time.Minute)
	if err != nil || !found || lease.Request.AttemptCount != 41 {
		t.Fatalf("Claim after 40 attempts = %#v, %v, %v", lease, found, err)
	}
}

func TestMediaIndexMigrationDownPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	fixture := createMediaIndexIntegrationFixture(t, store, "media-index-down", 1, false)
	if _, _, err := store.CompleteOrganizationBatchForIndexing(
		t.Context(), fixture.batch.ID, fixture.batch.LeaseToken, fixture.completion,
	); err != nil {
		t.Fatal(err)
	}
	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.Completed, "indexed", false, true)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET internal_state='pending_index', v2_state='R', current_download_intent_id=$2,
			completed_at=NULL, updated_at=now()
		WHERE id=$1`, fixture.subscriptionID, fixture.intentID); err != nil {
		t.Fatalf("seed legacy pending_index subscription: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscription_episodes SET state='pending_index', updated_at=now()
		WHERE subscription_id=$1 AND download_intent_id=$2`, fixture.subscriptionID, fixture.intentID); err != nil {
		t.Fatalf("seed legacy pending_index episodes: %v", err)
	}
	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.PendingIndex, "pending_index", true, false)

	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 21); err != nil {
		t.Fatalf("down migration 022: %v", err)
	}

	assertMediaIndexSubscriptionState(t, store, fixture.subscriptionID, fixture.intentID, subscription.PendingOrganization, "pending_organization", true, false)
	var tablesRemoved bool
	if err := store.db.Pool.QueryRow(ctx, `
		SELECT to_regclass('media_index_requests') IS NULL
		   AND to_regclass('media_index_files') IS NULL`).Scan(&tablesRemoved); err != nil {
		t.Fatal(err)
	}
	if !tablesRemoved {
		t.Fatal("media index tables still exist after down migration")
	}
	_, err := store.db.Pool.Exec(ctx, `
		UPDATE subscription_episodes SET state='pending_index'
		WHERE subscription_id=$1 AND download_intent_id=$2`, fixture.subscriptionID, fixture.intentID)
	var postgresErr *pgconn.PgError
	if !errors.As(err, &postgresErr) || postgresErr.Code != "23514" || postgresErr.ConstraintName != "subscription_episodes_state_check" {
		t.Fatalf("pending_index after down error = %v, want subscription_episodes_state_check violation", err)
	}
}

type mediaIndexIntegrationFixture struct {
	subscriptionID int64
	intentID       uuid.UUID
	batch          OrganizationBatch
	completion     mediaindex.CompletionPlan
}

func createMediaIndexIntegrationFixture(
	t *testing.T,
	store *Store,
	name string,
	fileCount int,
	withMissing bool,
) mediaIndexIntegrationFixture {
	t.Helper()
	username := name + "-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, name)
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, subscription_id, candidate_key, label, state)
		VALUES ($1,$2,$3,$4,$5,'completed')`, intentID, username, subscriptionID,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='pending_organization', v2_state='R'
		WHERE id=$2`, intentID, subscriptionID); err != nil {
		t.Fatal(err)
	}
	for episode := 1; episode <= fileCount; episode++ {
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO subscription_episodes (subscription_id, season, episode, state, download_intent_id)
			VALUES ($1,1,$2,'pending_organization',$3)`, subscriptionID, episode, intentID); err != nil {
			t.Fatal(err)
		}
	}
	if withMissing {
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO subscription_episodes (subscription_id, season, episode, state)
			VALUES ($1,1,$2,'missing')`, subscriptionID, fileCount+1); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	operations := make([]organizer.Operation, fileCount)
	files := make([]mediaindex.TargetFile, fileCount)
	for index := 0; index < fileCount; index++ {
		localTarget := filepath.Join(root, "library", "episode-"+string(rune('1'+index))+".mkv")
		operations[index] = organizationTestOperation(
			filepath.Join(root, "downloads", "episode-"+string(rune('1'+index))+".mkv"),
			localTarget,
			organizer.Copy,
		)
		files[index] = mediaindex.TargetFile{
			LocalPath:      localTarget,
			RemotePath:     "/media/episode-" + string(rune('1'+index)) + ".mkv",
			PathStyle:      "posix",
			CaseSensitive:  true,
			ExpectedSize:   operations[index].ExpectedSize,
			ExpectedSHA256: operations[index].ExpectedSHA256,
			MediaType:      "Episode",
			Season:         1,
			Episode:        index + 1,
		}
	}
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), operations)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, plan)
	batch, err := store.CreateOrganizationBatch(
		t.Context(), username, intentID, filepath.Join(root, "downloads"), plan, snapshots,
	)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimOrganizationBatch(t.Context(), batch.ID, time.Now().UTC(), time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimOrganizationBatch() = %#v, %v, %v", claimed, found, err)
	}
	organizationFiles, err := store.ClaimOrganizationFiles(t.Context(), batch.ID, claimed.LeaseToken, fileCount)
	if err != nil || len(organizationFiles) != fileCount {
		t.Fatalf("ClaimOrganizationFiles() = %#v, %v", organizationFiles, err)
	}
	for _, file := range organizationFiles {
		if err := store.CompleteOrganizationFile(t.Context(), batch.ID, claimed.LeaseToken, file.ID); err != nil {
			t.Fatal(err)
		}
	}
	claimed.Plan = plan
	return mediaIndexIntegrationFixture{
		subscriptionID: subscriptionID,
		intentID:       intentID,
		batch:          claimed,
		completion: mediaindex.CompletionPlan{
			RequiredServer: &mediaindex.ServerRef{
				ID: uuid.NewString(), Name: "Required Emby", Type: "emby", Fingerprint: strings.Repeat("d", 64),
			},
			Files: files,
		},
	}
}

func assertMediaIndexSubscriptionState(
	t *testing.T,
	store *Store,
	subscriptionID int64,
	intentID uuid.UUID,
	wantParent subscription.State,
	wantEpisode string,
	wantCurrent bool,
	wantCompleted bool,
) {
	t.Helper()
	var (
		parentState   subscription.State
		v2State       string
		currentIntent *uuid.UUID
		completedAt   *time.Time
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, v2_state, current_download_intent_id, completed_at
		FROM subscriptions WHERE id=$1`, subscriptionID).Scan(&parentState, &v2State, &currentIntent, &completedAt); err != nil {
		t.Fatal(err)
	}
	var episodeState string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state FROM subscription_episodes
		WHERE subscription_id=$1 AND download_intent_id=$2
		ORDER BY episode LIMIT 1`, subscriptionID, intentID).Scan(&episodeState); err != nil {
		t.Fatal(err)
	}
	wantV2 := "R"
	if wantParent == subscription.PendingSearch {
		wantV2 = "N"
	}
	if wantParent == subscription.Completed {
		wantV2 = ""
	}
	currentMatches := currentIntent != nil && *currentIntent == intentID
	completed := completedAt != nil
	if parentState != wantParent || v2State != wantV2 || episodeState != wantEpisode ||
		currentMatches != wantCurrent || completed != wantCompleted {
		t.Fatalf("state = parent %q v2 %q episode %q current %v completed %v; want %q/%q/%q/%v/%v",
			parentState, v2State, episodeState, currentIntent, completedAt,
			wantParent, wantV2, wantEpisode, wantCurrent, wantCompleted)
	}
}
