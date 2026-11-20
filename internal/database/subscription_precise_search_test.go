package database

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestBuildListSubscriptionsForPreciseSearch(t *testing.T) {
	t.Parallel()

	cutoff := time.Date(2026, time.August, 11, 12, 30, 0, 0, time.FixedZone("test", 8*60*60))
	tests := []struct {
		name           string
		cutoff         time.Time
		afterUpdatedAt time.Time
		afterID        int64
		limit          int
		wantArgs       []any
		wantErr        error
	}{
		{
			name:     "接受首次有界批次",
			cutoff:   cutoff,
			limit:    100,
			wantArgs: []any{cutoff.UTC(), 100},
		},
		{
			name:           "接受完整 keyset 游标",
			cutoff:         cutoff,
			afterUpdatedAt: cutoff.Add(-time.Minute),
			afterID:        41,
			limit:          50,
			wantArgs: []any{
				cutoff.UTC(),
				cutoff.Add(-time.Minute).UTC(),
				int64(41),
				50,
			},
		},
		{name: "拒绝零截止时间", limit: 1, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝仅有时间游标", cutoff: cutoff, afterUpdatedAt: cutoff, limit: 1, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝仅有 ID 游标", cutoff: cutoff, afterID: 41, limit: 1, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝负 ID 游标", cutoff: cutoff, afterUpdatedAt: cutoff, afterID: -1, limit: 1, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝零条数", cutoff: cutoff, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝超过硬上限", cutoff: cutoff, limit: 101, wantErr: ErrInvalidSubscriptionPreciseSearch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query, args, err := buildListSubscriptionsForPreciseSearch(
				tt.cutoff,
				tt.afterUpdatedAt,
				tt.afterID,
				tt.limit,
			)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildListSubscriptionsForPreciseSearch() error = %v, want %v", err, tt.wantErr)
				}
				if query != "" || args != nil {
					t.Fatal("非法扫描参数仍生成了查询")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildListSubscriptionsForPreciseSearch() error = %v", err)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("扫描参数 = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

func TestListSubscriptionsForPreciseSearchSQLIsBoundedAndStable(t *testing.T) {
	t.Parallel()

	query := compactSubscriptionPreciseSearchSQL(listSubscriptionsForPreciseSearchSQL)
	for _, fragment := range []string{
		"internal_state='pending_search'",
		"completed_at IS NULL",
		"current_download_intent_id IS NULL",
		"updated_at <= $1",
		"ORDER BY updated_at, id",
		"LIMIT $2",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("精准搜索扫描缺少 %q: %s", fragment, query)
		}
	}
	afterQuery := compactSubscriptionPreciseSearchSQL(listSubscriptionsForPreciseSearchAfterSQL)
	if !strings.Contains(afterQuery, "(updated_at, id) > ($2, $3)") || !strings.Contains(afterQuery, "LIMIT $4") {
		t.Fatalf("精准搜索后续扫描未使用 keyset 游标: %s", afterQuery)
	}
}

func TestBuildGetActiveSubscription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		username string
		id       int64
		wantArgs []any
		wantErr  error
	}{
		{name: "规范化 owner", username: "  alice \t", id: 41, wantArgs: []any{int64(41), "alice"}},
		{name: "拒绝空 owner", username: " \t", id: 41, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝无效 ID", username: "alice", wantErr: ErrInvalidSubscriptionPreciseSearch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildGetActiveSubscription(tt.username, tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildGetActiveSubscription() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildGetActiveSubscription() error = %v", err)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("读取参数 = %#v, want %#v", args, tt.wantArgs)
			}
			query = compactSubscriptionPreciseSearchSQL(query)
			if !strings.Contains(query, "WHERE id=$1 AND username=$2 AND completed_at IS NULL") {
				t.Fatalf("活动订阅读取未限定 ID、owner 和完成边界: %s", query)
			}
		})
	}
}

