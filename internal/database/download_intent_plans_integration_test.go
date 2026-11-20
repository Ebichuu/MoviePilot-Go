//go:build integration

package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

func TestDownloadIntentPlanPostgresIdempotencyOwnerAndEncryption(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	insertDownloadIntentIntegrationUsers(t, store, "plan-owner", "plan-other")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "plan-primary-site")
	otherSiteID := insertDownloadIntentPlanIntegrationSite(t, store, "plan-secondary-site")

	intent := mustDatabaseDownloadIntent(t, "plan-owner", "opaque-plan-idempotency")
	if _, err := store.CreateDownloadIntent(t.Context(), "plan-owner", intent); err != nil {
		t.Fatalf("创建计划所属下载意图: %v", err)
	}
	const (
		resourceSecret = "https://private.example/download/123?passkey=fixture-secret"
		savePath       = "D:\\Private Media\\Movies"
	)
	plan := DownloadIntentPlan{
		IntentID:        intent.ID,
		SiteID:          siteID,
		DownloaderID:    downloadIntentPlanTestDownloaderID,
		ResolverKind:    "haidan",
		ResolverVersion: 1,
		ResourceID:      "torrent-123",
		ResourceSecret:  resourceSecret,
		SavePath:        savePath,
		Paused:          true,
		MoviePriority:   r08MoviePriority(90),
	}

	const total = 16
	results := make(chan DownloadIntentPlan, total)
	start := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var group errgroup.Group
	for range total {
		group.Go(func() error {
			<-start
			stored, err := store.CreateDownloadIntentPlan(ctx, "  plan-owner \t", plan)
			if err != nil {
				return err
			}
			results <- stored
			return nil
		})
	}
	close(start)
	if err := group.Wait(); err != nil {
		t.Fatalf("并发幂等创建下载执行计划: %v", err)
	}
	close(results)

	var first DownloadIntentPlan
	for stored := range results {
		if first.IntentID == uuid.Nil {
			first = stored
		}
		if !sameDownloadIntentPlan(stored, plan) || stored.CreatedAt.IsZero() {
			t.Fatalf("并发创建返回计划不完整: %#v", stored)
		}
		if !stored.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("幂等创建返回不同创建时间: first=%v current=%v", first.CreatedAt, stored.CreatedAt)
		}
	}

	var (
		rowCount              int
		persistedDownloaderID string
		persistedSecret       string
		persistedSavePath     string
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT COUNT(*), MIN(downloader_id), MIN(resource_secret), MIN(save_path)
		FROM download_intent_plans
		WHERE intent_id=$1`, intent.ID).Scan(&rowCount, &persistedDownloaderID, &persistedSecret, &persistedSavePath); err != nil {
		t.Fatalf("读取计划密文: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("幂等计划行数 = %d, want 1", rowCount)
	}
	if !strings.HasPrefix(persistedSecret, "v1:") || persistedSecret == resourceSecret || strings.Contains(persistedSecret, "passkey") {
		t.Fatal("敏感资源定位信息未以 SecretBox 密文落库")
	}
	if persistedDownloaderID != downloadIntentPlanTestDownloaderID {
		t.Fatalf("稳定下载器 ID = %q, want %q", persistedDownloaderID, downloadIntentPlanTestDownloaderID)
	}
	if persistedSavePath != savePath {
		t.Fatal("保存路径持久化结果不符合预期")
	}

	replay := plan
	replay.ResolverKind = "  HAIDAN  "
	replay.ResourceID = " torrent-123 "
	replay.SavePath = "  " + savePath + "  "
	replay.CreatedAt = time.Now().Add(24 * time.Hour)
	replayed, err := store.CreateDownloadIntentPlan(t.Context(), "plan-owner", replay)
	if err != nil {
		t.Fatalf("相同语义重放计划: %v", err)
	}
	if !sameDownloadIntentPlan(replayed, plan) || !replayed.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("相同语义重放结果不稳定: %#v", replayed)
	}
	var replayedCiphertext string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT resource_secret FROM download_intent_plans WHERE intent_id=$1`, intent.ID).Scan(&replayedCiphertext); err != nil {
		t.Fatalf("读取重放后的计划密文: %v", err)
	}
	if replayedCiphertext != persistedSecret {
		t.Fatal("幂等重放改写了不可变计划密文")
	}

	conflicts := []struct {
		name   string
		mutate func(*DownloadIntentPlan)
	}{
		{name: "站点不同", mutate: func(candidate *DownloadIntentPlan) { candidate.SiteID = otherSiteID }},
		{name: "下载器不同", mutate: func(candidate *DownloadIntentPlan) { candidate.DownloaderID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff" }},
		{name: "敏感定位信息不同", mutate: func(candidate *DownloadIntentPlan) { candidate.ResourceSecret += "-different" }},
		{name: "保存路径不同", mutate: func(candidate *DownloadIntentPlan) { candidate.SavePath += "\\Different" }},
		{name: "暂停策略不同", mutate: func(candidate *DownloadIntentPlan) { candidate.Paused = !candidate.Paused }},
		{name: "电影质量不同", mutate: func(candidate *DownloadIntentPlan) { candidate.MoviePriority = r08MoviePriority(100) }},
		{name: "电影质量缺失", mutate: func(candidate *DownloadIntentPlan) { candidate.MoviePriority = nil }},
	}
	for _, tt := range conflicts {
		t.Run(tt.name, func(t *testing.T) {
			candidate := plan
			tt.mutate(&candidate)
			_, createErr := store.CreateDownloadIntentPlan(t.Context(), "plan-owner", candidate)
			if !errors.Is(createErr, ErrDownloadIntentPlanConflict) {
				t.Fatalf("不同语义重放错误 = %v, want ErrDownloadIntentPlanConflict", createErr)
			}
			for _, forbidden := range []string{resourceSecret, savePath, "plan-owner", persistedSecret} {
				if strings.Contains(createErr.Error(), forbidden) {
					t.Fatal("计划冲突错误泄露了敏感数据")
				}
			}
		})
	}
	for _, tc := range []struct {
		name     string
		username string
		plan     DownloadIntentPlan
	}{
		{name: "其他用户创建", username: "plan-other", plan: plan},
		{name: "不存在意图创建", username: "plan-owner", plan: func() DownloadIntentPlan {
			missing := plan
			missing.IntentID = uuid.MustParse("11111111-0000-4000-8000-000000000000")
			return missing
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, createErr := store.CreateDownloadIntentPlan(t.Context(), tc.username, tc.plan); !errors.Is(createErr, ErrDownloadIntentPlanNotFound) {
				t.Fatalf("owner-scoped 创建错误 = %v, want ErrDownloadIntentPlanNotFound", createErr)
			}
		})
	}

	got, err := store.GetDownloadIntentPlan(t.Context(), " plan-owner ", intent.ID)
	if err != nil {
		t.Fatalf("owner 读取并解密计划: %v", err)
	}
	if !sameDownloadIntentPlan(got, plan) || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("owner 读取计划 = %#v", got)
	}
	for _, tc := range []struct {
		name     string
		username string
		intentID uuid.UUID
	}{
		{name: "其他用户", username: "plan-other", intentID: intent.ID},
		{name: "不存在意图", username: "plan-owner", intentID: uuid.MustParse("01010101-0202-0303-0404-050505050505")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, getErr := store.GetDownloadIntentPlan(t.Context(), tc.username, tc.intentID); !errors.Is(getErr, ErrDownloadIntentPlanNotFound) {
				t.Fatalf("owner-scoped 缺失错误 = %v, want ErrDownloadIntentPlanNotFound", getErr)
			}
		})
	}
}

