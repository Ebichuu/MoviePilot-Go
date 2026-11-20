package database

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
)

const (
	downloadIntentPlanTestMasterKey    = "download-intent-plan-test-master-key"
	downloadIntentPlanTestDownloaderID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func TestNormalizeDownloadIntentPlanInput(t *testing.T) {
	t.Parallel()

	base := DownloadIntentPlan{
		IntentID:        uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		SiteID:          42,
		DownloaderID:    "  AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE  ",
		ResolverKind:    "  HDDolby_V2  ",
		ResolverVersion: 2,
		ResourceID:      "  torrent-123.v2  ",
		ResourceSecret:  "https://private.example/download?id=123&passkey=fixture",
		SavePath:        "  D:\\Private Media\\Movies  ",
		Paused:          true,
		CreatedAt:       time.Date(2026, time.August, 11, 10, 30, 0, 0, time.UTC),
	}
	want := base
	want.DownloaderID = downloadIntentPlanTestDownloaderID
	want.ResolverKind = "hddolby_v2"
	want.ResourceID = "torrent-123.v2"
	want.SavePath = "D:\\Private Media\\Movies"
	want.CreatedAt = time.Time{}

	tests := []struct {
		name     string
		username string
		mutate   func(*DownloadIntentPlan)
		wantUser string
		want     DownloadIntentPlan
		wantErr  error
	}{
		{name: "规范化公开字段并保留不透明敏感定位信息", username: "  alice \t", wantUser: "alice", want: want},
		{name: "拒绝空用户名", username: " \t", wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝空意图 UUID", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.IntentID = uuid.Nil }, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝非正站点 ID", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.SiteID = 0 }, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝非法下载器 ID", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.DownloaderID = "default-qbittorrent" }, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝非正解析器版本", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.ResolverVersion = 0 }, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝解析器路径", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.ResolverKind = "resolver/v1" }, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝 URL 作为公开资源 ID", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.ResourceID = "https://private.example/123" }, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝查询串作为公开资源 ID", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.ResourceID = "123?passkey=fixture" }, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝过长公开资源 ID", username: "alice", mutate: func(plan *DownloadIntentPlan) {
			plan.ResourceID = strings.Repeat("a", maxDownloadIntentPlanResourceIDLength+1)
		}, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝过长保存路径", username: "alice", mutate: func(plan *DownloadIntentPlan) {
			plan.SavePath = strings.Repeat("a", maxDownloadIntentPlanSavePathLength+1)
		}, wantErr: ErrInvalidDownloadIntentPlan},
		{name: "拒绝包含 NUL 的保存路径", username: "alice", mutate: func(plan *DownloadIntentPlan) { plan.SavePath = "private\x00path" }, wantErr: ErrInvalidDownloadIntentPlan},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			input := base
			if tt.mutate != nil {
				tt.mutate(&input)
			}
			username, got, err := normalizeDownloadIntentPlanInput(tt.username, input)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("normalizeDownloadIntentPlanInput() error = %v, want %v", err, tt.wantErr)
				}
				if username != "" || !reflect.DeepEqual(got, DownloadIntentPlan{}) {
					t.Fatal("非法计划仍返回了规范化数据")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDownloadIntentPlanInput() error = %v", err)
			}
			if username != tt.wantUser || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("规范化结果不符合预期: username=%q plan=%#v", username, got)
			}
		})
	}
}

func TestDownloadIntentPlanSQLIsOwnerScopedAndImmutable(t *testing.T) {
	t.Parallel()

	createQuery := compactDatabaseIntentSQL(createDownloadIntentPlanSQL)
	for _, required := range []string{
		"FROM download_intents AS di WHERE di.id=$1 AND di.username=$2",
		"ON CONFLICT (intent_id) DO NOTHING",
		"resource_secret",
	} {
		if !strings.Contains(createQuery, required) {
			t.Fatalf("计划创建 SQL 缺少约束 %q: %s", required, createQuery)
		}
	}
	if strings.Contains(strings.ToUpper(createQuery), "DO UPDATE") {
		t.Fatalf("不可变计划创建 SQL 包含 UPDATE: %s", createQuery)
	}

	getQuery := compactDatabaseIntentSQL(getDownloadIntentPlanSQL)
	for _, required := range []string{
		"JOIN download_intents AS di ON di.id=p.intent_id",
		"WHERE p.intent_id=$1 AND di.username=$2",
	} {
		if !strings.Contains(getQuery, required) {
			t.Fatalf("计划读取 SQL 缺少 owner 约束 %q: %s", required, getQuery)
		}
	}
}