func TestBuildCompareAndSwapSubscriptionPreciseSearchState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		username string
		id       int64
		next     subscription.State
		wantArgs []any
		wantErr  error
	}{
		{
			name:     "推进到 active",
			username: " alice ",
			id:       51,
			next:     subscription.Active,
			wantArgs: []any{int64(51), "alice", subscription.Active, "R", subscription.PendingSearch},
		},
		{
			name:     "推进到 acquired",
			username: "alice",
			id:       52,
			next:     subscription.Acquired,
			wantArgs: []any{int64(52), "alice", subscription.Acquired, "R", subscription.PendingSearch},
		},
		{
			name:     "暂停永久无效配置",
			username: "alice",
			id:       53,
			next:     subscription.Paused,
			wantArgs: []any{int64(53), "alice", subscription.Paused, "S", subscription.PendingSearch},
		},
		{name: "拒绝空 owner", id: 51, next: subscription.Active, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝无效 ID", username: "alice", next: subscription.Active, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝其他目标状态", username: "alice", id: 51, next: subscription.Downloading, wantErr: ErrInvalidSubscriptionPreciseSearch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildCompareAndSwapSubscriptionPreciseSearchState(tt.username, tt.id, tt.next)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildCompareAndSwapSubscriptionPreciseSearchState() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildCompareAndSwapSubscriptionPreciseSearchState() error = %v", err)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("CAS 参数 = %#v, want %#v", args, tt.wantArgs)
			}
			query = compactSubscriptionPreciseSearchSQL(query)
			for _, fragment := range []string{
				"WHERE id=$1 AND username=$2",
				"completed_at IS NULL",
				"internal_state=$5",
			} {
				if !strings.Contains(query, fragment) {
					t.Fatalf("精准搜索 CAS 缺少 %q: %s", fragment, query)
				}
			}
		})
	}
}

func TestBuildCompleteSatisfiedTVSubscription(t *testing.T) {
	t.Parallel()

	updatedAt := time.Date(2026, time.August, 15, 15, 0, 0, 0, time.FixedZone("test", 8*60*60))
	tests := []struct {
		name      string
		username  string
		id        int64
		expected  subscription.State
		updatedAt time.Time
		wantArgs  []any
		wantErr   error
	}{
		{
			name: "精准搜索完成媒体库已满足订阅", username: " alice ", id: 61,
			expected: subscription.PendingSearch, updatedAt: updatedAt,
			wantArgs: []any{
				int64(61), "alice", subscription.Completed, "",
				subscription.PendingSearch, updatedAt.UTC(), `{"lack_episode":0}`, "电视剧", "tv",
			},
		},
		{
			name: "RSS 完成媒体库已满足订阅", username: "alice", id: 62,
			expected: subscription.Active, updatedAt: updatedAt,
			wantArgs: []any{
				int64(62), "alice", subscription.Completed, "",
				subscription.Active, updatedAt.UTC(), `{"lack_episode":0}`, "电视剧", "tv",
			},
		},
		{
			name: "RSS 完成细分 R 状态订阅", username: "alice", id: 63,
			expected: subscription.Indexed, updatedAt: updatedAt,
			wantArgs: []any{
				int64(63), "alice", subscription.Completed, "",
				subscription.Indexed, updatedAt.UTC(), `{"lack_episode":0}`, "电视剧", "tv",
			},
		},
		{name: "拒绝空 owner", id: 61, expected: subscription.PendingSearch, updatedAt: updatedAt, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝无效 ID", username: "alice", expected: subscription.PendingSearch, updatedAt: updatedAt, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝 P 期望状态", username: "alice", id: 61, expected: subscription.PendingIdentification, updatedAt: updatedAt, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝 S 期望状态", username: "alice", id: 61, expected: subscription.Paused, updatedAt: updatedAt, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝零快照时间", username: "alice", id: 61, expected: subscription.Active, wantErr: ErrInvalidSubscriptionPreciseSearch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildCompleteSatisfiedTVSubscription(
				tt.username, tt.id, tt.expected, tt.updatedAt,
			)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildCompleteSatisfiedTVSubscription() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("完成参数 = %#v, want %#v", args, tt.wantArgs)
			}
			query = compactSubscriptionPreciseSearchSQL(query)
			for _, fragment := range []string{
				"completed_at=now()",
				"current_download_intent_id IS NULL",
				"jsonb_typeof(config)='object'",
				"internal_state=$5",
				"updated_at=$6",
				"config=config || $7::jsonb",
				"LOWER(BTRIM(media_type)) IN ($8, $9)",
				"config->'best_version'",
			} {
				if !strings.Contains(query, fragment) {
					t.Fatalf("满足完成 SQL 缺少 %q: %s", fragment, query)
				}
			}
		})
	}
}

