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

const databaseIntentDifferentV1Hash = "FEDCBA9876543210FEDCBA9876543210FEDCBA98"

func TestDownloadIntentPostgresConcurrentCreateIsIdempotent(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertDownloadIntentIntegrationUsers(t, store, "intent-create-owner")

	const total = 32
	intent := mustDatabaseDownloadIntent(t, "intent-create-owner", "opaque-concurrent-create")
	results := make(chan downloader.Intent, total)
	start := make(chan struct{})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var group errgroup.Group
	for range total {
		group.Go(func() error {
			<-start
			stored, err := store.CreateDownloadIntent(ctx, "intent-create-owner", intent)
			if err != nil {
				return err
			}
			results <- stored
			return nil
		})
	}
	close(start)
	if err := group.Wait(); err != nil {
		t.Fatalf("并发幂等创建下载意图: %v", err)
	}
	close(results)

	count := 0
	for stored := range results {
		count++
		if stored != intent {
			t.Fatalf("并发创建返回意图 = %#v, want %#v", stored, intent)
		}
	}
	if count != total {
		t.Fatalf("并发创建返回数 = %d, want %d", count, total)
	}

	replay := intent
	replay.ID = uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	replay.Label = "mp-" + strings.Repeat("a", 32)
	replay.State = downloader.IntentFailed
	replay.TorrentHash = strings.ToLower(databaseIntentV1Hash)
	stored, err := store.CreateDownloadIntent(t.Context(), "intent-create-owner", replay)
	if err != nil {
		t.Fatalf("重放已有下载意图: %v", err)
	}
	if stored != intent {
		t.Fatalf("冲突重放覆盖了已有进度: got %#v, want %#v", stored, intent)
	}

	var rowCount int
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT COUNT(*)
		FROM download_intents
		WHERE username=$1 AND candidate_key=$2`, "intent-create-owner", intent.CandidateKey).Scan(&rowCount); err != nil {
		t.Fatalf("统计下载意图幂等行: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("幂等创建行数 = %d, want 1", rowCount)
	}

	var (
		persistedID        uuid.UUID
		persistedCandidate string
		persistedLabel     string
		persistedState     downloader.IntentState
		persistedHash      string
		payload            string
		lastErrorIsNull    bool
	)
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT id, candidate_key, label, state, COALESCE(torrent_hash, ''), payload::text, last_error IS NULL
		FROM download_intents
		WHERE username=$1 AND candidate_key=$2`, "intent-create-owner", intent.CandidateKey).Scan(
		&persistedID,
		&persistedCandidate,
		&persistedLabel,
		&persistedState,
		&persistedHash,
		&payload,
		&lastErrorIsNull,
	); err != nil {
		t.Fatalf("读取持久化下载意图: %v", err)
	}
	if persistedID != intent.ID || persistedCandidate != intent.CandidateKey || persistedLabel != intent.Label {
		t.Fatalf("持久化安全身份不稳定: id=%s candidate=%q label=%q", persistedID, persistedCandidate, persistedLabel)
	}
	if persistedState != downloader.IntentPlanned || persistedHash != "" {
		t.Fatalf("冲突重放修改了状态或 Hash: state=%q hash=%q", persistedState, persistedHash)
	}
	if payload != "{}" || !lastErrorIsNull {
		t.Fatalf("下载意图非敏感列约束失效: payload=%q last_error_null=%v", payload, lastErrorIsNull)
	}
}