func TestDownloadIntentPlanPostgresTransactionCommitRollbackAndCAS(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	insertDownloadIntentIntegrationUsers(t, store, "plan-tx-owner")
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "plan-transaction-site")

	rollbackIntent := mustDatabaseDownloadIntent(t, "plan-tx-owner", "opaque-plan-tx-rollback")
	rollbackPlan := downloadIntentPlanIntegrationFixture(rollbackIntent.ID, siteID, "rollback-resource")
	rollbackTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始回滚事务: %v", err)
	}
	defer func() { _ = rollbackTx.Rollback(context.Background()) }()
	if _, err := store.CreateDownloadIntentTx(t.Context(), rollbackTx, "plan-tx-owner", rollbackIntent); err != nil {
		t.Fatalf("事务内创建回滚意图: %v", err)
	}
	if _, err := store.CreateDownloadIntentPlanTx(t.Context(), rollbackTx, "plan-tx-owner", rollbackPlan); err != nil {
		t.Fatalf("事务内创建回滚计划: %v", err)
	}
	submitting, err := store.CompareAndSwapDownloadIntentTx(
		t.Context(),
		rollbackTx,
		"plan-tx-owner",
		rollbackIntent.ID,
		[]downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed},
		downloader.IntentSubmitting,
		strings.ToLower(databaseIntentV1Hash),
	)
	if err != nil {
		t.Fatalf("事务内 CAS 回滚意图: %v", err)
	}
	if submitting.State != downloader.IntentSubmitting || submitting.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("多来源 CAS 未同时持久化 Hash: %#v", submitting)
	}
	if _, err := store.GetDownloadIntentPlanTx(t.Context(), rollbackTx, "plan-tx-owner", rollbackIntent.ID); err != nil {
		t.Fatalf("事务内读取回滚计划: %v", err)
	}
	if err := rollbackTx.Rollback(t.Context()); err != nil {
		t.Fatalf("回滚意图与计划事务: %v", err)
	}
	if _, err := store.GetDownloadIntent(t.Context(), "plan-tx-owner", rollbackIntent.ID); !errors.Is(err, ErrDownloadIntentNotFound) {
		t.Fatalf("回滚后意图错误 = %v, want ErrDownloadIntentNotFound", err)
	}
	if _, err := store.GetDownloadIntentPlan(t.Context(), "plan-tx-owner", rollbackIntent.ID); !errors.Is(err, ErrDownloadIntentPlanNotFound) {
		t.Fatalf("回滚后计划错误 = %v, want ErrDownloadIntentPlanNotFound", err)
	}

	commitIntent := mustDatabaseDownloadIntent(t, "plan-tx-owner", "opaque-plan-tx-commit")
	commitPlan := downloadIntentPlanIntegrationFixture(commitIntent.ID, siteID, "commit-resource")
	commitTx, err := store.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("开始提交事务: %v", err)
	}
	defer func() { _ = commitTx.Rollback(context.Background()) }()
	if _, err := store.CreateDownloadIntentTx(t.Context(), commitTx, "plan-tx-owner", commitIntent); err != nil {
		t.Fatalf("事务内创建提交意图: %v", err)
	}
	if _, err := store.CreateDownloadIntentPlanTx(t.Context(), commitTx, "plan-tx-owner", commitPlan); err != nil {
		t.Fatalf("事务内创建提交计划: %v", err)
	}
	if _, err := store.CompareAndSwapDownloadIntentTx(
		t.Context(),
		commitTx,
		"plan-tx-owner",
		commitIntent.ID,
		[]downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed},
		downloader.IntentSubmitting,
		databaseIntentV1Hash,
	); err != nil {
		t.Fatalf("事务内提交意图进入 submitting: %v", err)
	}
	if _, err := store.CompareAndSwapDownloadIntentTx(
		t.Context(),
		commitTx,
		"plan-tx-owner",
		commitIntent.ID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentDownloading,
		databaseIntentV1Hash,
	); err != nil {
		t.Fatalf("事务内提交意图进入 downloading: %v", err)
	}
	if err := commitTx.Commit(t.Context()); err != nil {
		t.Fatalf("提交意图与计划事务: %v", err)
	}

	committedIntent, err := store.GetDownloadIntent(t.Context(), "plan-tx-owner", commitIntent.ID)
	if err != nil {
		t.Fatalf("读取已提交意图: %v", err)
	}
	if committedIntent.State != downloader.IntentDownloading || committedIntent.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("已提交意图状态 = %#v", committedIntent)
	}
	committedPlan, err := store.GetDownloadIntentPlan(t.Context(), "plan-tx-owner", commitIntent.ID)
	if err != nil {
		t.Fatalf("读取已提交计划: %v", err)
	}
	if !sameDownloadIntentPlan(committedPlan, commitPlan) {
		t.Fatalf("已提交计划 = %#v", committedPlan)
	}

	failed, err := store.CompareAndSwapDownloadIntent(
		t.Context(),
		"plan-tx-owner",
		commitIntent.ID,
		[]downloader.IntentState{downloader.IntentDownloading},
		downloader.IntentFailed,
		"",
	)
	if err != nil {
		t.Fatalf("明确失败进入 failed: %v", err)
	}
	if failed.State != downloader.IntentFailed || failed.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("失败状态清除了既有 Hash: %#v", failed)
	}
	retrying, err := store.CompareAndSwapDownloadIntent(
		t.Context(),
		"plan-tx-owner",
		commitIntent.ID,
		[]downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed},
		downloader.IntentSubmitting,
		"",
	)
	if err != nil {
		t.Fatalf("failed 通过多来源 CAS 重试: %v", err)
	}
	if retrying.State != downloader.IntentSubmitting || retrying.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("failed 重试结果 = %#v", retrying)
	}
}