func TestBuildCompleteSatisfiedMovieSubscription(t *testing.T) {
	t.Parallel()

	updatedAt := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.FixedZone("test", 8*60*60))
	tests := []struct {
		name     string
		username string
		id       int64
		expected subscription.State
		updated  time.Time
		wantArgs []any
		wantErr  error
	}{
		{
			name: "精准搜索完成媒体库已存在电影", username: " alice ", id: 71,
			expected: subscription.PendingSearch, updated: updatedAt,
			wantArgs: []any{
				int64(71), "alice", subscription.Completed, "",
				subscription.PendingSearch, updatedAt.UTC(), `{}`, "电影", "movie",
			},
		},
		{
			name: "RSS 完成媒体库已存在电影", username: "alice", id: 72,
			expected: subscription.Active, updated: updatedAt,
			wantArgs: []any{
				int64(72), "alice", subscription.Completed, "",
				subscription.Active, updatedAt.UTC(), `{}`, "电影", "movie",
			},
		},
		{name: "拒绝 P 期望状态", username: "alice", id: 73, expected: subscription.PendingIdentification, updated: updatedAt, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝洗版暂停状态", username: "alice", id: 73, expected: subscription.Paused, updated: updatedAt, wantErr: ErrInvalidSubscriptionPreciseSearch},
		{name: "拒绝零快照时间", username: "alice", id: 73, expected: subscription.Active, wantErr: ErrInvalidSubscriptionPreciseSearch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildCompleteSatisfiedMovieSubscription(
				tt.username, tt.id, tt.expected, tt.updated,
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("buildCompleteSatisfiedMovieSubscription() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("完成参数 = %#v, want %#v", args, tt.wantArgs)
			}
			for _, fragment := range []string{
				"completed_at=now()",
				"current_download_intent_id IS NULL",
				"internal_state=$5",
				"updated_at=$6",
				"LOWER(BTRIM(media_type)) IN ($8, $9)",
				"config->'best_version'",
			} {
				if !strings.Contains(compactSubscriptionPreciseSearchSQL(query), fragment) {
					t.Fatalf("满足完成 SQL 缺少 %q: %s", fragment, query)
				}
			}
		})
	}
}

func TestBuildBindDownloadIntentSubscription(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	tests := []struct {
		name           string
		username       string
		intentID       uuid.UUID
		subscriptionID int64
		wantArgs       []any
		wantErr        error
	}{
		{name: "规范化 owner", username: " alice ", intentID: intentID, subscriptionID: 61, wantArgs: []any{intentID, "alice", int64(61)}},
		{name: "拒绝空 owner", intentID: intentID, subscriptionID: 61, wantErr: ErrInvalidDownloadIntentSubscriptionBinding},
		{name: "拒绝空意图 ID", username: "alice", subscriptionID: 61, wantErr: ErrInvalidDownloadIntentSubscriptionBinding},
		{name: "拒绝无效订阅 ID", username: "alice", intentID: intentID, wantErr: ErrInvalidDownloadIntentSubscriptionBinding},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := buildBindDownloadIntentSubscription(tt.username, tt.intentID, tt.subscriptionID)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildBindDownloadIntentSubscription() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildBindDownloadIntentSubscription() error = %v", err)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("绑定参数 = %#v, want %#v", args, tt.wantArgs)
			}
			query = compactSubscriptionPreciseSearchSQL(query)
			for _, fragment := range []string{
				"di.id=$1 AND di.username=$2",
				"s.id=$3 AND s.username=$2 AND s.completed_at IS NULL",
				"di.subscription_id IS NULL OR di.subscription_id=s.id",
				"s.current_download_intent_id IS NULL OR s.current_download_intent_id=di.id",
				"SET current_download_intent_id=target.intent_id",
			} {
				if !strings.Contains(query, fragment) {
					t.Fatalf("意图绑定缺少 %q: %s", fragment, query)
				}
			}
		})
	}
}