func TestDownloadIntentPostgresGetIsOwnerScopedAndSafe(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertDownloadIntentIntegrationUsers(t, store, "intent-get-owner", "intent-get-other")

	intent := mustDatabaseDownloadIntent(t, "intent-get-owner", "opaque-owner-scoped-get")
	if _, err := store.CreateDownloadIntent(t.Context(), "intent-get-owner", intent); err != nil {
		t.Fatalf("创建待读取下载意图: %v", err)
	}
	const (
		sensitivePayload   = `{"url":"https://secret.example/torrent","cookie":"secret-cookie","passkey":"secret-passkey","metainfo":"secret-metainfo"}`
		sensitiveLastError = "last-error-must-not-appear"
	)
	if _, err := store.db.Pool.Exec(t.Context(), `
		UPDATE download_intents
		SET state=$1, torrent_hash=$2, payload=$3::jsonb, last_error=$4
		WHERE id=$5 AND username=$6`,
		downloader.IntentDownloading,
		strings.ToLower(databaseIntentV1Hash),
		sensitivePayload,
		sensitiveLastError,
		intent.ID,
		"intent-get-owner",
	); err != nil {
		t.Fatal("准备 owner-scoped 读取数据失败")
	}

	stored, err := store.GetDownloadIntent(t.Context(), "  intent-get-owner \t", intent.ID)
	if err != nil {
		t.Fatalf("owner 读取下载意图: %v", err)
	}
	want := intent
	want.State = downloader.IntentDownloading
	want.TorrentHash = databaseIntentV1Hash
	if stored != want {
		t.Fatalf("owner 读取结果 = %#v, want %#v", stored, want)
	}

	tests := []struct {
		name     string
		username string
		id       uuid.UUID
	}{
		{name: "其他用户", username: "intent-get-other", id: intent.ID},
		{name: "不存在 UUID", username: "intent-get-owner", id: uuid.MustParse("01010101-0202-0303-0404-050505050505")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.GetDownloadIntent(t.Context(), tt.username, tt.id); !errors.Is(err, ErrDownloadIntentNotFound) {
				t.Fatalf("owner-scoped 缺失错误 = %v, want ErrDownloadIntentNotFound", err)
			}
		})
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.GetDownloadIntent(canceled, "intent-get-owner", intent.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消 owner-scoped 读取错误 = %v, want context.Canceled", err)
	}
}

func TestDownloadIntentPostgresGetDatabaseErrorsAreSanitized(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	if _, err := store.db.Pool.Exec(t.Context(), `DROP TABLE download_intents CASCADE`); err != nil {
		t.Fatal("准备下载意图数据库错误测试失败")
	}

	const sensitiveUsername = "username-must-not-appear"
	id := uuid.MustParse("abababab-cdcd-efef-0101-232323232323")
	_, err := store.GetDownloadIntent(t.Context(), sensitiveUsername, id)
	if !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("数据库读取错误 = %v, want ErrDownloadIntentPersistence", err)
	}
	for _, sensitiveValue := range []string{sensitiveUsername, id.String(), "download_intents"} {
		if strings.Contains(err.Error(), sensitiveValue) {
			t.Fatal("下载意图数据库错误泄露了查询或底层数据库信息")
		}
	}
}

