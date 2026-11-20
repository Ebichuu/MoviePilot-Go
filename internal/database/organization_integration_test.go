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

	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestOrganizationPostgresExecutionClosure(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	username := "organization-closure-owner"
	insertSubscriptionIntegrationUsers(t, store, username)

	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "organization-closure")
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, subscription_id, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, $5, 'completed')`, intentID, username, subscriptionID,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatalf("创建已完成下载意图: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='pending_organization', v2_state='R'
		WHERE id=$2`, intentID, subscriptionID); err != nil {
		t.Fatalf("绑定当前下载意图: %v", err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO subscription_episodes (subscription_id, season, episode, state, download_intent_id)
		VALUES ($1, 1, 1, 'pending_organization', $2), ($1, 1, 2, 'pending_organization', $2)`, subscriptionID, intentID); err != nil {
		t.Fatalf("创建逐集 fixture: %v", err)
	}

	root := t.TempDir()
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(filepath.Join(root, "downloads", "one.mkv"), filepath.Join(root, "library", "one.mkv"), organizer.Move),
		organizationTestOperation(filepath.Join(root, "downloads", "two.mkv"), filepath.Join(root, "library", "two.mkv"), organizer.Copy),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, plan)
	batch, err := store.CreateOrganizationBatch(t.Context(), username, intentID, filepath.Join(root, "downloads"), plan, snapshots)
	if err != nil {
		t.Fatalf("创建整理批次: %v", err)
	}
	byIntent, err := store.GetOrganizationBatchByIntent(t.Context(), " "+username+" ", intentID)
	if err != nil || byIntent.ID != batch.ID {
		t.Fatalf("按意图读取批次 = %s, %v; want %s", byIntent.ID, err, batch.ID)
	}
	if _, err := store.GetOrganizationBatchByIntent(t.Context(), "another-owner", intentID); !errors.Is(err, ErrOrganizationBatchNotFound) {
		t.Fatalf("其他 owner 按意图读取 error = %v, want not found", err)
	}
	replayed, err := store.CreateOrganizationBatch(t.Context(), username, intentID, filepath.Join(root, "downloads"), plan, snapshots)
	if err != nil || replayed.ID != batch.ID {
		t.Fatalf("幂等创建批次 = %s, %v; want %s", replayed.ID, err, batch.ID)
	}
	changedPlan, err := organizer.BuildPlan(filepath.Join(root, "library2"), []organizer.Operation{
		organizationTestOperation(filepath.Join(root, "downloads", "one.mkv"), filepath.Join(root, "library2", "one.mkv"), organizer.Move),
		organizationTestOperation(filepath.Join(root, "downloads", "two.mkv"), filepath.Join(root, "library2", "two.mkv"), organizer.Copy),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOrganizationBatch(
		t.Context(), username, intentID, filepath.Join(root, "downloads"), changedPlan,
		organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, changedPlan),
	); !errors.Is(err, ErrOrganizationPlanConflict) {
		t.Fatalf("不同计划重放 error = %v, want conflict", err)
	}

	files, err := store.ListOrganizationFiles(t.Context(), username, batch.ID, 0, 1)
	if err != nil || len(files) != 1 {
		t.Fatalf("分页读取整理文件 = %#v, %v", files, err)
	}
	if files[0].ExpectedSize != plan.Operations[0].ExpectedSize ||
		files[0].ExpectedSHA256 != plan.Operations[0].ExpectedSHA256 {
		t.Fatalf("整理文件指纹往返 = (%d, %q), want (%d, %q)",
			files[0].ExpectedSize, files[0].ExpectedSHA256,
			plan.Operations[0].ExpectedSize, plan.Operations[0].ExpectedSHA256)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE organization_files
		SET expected_size=expected_size+1
		WHERE id=$1`, files[0].ID); err == nil {
		t.Fatal("持久化文件指纹应不可变")
	} else {
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "P0001" {
			t.Fatalf("修改不可变指纹 error = %v, want P0001", err)
		}
	}
	for index, tt := range []struct {
		name       string
		size       int64
		digest     string
		constraint string
	}{
		{
			name: "negative size", size: -1, digest: organizationTestSHA256,
			constraint: "organization_files_expected_size_check",
		},
		{
			name: "uppercase digest", size: 5, digest: strings.Repeat("A", 64),
			constraint: "organization_files_expected_sha256_check",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, insertErr := store.db.Pool.Exec(t.Context(), `
				INSERT INTO organization_files (
					batch_id, ordinal, source_path, target_path, mode,
					expected_size, expected_sha256, state, last_error
				) VALUES ($1, $2, $3, $4, 'copy', $5, $6, 'planned', '')`,
				batch.ID, 100+index, filepath.Join(root, "downloads", tt.name),
				filepath.Join(root, "library", tt.name), tt.size, tt.digest)
			var postgresError *pgconn.PgError
			if !errors.As(insertErr, &postgresError) || postgresError.Code != "23514" || postgresError.ConstraintName != tt.constraint {
				t.Fatalf("非法指纹 error = %v, want check %s", insertErr, tt.constraint)
			}
		})
	}
	claimed, err := store.ClaimOrganizationBatches(t.Context(), time.Now().UTC(), 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != batch.ID || claimed[0].State != OrganizationBatchOrganizing {
		t.Fatalf("领取整理批次 = %#v, %v", claimed, err)
	}
	claimedBatch := claimed[0]
	if err := store.RenewOrganizationBatchLease(t.Context(), batch.ID, claimedBatch.LeaseToken, time.Now().UTC(), time.Minute); err != nil {
		t.Fatalf("续租整理批次: %v", err)
	}

	claimedFiles, err := store.ClaimOrganizationFiles(t.Context(), batch.ID, claimedBatch.LeaseToken, 10)
	if err != nil || len(claimedFiles) != 2 {
		t.Fatalf("领取整理文件 = %#v, %v", claimedFiles, err)
	}
	for _, file := range claimedFiles {
		operation := plan.Operations[file.Ordinal]
		if file.ExpectedSize != operation.ExpectedSize || file.ExpectedSHA256 != operation.ExpectedSHA256 {
			t.Fatalf("领取文件指纹 = (%d, %q), want (%d, %q)", file.ExpectedSize, file.ExpectedSHA256, operation.ExpectedSize, operation.ExpectedSHA256)
		}
		if err := store.CompleteOrganizationFile(t.Context(), batch.ID, claimedBatch.LeaseToken, file.ID); err != nil {
			t.Fatalf("完成整理文件 %d: %v", file.ID, err)
		}
		if err := store.CompleteOrganizationFile(t.Context(), batch.ID, claimedBatch.LeaseToken, file.ID); err != nil {
			t.Fatalf("重放完成整理文件 %d: %v", file.ID, err)
		}
	}
	if err := store.CompleteOrganizationBatch(t.Context(), batch.ID, claimedBatch.LeaseToken); err != nil {
		t.Fatalf("完成整理批次: %v", err)
	}
	if err := store.CompleteOrganizationBatch(t.Context(), batch.ID, claimedBatch.LeaseToken); err != nil {
		t.Fatalf("重放完成整理批次: %v", err)
	}
	if completed, found, err := store.ClaimOrganizationBatch(t.Context(), batch.ID, time.Now().UTC(), time.Minute); err != nil || found {
		t.Fatalf("已完成批次再次领取 = %#v, found %v, %v", completed, found, err)
	}
	if due, err := store.ListDueOrganizationBatchIDs(t.Context(), time.Now().UTC().Add(time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("已完成批次 due 列表 = %#v, %v", due, err)
	}

	var episodeStates []string
	rows, err := store.db.Pool.Query(t.Context(), `
		SELECT state FROM subscription_episodes
		WHERE subscription_id=$1 ORDER BY season, episode`, subscriptionID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		episodeStates = append(episodeStates, state)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(episodeStates) != 2 || episodeStates[0] != string(SubscriptionEpisodeIndexed) || episodeStates[1] != string(SubscriptionEpisodeIndexed) {
		t.Fatalf("完成后逐集状态 = %#v", episodeStates)
	}
	var parentState string
	var completedAt *time.Time
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, completed_at FROM subscriptions WHERE id=$1`, subscriptionID).Scan(&parentState, &completedAt); err != nil {
		t.Fatal(err)
	}
	if parentState != string(subscription.Completed) || completedAt == nil {
		t.Fatalf("完成后父订阅 = (%q, %v), want indexed and not completed", parentState, completedAt)
	}
}

func TestOrganizationPostgresConcurrentCreateAndClaim(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	username := "organization-concurrent-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, 'completed')`, intentID, username,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatalf("create completed download intent: %v", err)
	}

	root := t.TempDir()
	sourceRoot := filepath.Join(root, "downloads")
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{
		organizationTestOperation(
			filepath.Join(sourceRoot, "one.mkv"),
			filepath.Join(root, "library", "one.mkv"),
			organizer.Copy,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, plan)

	const workers = 8
	createStart := make(chan struct{})
	created := make([]OrganizationBatch, workers)
	createErrs := make([]error, workers)
	var createWG sync.WaitGroup
	createWG.Add(workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer createWG.Done()
			<-createStart
			created[index], createErrs[index] = store.CreateOrganizationBatch(
				t.Context(), username, intentID, sourceRoot, plan, snapshots,
			)
		}(index)
	}
	close(createStart)
	createWG.Wait()

	batchID := created[0].ID
	if batchID == uuid.Nil {
		t.Fatal("concurrent create returned a nil batch ID")
	}
	for index := 0; index < workers; index++ {
		if createErrs[index] != nil {
			t.Fatalf("concurrent create %d: %v", index, createErrs[index])
		}
		if created[index].ID != batchID {
			t.Fatalf("concurrent create %d returned batch %s, want %s", index, created[index].ID, batchID)
		}
	}
	var batchCount int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM organization_batches WHERE intent_id=$1`, intentID).Scan(&batchCount); err != nil {
		t.Fatal(err)
	}
	if batchCount != 1 {
		t.Fatalf("organization batch count = %d, want 1", batchCount)
	}

	claimAt := time.Now().UTC()
	claimStart := make(chan struct{})
	claims := make([]OrganizationBatch, workers)
	claimFound := make([]bool, workers)
	claimErrs := make([]error, workers)
	var claimWG sync.WaitGroup
	claimWG.Add(workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer claimWG.Done()
			<-claimStart
			claims[index], claimFound[index], claimErrs[index] = store.ClaimOrganizationBatch(
				t.Context(), batchID, claimAt, time.Minute,
			)
		}(index)
	}
	close(claimStart)
	claimWG.Wait()

	foundCount := 0
	var claimed OrganizationBatch
	for index := 0; index < workers; index++ {
		if claimErrs[index] != nil {
			t.Fatalf("concurrent claim %d: %v", index, claimErrs[index])
		}
		if claimFound[index] {
			foundCount++
			claimed = claims[index]
		}
	}
	if foundCount != 1 || claimed.ID != batchID || claimed.LeaseToken == uuid.Nil {
		t.Fatalf("concurrent claims found=%d claimed=%#v, want one non-empty lease", foundCount, claimed)
	}
	var (
		batchState    string
		batchAttempts int
		leaseToken    uuid.UUID
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state, attempt_count, lease_token
		FROM organization_batches WHERE id=$1`, batchID).Scan(&batchState, &batchAttempts, &leaseToken); err != nil {
		t.Fatal(err)
	}
	if batchState != string(OrganizationBatchOrganizing) || batchAttempts != 1 || leaseToken != claimed.LeaseToken {
		t.Fatalf("claimed batch state=%q attempts=%d token=%s, want organizing/1/%s",
			batchState, batchAttempts, leaseToken, claimed.LeaseToken)
	}

	fileStart := make(chan struct{})
	fileClaims := make([][]OrganizationFile, 2)
	fileErrs := make([]error, 2)
	var fileWG sync.WaitGroup
	fileWG.Add(2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			defer fileWG.Done()
			<-fileStart
			fileClaims[index], fileErrs[index] = store.ClaimOrganizationFiles(
				t.Context(), batchID, claimed.LeaseToken, 1,
			)
		}(index)
	}
	close(fileStart)
	fileWG.Wait()

	claimedFiles := 0
	for index := range fileClaims {
		if fileErrs[index] != nil {
			t.Fatalf("concurrent file claim %d: %v", index, fileErrs[index])
		}
		claimedFiles += len(fileClaims[index])
	}
	if claimedFiles != 1 {
		t.Fatalf("concurrent file claims returned %d files, want 1", claimedFiles)
	}
	var (
		fileState    string
		fileAttempts int
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state, attempt_count FROM organization_files WHERE batch_id=$1`, batchID).Scan(&fileState, &fileAttempts); err != nil {
		t.Fatal(err)
	}
	if fileState != string(OrganizationFileOrganizing) || fileAttempts != 1 {
		t.Fatalf("claimed file state=%q attempts=%d, want organizing/1", fileState, fileAttempts)
	}
}

func TestOrganizationFingerprintMigrationPostgres(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	var schema string
	if err := store.db.Pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatalf("读取隔离 schema: %v", err)
	}
	migrationDB, _ := openSubscriptionV2StateMigrationDB(t, schema)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("设置 goose PostgreSQL 方言: %v", err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", 20); err != nil {
		t.Fatalf("回退到迁移 020: %v", err)
	}

	username := "organization-fingerprint-upgrade-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO download_intents (id, username, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, 'completed')`, intentID, username,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatalf("创建升级下载意图: %v", err)
	}
	root := t.TempDir()
	batchID := uuid.New()
	source := filepath.Join(root, "downloads", "legacy.mkv")
	targetRoot := filepath.Join(root, "library")
	target := filepath.Join(targetRoot, "legacy.mkv")
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO organization_batches (
			id, intent_id, state, source_root, target_root, plan,
			last_error, attempt_count, next_attempt_at
		) VALUES (
			$1, $2, 'planned', $3, $4,
			jsonb_build_array(jsonb_build_object(
				'source', $5::text, 'target', $6::text, 'mode', 'move'
			)),
			'', 0, now()
		)`, batchID, intentID, filepath.Join(root, "downloads"), targetRoot, source, target); err != nil {
		t.Fatalf("创建旧版整理批次: %v", err)
	}
	if _, err := store.db.Pool.Exec(ctx, `
		INSERT INTO organization_files (
			batch_id, ordinal, source_path, target_path, mode, state,
			attempt_count, last_error
		) VALUES ($1, 0, $2, $3, 'move', 'planned', 0, '')`, batchID, source, target); err != nil {
		t.Fatalf("创建旧版整理文件: %v", err)
	}

	if err := goose.UpToContext(ctx, migrationDB, "migrations", 21); err != nil {
		t.Fatalf("升级到迁移 021: %v", err)
	}
	if err := goose.UpToContext(ctx, migrationDB, "migrations", 21); err != nil {
		t.Fatalf("重复执行迁移流程: %v", err)
	}

	batch, err := store.GetOrganizationBatch(t.Context(), username, batchID)
	if err != nil {
		t.Fatalf("读取升级批次: %v", err)
	}
	files, err := store.ListOrganizationFiles(t.Context(), username, batchID, 0, 10)
	if err != nil || len(files) != 1 {
		t.Fatalf("读取升级文件 = %#v, %v", files, err)
	}
	sentinel := strings.Repeat("0", 64)
	if files[0].ExpectedSize != 0 || files[0].ExpectedSHA256 != sentinel ||
		batch.Plan.Operations[0].ExpectedSize != 0 || batch.Plan.Operations[0].ExpectedSHA256 != sentinel {
		t.Fatalf("旧计划指纹回填 = batch %#v file %#v", batch.Plan.Operations[0], files[0])
	}
}

func TestOrganizationPostgresPartialEpisodesResumeSearch(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	username := "organization-partial-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "organization-partial")
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, subscription_id, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, $5, 'completed')`, intentID, username, subscriptionID,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='pending_organization', v2_state='R'
		WHERE id=$2`, intentID, subscriptionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO subscription_episodes (subscription_id, season, episode, state, download_intent_id)
		VALUES ($1, 1, 1, 'pending_organization', $2), ($1, 1, 2, 'missing', NULL)`, subscriptionID, intentID); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{{
		Source: filepath.Join(root, "downloads", "one.mkv"), Target: filepath.Join(root, "library", "one.mkv"), Mode: organizer.Copy,
		ExpectedSize: 5, ExpectedSHA256: organizationTestSHA256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, plan)
	batch, err := store.CreateOrganizationBatch(t.Context(), username, intentID, filepath.Join(root, "downloads"), plan, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimOrganizationBatch(t.Context(), batch.ID, time.Now().UTC(), time.Minute)
	if err != nil || !found {
		t.Fatalf("领取部分覆盖批次 = %#v, found %v, %v", claimed, found, err)
	}
	files, err := store.ClaimOrganizationFiles(t.Context(), batch.ID, claimed.LeaseToken, 1)
	if err != nil || len(files) != 1 {
		t.Fatalf("领取部分覆盖文件 = %#v, %v", files, err)
	}
	if err := store.CompleteOrganizationFile(t.Context(), batch.ID, claimed.LeaseToken, files[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOrganizationBatch(t.Context(), batch.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	assertOrganizationSubscriptionState(t, store, subscriptionID, intentID, "pending_search", "indexed", false)
	var missing int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT count(*) FROM subscription_episodes
		WHERE subscription_id=$1 AND state='missing'`, subscriptionID).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 1 {
		t.Fatalf("未覆盖缺集数 = %d, want 1", missing)
	}
}

func TestOrganizationPostgresLeaseRecoveryAndRetry(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	username := "organization-recovery-owner"
	insertSubscriptionIntegrationUsers(t, store, username)
	subscriptionID := createSubscriptionIntegrationFixture(t, store, username, "organization-recovery")
	intentID := uuid.New()
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intents (id, username, subscription_id, candidate_key, label, state)
		VALUES ($1, $2, $3, $4, $5, 'completed')`, intentID, username, subscriptionID,
		"sha256"+uuid.NewString()+uuid.NewString(), "mp-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE subscriptions
		SET current_download_intent_id=$1, internal_state='pending_organization', v2_state='R'
		WHERE id=$2`, intentID, subscriptionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO subscription_episodes (subscription_id, season, episode, state, download_intent_id)
		VALUES ($1, 1, 1, 'pending_organization', $2)`, subscriptionID, intentID); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	plan, err := organizer.BuildPlan(filepath.Join(root, "library"), []organizer.Operation{{
		Source: filepath.Join(root, "downloads", "one.mkv"), Target: filepath.Join(root, "library", "one.mkv"), Mode: organizer.Copy,
		ExpectedSize: 5, ExpectedSHA256: organizationTestSHA256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := organizationLegacySuccessSnapshotsIntegration(t, store, username, intentID, plan)
	batch, err := store.CreateOrganizationBatch(t.Context(), username, intentID, filepath.Join(root, "downloads"), plan, snapshots)
	if err != nil {
		t.Fatalf("创建恢复 fixture: %v", err)
	}
	claimNow := time.Now().UTC()
	if due, err := store.ListDueOrganizationBatchIDs(t.Context(), claimNow, 10); err != nil || len(due) != 1 || due[0] != batch.ID {
		t.Fatalf("planned due 列表 = %#v, %v", due, err)
	}
	if missing, found, err := store.ClaimOrganizationBatch(t.Context(), uuid.New(), claimNow, time.Minute); err != nil || found {
		t.Fatalf("领取不存在批次 = %#v, found %v, %v", missing, found, err)
	}
	claimed, found, err := store.ClaimOrganizationBatch(t.Context(), batch.ID, claimNow, time.Minute)
	if err != nil || !found || claimed.ID != batch.ID {
		t.Fatalf("按 ID 领取恢复 fixture = %#v, found %v, %v", claimed, found, err)
	}
	assertOrganizationSubscriptionState(t, store, subscriptionID, intentID, "organizing", "organizing", true)
	if active, activeFound, activeErr := store.ClaimOrganizationBatch(t.Context(), batch.ID, claimNow, time.Minute); activeErr != nil || activeFound {
		t.Fatalf("活动租约重复领取 = %#v, found %v, %v", active, activeFound, activeErr)
	}
	if due, err := store.ListDueOrganizationBatchIDs(t.Context(), claimNow, 10); err != nil || len(due) != 0 {
		t.Fatalf("活动租约 due 列表 = %#v, %v", due, err)
	}
	files, err := store.ClaimOrganizationFiles(t.Context(), batch.ID, claimed.LeaseToken, 1)
	if err != nil || len(files) != 1 {
		t.Fatalf("领取恢复文件 = %#v, %v", files, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE organization_batches SET lease_expires_at=$2 WHERE id=$1`, batch.ID, claimNow.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	reclaimNow := claimNow.Add(time.Second)
	if due, err := store.ListDueOrganizationBatchIDs(t.Context(), reclaimNow, 10); err != nil || len(due) != 1 || due[0] != batch.ID {
		t.Fatalf("过期租约 due 列表 = %#v, %v", due, err)
	}
	reclaimed, found, err := store.ClaimOrganizationBatch(t.Context(), batch.ID, reclaimNow, time.Minute)
	if err != nil || !found || reclaimed.ID != batch.ID || reclaimed.LeaseToken == claimed.LeaseToken {
		t.Fatalf("直接重领过期批次 = %#v, found %v, %v", reclaimed, found, err)
	}
	var fileState string
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT state FROM organization_files WHERE batch_id=$1`, batch.ID).Scan(&fileState); err != nil {
		t.Fatal(err)
	}
	if fileState != string(OrganizationFileFailed) {
		t.Fatalf("直接重领后的旧执行中文件状态 = %q, want failed", fileState)
	}
	assertOrganizationSubscriptionState(t, store, subscriptionID, intentID, "organizing", "organizing", true)
	files, err = store.ClaimOrganizationFiles(t.Context(), batch.ID, reclaimed.LeaseToken, 1)
	if err != nil || len(files) != 1 {
		t.Fatalf("直接重领后的文件领取 = %#v, %v", files, err)
	}
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE organization_batches SET lease_expires_at=$2 WHERE id=$1`, batch.ID, reclaimNow.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	recoveryNow := reclaimNow.Add(time.Second)
	recovered, err := store.RecoverExpiredOrganizationLeases(t.Context(), recoveryNow, 1)
	if err != nil || recovered != 1 {
		t.Fatalf("回收过期租约 = %d, %v", recovered, err)
	}
	var batchState string
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT state FROM organization_batches WHERE id=$1`, batch.ID).Scan(&batchState); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `SELECT state FROM organization_files WHERE batch_id=$1`, batch.ID).Scan(&fileState); err != nil {
		t.Fatal(err)
	}
	if batchState != string(OrganizationBatchFailed) || fileState != string(OrganizationFileFailed) {
		t.Fatalf("过期回收状态 = batch %q file %q", batchState, fileState)
	}
	assertOrganizationSubscriptionState(t, store, subscriptionID, intentID, "pending_organization", "pending_organization", true)

	retryNow := recoveryNow.Add(time.Second)
	claimed, found, err = store.ClaimOrganizationBatch(t.Context(), batch.ID, retryNow, time.Minute)
	if err != nil || !found {
		t.Fatalf("重试领取批次 = %#v, found %v, %v", claimed, found, err)
	}
	files, err = store.ClaimOrganizationFiles(t.Context(), batch.ID, claimed.LeaseToken, 1)
	if err != nil || len(files) != 1 {
		t.Fatalf("重试领取文件 = %#v, %v", files, err)
	}
	if err := store.FailOrganizationFile(t.Context(), batch.ID, claimed.LeaseToken, files[0].ID, "  disk timeout\r\n"); err != nil {
		t.Fatalf("记录文件失败: %v", err)
	}
	if err := store.FailOrganizationBatch(t.Context(), batch.ID, claimed.LeaseToken, retryNow.Add(time.Minute), "worker failed"); err != nil {
		t.Fatalf("记录批次失败: %v", err)
	}
	assertOrganizationSubscriptionState(t, store, subscriptionID, intentID, "pending_organization", "pending_organization", true)
	if err := store.CompleteOrganizationFile(t.Context(), batch.ID, claimed.LeaseToken, files[0].ID); !errors.Is(err, ErrOrganizationLeaseConflict) {
		t.Fatalf("过期 token 完成文件 error = %v, want lease conflict", err)
	}
}

func assertOrganizationSubscriptionState(
	t *testing.T,
	store *Store,
	subscriptionID int64,
	intentID uuid.UUID,
	wantParent string,
	wantEpisode string,
	wantCurrent bool,
) {
	t.Helper()
	var (
		parentState   string
		v2State       string
		currentIntent *uuid.UUID
		completedAt   *time.Time
		episodeState  string
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT internal_state, v2_state, current_download_intent_id, completed_at
		FROM subscriptions WHERE id=$1`, subscriptionID).Scan(&parentState, &v2State, &currentIntent, &completedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT state FROM subscription_episodes
		WHERE subscription_id=$1 AND download_intent_id=$2`, subscriptionID, intentID).Scan(&episodeState); err != nil {
		t.Fatal(err)
	}
	wantV2 := "R"
	if wantParent == string(subscription.PendingSearch) {
		wantV2 = "N"
	}
	currentMatches := currentIntent != nil && *currentIntent == intentID
	if parentState != wantParent || episodeState != wantEpisode || v2State != wantV2 || currentMatches != wantCurrent || completedAt != nil {
		t.Fatalf("订阅整理状态 = parent %q v2 %q current %v completed %v episode %q; want parent %q v2 %q current %v episode %q",
			parentState, v2State, currentIntent, completedAt, episodeState, wantParent, wantV2, wantCurrent, wantEpisode)
	}
}