func TestSubscriptionPreciseSearchPersistenceErrorsAreSanitized(t *testing.T) {
	t.Parallel()

	var store *Store
	cutoff := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	if _, err := store.ListSubscriptionsForPreciseSearch(t.Context(), cutoff, time.Time{}, 0, 1); err != ErrSubscriptionPreciseSearchPersistence {
		t.Fatalf("未初始化 Store 扫描错误 = %v", err)
	}
	if _, err := store.GetActiveSubscription(t.Context(), "sensitive-owner", 71); err != ErrSubscriptionPreciseSearchPersistence {
		t.Fatalf("未初始化 Store 读取错误 = %v", err)
	}
	if err := store.CompareAndSwapSubscriptionPreciseSearchState(t.Context(), "sensitive-owner", 71, subscription.Active); err != ErrSubscriptionPreciseSearchPersistence {
		t.Fatalf("未初始化 Store CAS 错误 = %v", err)
	}
	if err := store.CompleteSatisfiedTVSubscription(
		t.Context(), "sensitive-owner", 71, subscription.PendingSearch, cutoff,
	); err != ErrSubscriptionPreciseSearchPersistence {
		t.Fatalf("未初始化 Store 完成错误 = %v", err)
	}

	initializedStore := &Store{}
	var tx pgx.Tx
	if err := initializedStore.CompareAndSwapSubscriptionPreciseSearchStateTx(t.Context(), tx, "sensitive-owner", 71, subscription.Active); err != ErrSubscriptionPreciseSearchPersistence {
		t.Fatalf("空事务 CAS 错误 = %v", err)
	}
	intentID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if err := initializedStore.BindDownloadIntentSubscriptionTx(t.Context(), tx, "sensitive-owner", intentID, 71); err != ErrDownloadIntentSubscriptionBindingPersistence {
		t.Fatalf("空事务绑定错误 = %v", err)
	}

	for _, err := range []error{
		ErrSubscriptionPreciseSearchPersistence,
		ErrDownloadIntentSubscriptionBindingPersistence,
	} {
		for _, sensitive := range []string{"sensitive-owner", "71", intentID.String(), "subscriptions", "download_intents", "SELECT", "UPDATE"} {
			if strings.Contains(err.Error(), sensitive) {
				t.Fatalf("脱敏错误 %q 泄露了 %q", err, sensitive)
			}
		}
	}
}

func TestSubscriptionPreciseSearchMigrationDefinesPartialIndex(t *testing.T) {
	t.Parallel()

	content, err := migrationFS.ReadFile("migrations/013_subscription_precise_search_index.sql")
	if err != nil {
		t.Fatalf("读取精准搜索索引迁移: %v", err)
	}
	query := compactSubscriptionPreciseSearchSQL(string(content))
	for _, fragment := range []string{
		"ON subscriptions (updated_at, id)",
		"WHERE internal_state = 'pending_search' AND completed_at IS NULL",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("精准搜索索引迁移缺少 %q: %s", fragment, query)
		}
	}
}

func compactSubscriptionPreciseSearchSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}