func TestDownloadIntentPostgresCompareAndSwap(t *testing.T) {
	store := newSiteStatisticsIntegrationStore(t)
	insertDownloadIntentIntegrationUsers(t, store, "intent-cas-owner", "intent-cas-other")

	intent := mustDatabaseDownloadIntent(t, "intent-cas-owner", "opaque-cas")
	if _, err := store.CreateDownloadIntent(t.Context(), "intent-cas-owner", intent); err != nil {
		t.Fatalf("创建 CAS 下载意图: %v", err)
	}

	type casResult struct {
		intent downloader.Intent
		err    error
	}
	results := make(chan casResult, 2)
	start := make(chan struct{})
	var group errgroup.Group
	for range 2 {
		group.Go(func() error {
			<-start
			stored, err := store.CompareAndSwapDownloadIntent(
				t.Context(),
				"intent-cas-owner",
				intent.ID,
				[]downloader.IntentState{downloader.IntentPlanned},
				downloader.IntentSubmitting,
				"",
			)
			results <- casResult{intent: stored, err: err}
			return nil
		})
	}
	close(start)
	if err := group.Wait(); err != nil {
		t.Fatalf("并发执行下载意图 CAS: %v", err)
	}
	close(results)

	successes := 0
	conflicts := 0
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			if result.intent.State != downloader.IntentSubmitting {
				t.Fatalf("CAS 成功状态 = %q, want %q", result.intent.State, downloader.IntentSubmitting)
			}
		case errors.Is(result.err, ErrDownloadIntentStateConflict):
			conflicts++
		default:
			t.Fatalf("并发 CAS 返回意外错误: %v", result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("并发 CAS 结果 = success %d conflict %d, want 1/1", successes, conflicts)
	}

	reconciled, err := store.CompareAndSwapDownloadIntent(
		t.Context(),
		"intent-cas-owner",
		intent.ID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentSubmitting,
		strings.ToLower(databaseIntentV1Hash),
	)
	if err != nil {
		t.Fatalf("CAS 对账 Hash: %v", err)
	}
	if reconciled.State != downloader.IntentSubmitting || reconciled.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("Hash 对账结果 = %#v", reconciled)
	}

	_, err = store.CompareAndSwapDownloadIntent(
		t.Context(),
		"intent-cas-owner",
		intent.ID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentSubmitting,
		databaseIntentDifferentV1Hash,
	)
	if !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("不同 Hash 覆盖错误 = %v, want ErrDownloadIntentStateConflict", err)
	}
	var persistedHash string
	if err := store.db.Pool.QueryRow(t.Context(), `
		SELECT COALESCE(torrent_hash, '')
		FROM download_intents
		WHERE id=$1 AND username=$2`, intent.ID, "intent-cas-owner").Scan(&persistedHash); err != nil {
		t.Fatalf("读取 Hash 冲突后的下载意图: %v", err)
	}
	if persistedHash != databaseIntentV1Hash {
		t.Fatalf("不同 Hash 覆盖了既有值: got %q, want %q", persistedHash, databaseIntentV1Hash)
	}

	downloading, err := store.CompareAndSwapDownloadIntent(
		t.Context(),
		"intent-cas-owner",
		intent.ID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentDownloading,
		strings.ToLower(databaseIntentV1Hash),
	)
	if err != nil {
		t.Fatalf("CAS 进入下载状态: %v", err)
	}
	if downloading.State != downloader.IntentDownloading || downloading.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("下载状态 CAS 结果 = %#v", downloading)
	}

	_, err = store.CompareAndSwapDownloadIntent(
		t.Context(),
		"intent-cas-owner",
		intent.ID,
		[]downloader.IntentState{downloader.IntentSubmitting},
		downloader.IntentFailed,
		"",
	)
	if !errors.Is(err, ErrDownloadIntentStateConflict) {
		t.Fatalf("过期 expected-state 错误 = %v, want ErrDownloadIntentStateConflict", err)
	}

	_, err = store.CompareAndSwapDownloadIntent(
		t.Context(),
		"intent-cas-other",
		intent.ID,
		[]downloader.IntentState{downloader.IntentDownloading},
		downloader.IntentFailed,
		"",
	)
	if !errors.Is(err, ErrDownloadIntentNotFound) {
		t.Fatalf("其他用户访问错误 = %v, want ErrDownloadIntentNotFound", err)
	}

	_, err = store.CompareAndSwapDownloadIntent(
		t.Context(),
		"intent-cas-owner",
		uuid.MustParse("99999999-8888-7777-6666-555555555555"),
		[]downloader.IntentState{downloader.IntentDownloading},
		downloader.IntentFailed,
		"",
	)
	if !errors.Is(err, ErrDownloadIntentNotFound) {
		t.Fatalf("不存在意图错误 = %v, want ErrDownloadIntentNotFound", err)
	}

	failed, err := store.CompareAndSwapDownloadIntent(
		t.Context(),
		"intent-cas-owner",
		intent.ID,
		[]downloader.IntentState{downloader.IntentDownloading},
		downloader.IntentFailed,
		"",
	)
	if err != nil {
		t.Fatalf("CAS 进入失败状态: %v", err)
	}
	if failed.State != downloader.IntentFailed || failed.TorrentHash != databaseIntentV1Hash {
		t.Fatalf("空 Hash 清除了既有对账结果: %#v", failed)
	}
}

func insertDownloadIntentIntegrationUsers(t *testing.T, store *Store, usernames ...string) {
	t.Helper()
	for _, username := range usernames {
		if _, err := store.db.Pool.Exec(t.Context(), `
			INSERT INTO users (username, password_hash)
			VALUES ($1, 'integration-fixture-hash')`, username); err != nil {
			t.Fatalf("创建下载意图测试用户 %q: %v", username, err)
		}
	}
}