func TestDownloadIntentPlanMigrationConstrainsPublicIdentity(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/010_download_intent_plans.sql")
	if err != nil {
		t.Fatalf("读取下载执行计划迁移: %v", err)
	}
	migration := string(content)
	for _, required := range []string{
		"intent_id UUID PRIMARY KEY REFERENCES download_intents(id) ON DELETE CASCADE",
		"site_id BIGINT NOT NULL REFERENCES sites(id) ON DELETE RESTRICT",
		"downloader_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'",
		"resolver_version > 0",
		"resource_id ~ '^(sha256:[0-9a-f]{64}|[A-Za-z0-9][A-Za-z0-9._-]*)$'",
		"resource_secret = '' OR resource_secret LIKE 'v1:%'",
		"octet_length(save_path) <= 4096",
	} {
		if !strings.Contains(migration, required) {
			t.Fatalf("计划迁移缺少安全约束 %q", required)
		}
	}
	for _, forbiddenColumn := range []string{"download_url", "cookie", "passkey", "metainfo"} {
		if strings.Contains(strings.ToLower(migration), forbiddenColumn+" ") {
			t.Fatalf("计划迁移新增了敏感明文列 %q", forbiddenColumn)
		}
	}
}

func TestScanDownloadIntentPlanDecryptsSecret(t *testing.T) {
	t.Parallel()

	box := mustDownloadIntentPlanTestSecretBox(t)
	const plainText = "https://private.example/download/fixture"
	encrypted, err := box.Encrypt(plainText)
	if err != nil {
		t.Fatalf("加密计划 fixture: %v", err)
	}
	createdAt := time.Date(2026, time.August, 11, 10, 45, 0, 0, time.UTC)
	row := downloadIntentPlanRowFunc(func(dest ...any) error {
		*dest[0].(*uuid.UUID) = uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
		*dest[1].(*int64) = 8
		*dest[2].(*string) = downloadIntentPlanTestDownloaderID
		*dest[3].(*string) = "hddolby"
		*dest[4].(*int16) = 1
		*dest[5].(*string) = "98765"
		*dest[6].(*string) = encrypted
		*dest[7].(*string) = "D:\\Private"
		*dest[8].(*bool) = true
		*dest[9].(*time.Time) = createdAt
		priority := 90
		*dest[10].(**int) = &priority
		return nil
	})

	store := NewStore(nil, box)
	plan, err := store.scanDownloadIntentPlan(row)
	if err != nil {
		t.Fatalf("扫描并解密计划: %v", err)
	}
	if plan.ResourceSecret != plainText || plan.CreatedAt != createdAt || plan.MoviePriority == nil || *plan.MoviePriority != 90 {
		t.Fatalf("扫描计划未恢复内存安全模型: %#v", plan)
	}
	if strings.Contains(plan.ResourceSecret, encrypted) {
		t.Fatal("计划内存模型返回了密文")
	}
}