func TestDownloadIntentPlanPostgresErrorsAreSanitized(t *testing.T) {
	store := newDownloadIntentPlanIntegrationStore(t)
	const username = "plan-private-owner-must-not-appear"
	insertDownloadIntentIntegrationUsers(t, store, username)
	siteID := insertDownloadIntentPlanIntegrationSite(t, store, "plan-error-site")
	intent := mustDatabaseDownloadIntent(t, username, "opaque-plan-errors")
	if _, err := store.CreateDownloadIntent(t.Context(), username, intent); err != nil {
		t.Fatalf("创建错误测试意图: %v", err)
	}
	plan := downloadIntentPlanIntegrationFixture(intent.ID, siteID, "error-resource")
	plan.ResourceSecret = "https://private.example/error?passkey=must-not-appear"
	plan.SavePath = "D:\\Private\\Must-Not-Appear"
	const corruptCiphertext = "v1:corrupt-ciphertext-must-not-appear"
	if _, err := store.db.Pool.Exec(t.Context(), `
		INSERT INTO download_intent_plans (
			intent_id, site_id, downloader_id, resolver_kind, resolver_version,
			resource_id, resource_secret, save_path, paused
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		plan.IntentID,
		plan.SiteID,
		plan.DownloaderID,
		plan.ResolverKind,
		plan.ResolverVersion,
		plan.ResourceID,
		corruptCiphertext,
		plan.SavePath,
		plan.Paused,
	); err != nil {
		t.Fatalf("准备损坏计划密文: %v", err)
	}

	_, err := store.GetDownloadIntentPlan(t.Context(), username, intent.ID)
	assertDownloadIntentPlanSanitizedError(t, err, ErrDownloadIntentPlanPersistence, username, plan, corruptCiphertext)

	if _, err := store.db.Pool.Exec(t.Context(), `
		ALTER TABLE download_intent_plans
		RENAME TO download_intent_plans_fault_injected`); err != nil {
		t.Fatalf("准备数据库错误: %v", err)
	}
	_, err = store.CreateDownloadIntentPlan(t.Context(), username, plan)
	assertDownloadIntentPlanSanitizedError(t, err, ErrDownloadIntentPlanPersistence, username, plan, corruptCiphertext)
	_, err = store.GetDownloadIntentPlan(t.Context(), username, intent.ID)
	assertDownloadIntentPlanSanitizedError(t, err, ErrDownloadIntentPlanPersistence, username, plan, corruptCiphertext)
}

func newDownloadIntentPlanIntegrationStore(t *testing.T) *Store {
	t.Helper()
	base := newSiteStatisticsIntegrationStore(t)
	return NewStore(base.db, mustDownloadIntentPlanTestSecretBox(t))
}

func insertDownloadIntentPlanIntegrationSite(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	var siteID int64
	if err := store.db.Pool.QueryRow(t.Context(), `
		INSERT INTO sites (name, url, adapter)
		VALUES ($1, 'https://public.example', 'test-adapter')
		RETURNING id`, name).Scan(&siteID); err != nil {
		t.Fatalf("创建计划测试站点: %v", err)
	}
	return siteID
}

func downloadIntentPlanIntegrationFixture(intentID uuid.UUID, siteID int64, resourceID string) DownloadIntentPlan {
	return DownloadIntentPlan{
		IntentID:        intentID,
		SiteID:          siteID,
		DownloaderID:    downloadIntentPlanTestDownloaderID,
		ResolverKind:    "haidan",
		ResolverVersion: 1,
		ResourceID:      resourceID,
		ResourceSecret:  "https://private.example/download/fixture",
		SavePath:        "D:\\Media",
		Paused:          true,
	}
}

func assertDownloadIntentPlanSanitizedError(
	t *testing.T,
	err error,
	want error,
	username string,
	plan DownloadIntentPlan,
	extra ...string,
) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("计划错误 = %v, want %v", err, want)
	}
	forbidden := []string{
		username,
		plan.IntentID.String(),
		plan.ResourceSecret,
		plan.SavePath,
		"download_intent_plans",
		"INSERT",
		"SELECT",
	}
	forbidden = append(forbidden, extra...)
	for _, value := range forbidden {
		if value != "" && strings.Contains(err.Error(), value) {
			t.Fatalf("计划错误泄露敏感信息或 SQL: %q", value)
		}
	}
}
