package database

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

const databaseIntentV1Hash = "0123456789ABCDEF0123456789ABCDEF01234567"

func TestBuildCreateDownloadIntent(t *testing.T) {
	t.Parallel()

	planned := mustDatabaseDownloadIntent(t, "alice", "opaque-create-planned")
	downloading := mustDatabaseDownloadIntent(t, "alice", "opaque-create-downloading")
	downloading.State = downloader.IntentDownloading
	downloading.TorrentHash = strings.ToLower(databaseIntentV1Hash)
	whitespaceHash := planned
	whitespaceHash.TorrentHash = " \t\r\n "

	missingID := planned
	missingID.ID = uuid.Nil
	unsafeCandidateKey := planned
	unsafeCandidateKey.CandidateKey = "opaque-unhashed-candidate"
	unsafeLabel := planned
	unsafeLabel.Label = "opaque-label"
	unknownState := planned
	unknownState.State = downloader.IntentState("unknown")
	missingDownloadingHash := downloading
	missingDownloadingHash.TorrentHash = ""
	invalidHash := planned
	invalidHash.TorrentHash = strings.Repeat("g", 40)

	tests := []struct {
		name     string
		username string
		intent   downloader.Intent
		wantArgs []any
		wantErr  error
	}{
		{
			name:     "计划状态规范化用户名且不写入空 Hash",
			username: "  alice  ",
			intent:   planned,
			wantArgs: []any{planned.ID, "alice", planned.CandidateKey, "", planned.Label, downloader.IntentPlanned},
		},
		{
			name:     "下载状态规范化 Hash",
			username: "alice",
			intent:   downloading,
			wantArgs: []any{downloading.ID, "alice", downloading.CandidateKey, databaseIntentV1Hash, downloading.Label, downloader.IntentDownloading},
		},
		{
			name:     "可选 Hash 只有空白时按空值持久化",
			username: "alice",
			intent:   whitespaceHash,
			wantArgs: []any{whitespaceHash.ID, "alice", whitespaceHash.CandidateKey, "", whitespaceHash.Label, downloader.IntentPlanned},
		},
		{name: "拒绝空用户名", username: " \t ", intent: planned, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝空 UUID", username: "alice", intent: missingID, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝非摘要候选键", username: "alice", intent: unsafeCandidateKey, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝非确定性标签", username: "alice", intent: unsafeLabel, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝未知状态", username: "alice", intent: unknownState, wantErr: ErrInvalidDownloadIntentState},
		{name: "拒绝下载状态缺少 Hash", username: "alice", intent: missingDownloadingHash, wantErr: ErrInvalidDownloadIntentState},
		{name: "拒绝非法 Hash", username: "alice", intent: invalidHash, wantErr: ErrInvalidDownloadIntent},
	}

	const wantSQL = `
		INSERT INTO download_intents (id, username, candidate_key, torrent_hash, label, state, payload)
		VALUES ($1, $2, $3, NULLIF($4::text, ''), $5, $6, '{}'::jsonb)
		ON CONFLICT (username, candidate_key) DO UPDATE
		SET updated_at=now()
		RETURNING id, candidate_key, label, state, COALESCE(torrent_hash, '')`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query, args, err := buildCreateDownloadIntent(tt.username, tt.intent)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildCreateDownloadIntent() error = %v, want %v", err, tt.wantErr)
				}
				if query != "" || args != nil {
					t.Fatalf("非法输入仍生成 SQL: query=%q args=%#v", query, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildCreateDownloadIntent() error = %v", err)
			}
			if compactDatabaseIntentSQL(query) != compactDatabaseIntentSQL(wantSQL) {
				t.Fatalf("buildCreateDownloadIntent() SQL = %q, want %q", query, wantSQL)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("buildCreateDownloadIntent() args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

func TestCreateDownloadIntentSQLDoesNotRegressExistingIntent(t *testing.T) {
	t.Parallel()

	query := compactDatabaseIntentSQL(createDownloadIntentSQL)
	for _, forbidden := range []string{
		"state=excluded.state",
		"torrent_hash=excluded.torrent_hash",
		"label=excluded.label",
		"id=excluded.id",
		"last_error",
	} {
		if strings.Contains(strings.ToLower(query), forbidden) {
			t.Fatalf("幂等创建 SQL 含不允许的覆盖字段 %q: %s", forbidden, query)
		}
	}
	if !strings.Contains(query, "ON CONFLICT (username, candidate_key) DO UPDATE SET updated_at=now()") {
		t.Fatalf("幂等创建 SQL 未限定为只刷新 updated_at: %s", query)
	}
	if !strings.Contains(query, "'{}'::jsonb") {
		t.Fatalf("幂等创建 SQL 未将 payload 固定为空对象: %s", query)
	}
}

func TestBuildGetDownloadIntent(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("77777777-8888-9999-aaaa-bbbbbbbbbbbb")
	tests := []struct {
		name     string
		username string
		id       uuid.UUID
		wantArgs []any
		wantErr  error
	}{
		{
			name:     "规范化 owner 用户名",
			username: "  alice \t",
			id:       id,
			wantArgs: []any{id, "alice"},
		},
		{name: "拒绝空用户名", username: " \t ", id: id, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝空 UUID", username: "alice", wantErr: ErrInvalidDownloadIntent},
	}

	const wantSQL = `
		SELECT id, candidate_key, label, state, COALESCE(torrent_hash, '')
		FROM download_intents
		WHERE id=$1 AND username=$2`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query, args, err := buildGetDownloadIntent(tt.username, tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildGetDownloadIntent() error = %v, want %v", err, tt.wantErr)
				}
				if query != "" || args != nil {
					t.Fatal("非法 owner-scoped 读取仍生成了查询")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildGetDownloadIntent() error = %v", err)
			}
			if compactDatabaseIntentSQL(query) != compactDatabaseIntentSQL(wantSQL) {
				t.Fatal("owner-scoped 读取 SQL 不符合预期")
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("buildGetDownloadIntent() args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

func TestGetDownloadIntentSQLIsOwnerScopedAndExcludesSensitiveColumns(t *testing.T) {
	t.Parallel()

	query := compactDatabaseIntentSQL(getDownloadIntentSQL)
	if !strings.Contains(query, "WHERE id=$1 AND username=$2") {
		t.Fatal("下载意图读取未同时限定 UUID 和 owner")
	}
	for _, forbidden := range []string{"payload", "last_error", "download_url", "cookie", "passkey", "metainfo"} {
		if strings.Contains(strings.ToLower(query), forbidden) {
			t.Fatalf("下载意图读取包含禁止列 %q", forbidden)
		}
	}
}

func TestBuildCompareAndSwapDownloadIntent(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	tests := []struct {
		name     string
		username string
		id       uuid.UUID
		from     []downloader.IntentState
		next     downloader.IntentState
		hash     string
		wantArgs []any
		wantErr  error
	}{
		{
			name:     "计划或失败进入提交并同时规范化 Hash",
			username: "  alice ",
			id:       id,
			from:     []downloader.IntentState{downloader.IntentPlanned, downloader.IntentFailed},
			next:     downloader.IntentSubmitting,
			hash:     strings.ToLower(databaseIntentV1Hash),
			wantArgs: []any{id, "alice", []string{"planned", "failed"}, downloader.IntentSubmitting, databaseIntentV1Hash},
		},
		{
			name:     "提交进入下载并规范化 Hash",
			username: "alice",
			id:       id,
			from:     []downloader.IntentState{downloader.IntentSubmitting},
			next:     downloader.IntentDownloading,
			hash:     strings.ToLower(databaseIntentV1Hash),
			wantArgs: []any{id, "alice", []string{"submitting"}, downloader.IntentDownloading, databaseIntentV1Hash},
		},
		{
			name:     "提交状态幂等对账 Hash",
			username: "alice",
			id:       id,
			from:     []downloader.IntentState{downloader.IntentSubmitting, downloader.IntentSubmitting},
			next:     downloader.IntentSubmitting,
			hash:     databaseIntentV1Hash,
			wantArgs: []any{id, "alice", []string{"submitting"}, downloader.IntentSubmitting, databaseIntentV1Hash},
		},
		{
			name:     "下载失败时空 Hash 表示保留原值",
			username: "alice",
			id:       id,
			from:     []downloader.IntentState{downloader.IntentDownloading},
			next:     downloader.IntentFailed,
			wantArgs: []any{id, "alice", []string{"downloading"}, downloader.IntentFailed, ""},
		},
		{name: "拒绝空用户名", id: id, from: []downloader.IntentState{downloader.IntentPlanned}, next: downloader.IntentSubmitting, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝空 UUID", username: "alice", from: []downloader.IntentState{downloader.IntentPlanned}, next: downloader.IntentSubmitting, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝空来源状态", username: "alice", id: id, next: downloader.IntentSubmitting, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝非法跃迁", username: "alice", id: id, from: []downloader.IntentState{downloader.IntentPlanned}, next: downloader.IntentCompleted, hash: databaseIntentV1Hash, wantErr: ErrInvalidDownloadIntentState},
		{name: "拒绝下载状态缺少 Hash", username: "alice", id: id, from: []downloader.IntentState{downloader.IntentSubmitting}, next: downloader.IntentDownloading, wantErr: ErrInvalidDownloadIntentState},
		{name: "拒绝未知预期状态", username: "alice", id: id, from: []downloader.IntentState{downloader.IntentState("unknown")}, next: downloader.IntentSubmitting, wantErr: ErrInvalidDownloadIntentState},
		{name: "拒绝未知目标状态", username: "alice", id: id, from: []downloader.IntentState{downloader.IntentPlanned}, next: downloader.IntentState("unknown"), wantErr: ErrInvalidDownloadIntentState},
		{name: "拒绝非法 Hash", username: "alice", id: id, from: []downloader.IntentState{downloader.IntentSubmitting}, next: downloader.IntentDownloading, hash: strings.Repeat("z", 40), wantErr: ErrInvalidDownloadIntent},
	}

	const wantSQL = `
		UPDATE download_intents
		SET state=$4,
			torrent_hash=COALESCE(NULLIF(torrent_hash, ''), NULLIF($5::text, '')),
			updated_at=now()
		WHERE id=$1 AND username=$2 AND state=ANY($3::text[])
			AND (
				NULLIF($5::text, '') IS NULL
				OR NULLIF(torrent_hash, '') IS NULL
				OR torrent_hash=$5
			)
		RETURNING id, candidate_key, label, state, COALESCE(torrent_hash, '')`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query, args, err := buildCompareAndSwapDownloadIntent(
				tt.username,
				tt.id,
				tt.from,
				tt.next,
				tt.hash,
			)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildCompareAndSwapDownloadIntent() error = %v, want %v", err, tt.wantErr)
				}
				if query != "" || args != nil {
					t.Fatalf("非法 CAS 仍生成 SQL: query=%q args=%#v", query, args)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildCompareAndSwapDownloadIntent() error = %v", err)
			}
			if compactDatabaseIntentSQL(query) != compactDatabaseIntentSQL(wantSQL) {
				t.Fatalf("buildCompareAndSwapDownloadIntent() SQL = %q, want %q", query, wantSQL)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("buildCompareAndSwapDownloadIntent() args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

func TestDownloadIntentCASQueriesAreOwnerScoped(t *testing.T) {
	t.Parallel()

	update := compactDatabaseIntentSQL(compareAndSwapDownloadIntentSQL)
	if !strings.Contains(update, "WHERE id=$1 AND username=$2 AND state=ANY($3::text[])") {
		t.Fatalf("CAS UPDATE 未同时限定 ID、用户和预期状态: %s", update)
	}
	if !strings.Contains(update, "NULLIF($5::text, '') IS NULL OR NULLIF(torrent_hash, '') IS NULL OR torrent_hash=$5") {
		t.Fatalf("CAS UPDATE 未拒绝覆盖不同 Hash: %s", update)
	}
	if !strings.Contains(update, "torrent_hash=COALESCE(NULLIF(torrent_hash, ''), NULLIF($5::text, ''))") {
		t.Fatalf("CAS UPDATE 未优先保留既有 Hash: %s", update)
	}
	stateQuery := compactDatabaseIntentSQL(getDownloadIntentStateSQL)
	if !strings.Contains(stateQuery, "WHERE id=$1 AND username=$2") {
		t.Fatalf("CAS 冲突判定未限定用户: %s", stateQuery)
	}
}

func TestDownloadIntentPersistenceErrorsAreSanitized(t *testing.T) {
	t.Parallel()

	intent := mustDatabaseDownloadIntent(t, "alice", "opaque-unavailable-store")
	var store *Store
	if _, err := store.CreateDownloadIntent(t.Context(), "alice", intent); err != ErrDownloadIntentPersistence {
		t.Fatalf("未初始化 Store 创建错误 = %v, want ErrDownloadIntentPersistence", err)
	}
	if _, err := store.GetDownloadIntent(t.Context(), "alice", intent.ID); err != ErrDownloadIntentPersistence {
		t.Fatalf("未初始化 Store 读取错误 = %v, want ErrDownloadIntentPersistence", err)
	}
	if _, err := store.CompareAndSwapDownloadIntent(
		t.Context(),
		"alice",
		intent.ID,
		[]downloader.IntentState{downloader.IntentPlanned},
		downloader.IntentSubmitting,
		"",
	); err != ErrDownloadIntentPersistence {
		t.Fatalf("未初始化 Store CAS 错误 = %v, want ErrDownloadIntentPersistence", err)
	}
	var tx pgx.Tx
	initializedStore := &Store{}
	if _, err := initializedStore.CreateDownloadIntentTx(t.Context(), tx, "alice", intent); err != ErrDownloadIntentPersistence {
		t.Fatalf("空事务创建错误 = %v, want ErrDownloadIntentPersistence", err)
	}
	if _, err := initializedStore.GetDownloadIntentTx(t.Context(), tx, "alice", intent.ID); err != ErrDownloadIntentPersistence {
		t.Fatalf("空事务读取错误 = %v, want ErrDownloadIntentPersistence", err)
	}
	if _, err := initializedStore.CompareAndSwapDownloadIntentTx(
		t.Context(),
		tx,
		"alice",
		intent.ID,
		[]downloader.IntentState{downloader.IntentPlanned},
		downloader.IntentSubmitting,
		"",
	); err != ErrDownloadIntentPersistence {
		t.Fatalf("空事务 CAS 错误 = %v, want ErrDownloadIntentPersistence", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.GetDownloadIntent(canceled, "alice", intent.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文读取错误 = %v, want context.Canceled", err)
	}
	if err := downloadIntentPersistenceError(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文持久化错误 = %v, want context.Canceled", err)
	}
	if err := downloadIntentPersistenceError(t.Context()); err != ErrDownloadIntentPersistence {
		t.Fatalf("普通持久化错误 = %v, want ErrDownloadIntentPersistence", err)
	}
}

func TestScanDownloadIntentRejectsInvalidStoredData(t *testing.T) {
	t.Parallel()

	valid := mustDatabaseDownloadIntent(t, "alice", "stored-intent")
	tests := []struct {
		name   string
		mutate func(*downloader.Intent)
	}{
		{name: "nil id", mutate: func(intent *downloader.Intent) { intent.ID = uuid.Nil }},
		{name: "invalid candidate key", mutate: func(intent *downloader.Intent) { intent.CandidateKey = "raw-key" }},
		{name: "invalid label", mutate: func(intent *downloader.Intent) { intent.Label = "manual-label" }},
		{name: "invalid state", mutate: func(intent *downloader.Intent) { intent.State = downloader.IntentState("corrupt") }},
		{name: "downloading without hash", mutate: func(intent *downloader.Intent) { intent.State = downloader.IntentDownloading }},
		{name: "invalid torrent hash", mutate: func(intent *downloader.Intent) { intent.TorrentHash = "not-a-hash" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stored := valid
			tt.mutate(&stored)
			row := scannerFunc(func(dest ...any) error {
				*dest[0].(*uuid.UUID) = stored.ID
				*dest[1].(*string) = stored.CandidateKey
				*dest[2].(*string) = stored.Label
				*dest[3].(*downloader.IntentState) = stored.State
				*dest[4].(*string) = stored.TorrentHash
				return nil
			})
			if _, err := scanDownloadIntent(row); !errors.Is(err, errStoredDownloadIntentInvalid) {
				t.Fatalf("scanDownloadIntent() error = %v, want errStoredDownloadIntentInvalid", err)
			}
		})
	}
}

func mustDatabaseDownloadIntent(t *testing.T, username, candidateKey string) downloader.Intent {
	t.Helper()
	intent, err := downloader.NewIntent(username, downloader.Candidate{Key: candidateKey})
	if err != nil {
		t.Fatalf("创建下载意图 fixture: %v", err)
	}
	return intent
}

func compactDatabaseIntentSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}