func TestDownloadIntentPlanSemanticEqualityIgnoresCreatedAt(t *testing.T) {
	t.Parallel()

	base := DownloadIntentPlan{
		IntentID:        uuid.MustParse("12121212-3434-5656-7878-909090909090"),
		SiteID:          3,
		DownloaderID:    downloadIntentPlanTestDownloaderID,
		ResolverKind:    "haidan",
		ResolverVersion: 1,
		ResourceID:      "resource-9",
		ResourceSecret:  "opaque-secret",
		SavePath:        "D:\\Media",
		Paused:          true,
		CreatedAt:       time.Now(),
	}

	tests := []struct {
		name   string
		mutate func(*DownloadIntentPlan)
		want   bool
	}{
		{name: "仅创建时间不同仍为同一语义", mutate: func(plan *DownloadIntentPlan) { plan.CreatedAt = time.Time{} }, want: true},
		{name: "站点不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.SiteID++ }},
		{name: "下载器不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.DownloaderID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff" }},
		{name: "解析器不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.ResolverKind = "hddolby" }},
		{name: "解析器版本不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.ResolverVersion++ }},
		{name: "公开资源 ID 不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.ResourceID += "-other" }},
		{name: "敏感定位信息不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.ResourceSecret += "-other" }},
		{name: "保存路径不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.SavePath += "\\Other" }},
		{name: "暂停策略不同冲突", mutate: func(plan *DownloadIntentPlan) { plan.Paused = !plan.Paused }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			other := base
			tt.mutate(&other)
			if got := sameDownloadIntentPlan(base, other); got != tt.want {
				t.Fatalf("sameDownloadIntentPlan() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDownloadIntentPlanErrorsAreSanitized(t *testing.T) {
	t.Parallel()

	const (
		username   = "private-owner-must-not-appear"
		secret     = "https://private.example/download?passkey=must-not-appear"
		savePath   = "D:\\Private\\Path-Must-Not-Appear"
		ciphertext = "v1:corrupt-ciphertext-must-not-appear"
	)
	plan := DownloadIntentPlan{
		IntentID:        uuid.MustParse("abababab-cdcd-efef-0101-232323232323"),
		SiteID:          1,
		DownloaderID:    downloadIntentPlanTestDownloaderID,
		ResolverKind:    "haidan",
		ResolverVersion: 1,
		ResourceID:      "resource-1",
		ResourceSecret:  secret,
		SavePath:        savePath,
	}

	var nilStore *Store
	if _, err := nilStore.CreateDownloadIntentPlan(t.Context(), username, plan); err != ErrDownloadIntentPlanPersistence {
		t.Fatalf("未初始化 Store 创建错误 = %v, want ErrDownloadIntentPlanPersistence", err)
	}
	if _, err := nilStore.GetDownloadIntentPlan(t.Context(), username, plan.IntentID); err != ErrDownloadIntentPlanPersistence {
		t.Fatalf("未初始化 Store 读取错误 = %v, want ErrDownloadIntentPlanPersistence", err)
	}

	store := NewStore(nil, mustDownloadIntentPlanTestSecretBox(t))
	var tx pgx.Tx
	if _, err := store.CreateDownloadIntentPlanTx(t.Context(), tx, username, plan); err != ErrDownloadIntentPlanPersistence {
		t.Fatalf("空事务创建计划错误 = %v, want ErrDownloadIntentPlanPersistence", err)
	}
	if _, err := store.GetDownloadIntentPlanTx(t.Context(), tx, username, plan.IntentID); err != ErrDownloadIntentPlanPersistence {
		t.Fatalf("空事务读取计划错误 = %v, want ErrDownloadIntentPlanPersistence", err)
	}
	querier := downloadIntentPlanQueryRower{row: downloadIntentPlanRowFunc(func(dest ...any) error {
		*dest[0].(*uuid.UUID) = plan.IntentID
		*dest[1].(*int64) = plan.SiteID
		*dest[2].(*string) = plan.DownloaderID
		*dest[3].(*string) = plan.ResolverKind
		*dest[4].(*int16) = plan.ResolverVersion
		*dest[5].(*string) = plan.ResourceID
		*dest[6].(*string) = ciphertext
		*dest[7].(*string) = plan.SavePath
		*dest[8].(*bool) = plan.Paused
		*dest[9].(*time.Time) = time.Now()
		return nil
	})}
	_, err := store.getDownloadIntentPlan(t.Context(), querier, username, plan.IntentID)
	if err != ErrDownloadIntentPlanPersistence {
		t.Fatalf("损坏密文读取错误 = %v, want ErrDownloadIntentPlanPersistence", err)
	}
	for _, forbidden := range []string{username, secret, savePath, ciphertext, "download_intent_plans", "SELECT"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("计划错误泄露敏感信息或 SQL: %q", forbidden)
		}
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := downloadIntentPlanPersistenceError(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文错误 = %v, want context.Canceled", err)
	}
}

type downloadIntentPlanRowFunc func(...any) error

func (scan downloadIntentPlanRowFunc) Scan(dest ...any) error {
	return scan(dest...)
}

type downloadIntentPlanQueryRower struct {
	row pgx.Row
}

func (q downloadIntentPlanQueryRower) QueryRow(context.Context, string, ...any) pgx.Row {
	return q.row
}

func mustDownloadIntentPlanTestSecretBox(t *testing.T) *auth.SecretBox {
	t.Helper()
	box, err := auth.NewSecretBox(downloadIntentPlanTestMasterKey)
	if err != nil {
		t.Fatalf("创建计划测试 SecretBox: %v", err)
	}
	return box
}
