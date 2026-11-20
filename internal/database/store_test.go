package database

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestNormalizeNewSubscription(t *testing.T) {
	t.Parallel()

	validInput := func(state subscription.State) NewSubscription {
		return NewSubscription{
			Username:    "alice",
			Name:        "Example Movie",
			MediaType:   "movie",
			Year:        "2026",
			MediaSource: "tmdb",
			MediaID:     "12345",
			State:       state,
			Config: map[string]any{
				"quality": "2160p",
				"enabled": true,
			},
		}
	}

	seasonOne := 1
	pendingSearchInput := validInput(subscription.PendingSearch)
	pendingSearchInput.Username = "  alice  "
	pendingSearchInput.Name = "\tExample Movie  "
	pendingSearchInput.MediaType = "  movie\t"
	pendingSearchInput.Year = "  2026 "
	pendingSearchInput.Season = &seasonOne
	pendingSearchInput.MediaSource = "  TMDb  "
	pendingSearchInput.MediaID = "  12345\t"
	pendingSearchWant := pendingSearchInput
	pendingSearchWant.Username = "alice"
	pendingSearchWant.Name = "Example Movie"
	pendingSearchWant.MediaType = "movie"
	pendingSearchWant.Year = "2026"
	pendingSearchWant.MediaSource = "tmdb"
	pendingSearchWant.MediaID = "12345"
	pendingSearchWant.Config = cloneSubscriptionTestConfig(pendingSearchInput.Config)

	pendingIdentificationInput := validInput(subscription.PendingIdentification)
	pendingIdentificationInput.MediaSource = ""
	pendingIdentificationInput.MediaID = ""
	pendingIdentificationInput.Config = nil
	pendingIdentificationWant := pendingIdentificationInput
	pendingIdentificationWant.Config = map[string]any{}
	pendingIdentificationWithConfig := validInput(subscription.PendingIdentification)
	pendingIdentificationWithConfigWant := pendingIdentificationWithConfig
	pendingIdentificationWithConfigWant.Config = cloneSubscriptionTestConfig(pendingIdentificationWithConfig.Config)

	missingUsername := validInput(subscription.PendingSearch)
	missingUsername.Username = " \t "
	missingName := validInput(subscription.PendingSearch)
	missingName.Name = "  "
	missingMediaType := validInput(subscription.PendingSearch)
	missingMediaType.MediaType = "\n"
	onlyMediaSource := validInput(subscription.PendingSearch)
	onlyMediaSource.MediaSource = " TMDB "
	onlyMediaSource.MediaID = " "
	onlyMediaID := validInput(subscription.PendingSearch)
	onlyMediaID.MediaSource = " "
	onlyMediaID.MediaID = " 12345 "
	negativeSeasonValue := -1
	negativeSeason := validInput(subscription.PendingSearch)
	negativeSeason.Season = &negativeSeasonValue
	unknownState := validInput(subscription.State("unknown"))

	tests := []struct {
		name    string
		input   NewSubscription
		want    NewSubscription
		wantErr bool
	}{
		{
			name:  "合法 PendingSearch 规范化空白和媒体来源",
			input: pendingSearchInput,
			want:  pendingSearchWant,
		},
		{
			name:  "合法 PendingIdentification 将 nil config 转为空 map",
			input: pendingIdentificationInput,
			want:  pendingIdentificationWant,
		},
		{
			name:  "合法 PendingIdentification 保留调用者 Config 内容",
			input: pendingIdentificationWithConfig,
			want:  pendingIdentificationWithConfigWant,
		},
		{
			name:    "拒绝缺少用户",
			input:   missingUsername,
			wantErr: true,
		},
		{
			name:    "拒绝缺少名称",
			input:   missingName,
			wantErr: true,
		},
		{
			name:    "拒绝缺少媒体类型",
			input:   missingMediaType,
			wantErr: true,
		},
		{
			name:    "拒绝只提供媒体来源",
			input:   onlyMediaSource,
			wantErr: true,
		},
		{
			name:    "拒绝只提供媒体 ID",
			input:   onlyMediaID,
			wantErr: true,
		},
		{
			name:    "拒绝负季号",
			input:   negativeSeason,
			wantErr: true,
		},
		{
			name:    "拒绝未知初始状态",
			input:   unknownState,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			callerConfigBefore := cloneSubscriptionTestConfig(tt.input.Config)
			got, err := normalizeNewSubscription(tt.input)
			if !reflect.DeepEqual(tt.input.Config, callerConfigBefore) {
				t.Fatalf("normalizeNewSubscription 原地修改了调用者 Config: got %#v, want %#v",
					tt.input.Config, callerConfigBefore)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeNewSubscription(%#v) error = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeNewSubscription(%#v) error = %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeNewSubscription() = %#v, want %#v", got, tt.want)
			}
			if got.Config == nil {
				t.Fatal("normalizeNewSubscription() 返回 nil Config")
			}
		})
	}
}

func TestResolveSubscriptionIdentity(t *testing.T) {
	t.Parallel()

	t.Run("首次插入成功", func(t *testing.T) {
		t.Parallel()
		refreshCalls := 0
		id, err := resolveSubscriptionIdentity(
			func() (int64, error) { return 11, nil },
			func() (int64, error) { refreshCalls++; return 0, nil },
		)
		if err != nil || id != 11 || refreshCalls != 0 {
			t.Fatalf("resolveSubscriptionIdentity() = id=%d err=%v refreshCalls=%d", id, err, refreshCalls)
		}
	})

	t.Run("普通冲突复用活动行", func(t *testing.T) {
		t.Parallel()
		insertCalls := 0
		id, err := resolveSubscriptionIdentity(
			func() (int64, error) { insertCalls++; return 0, pgx.ErrNoRows },
			func() (int64, error) { return 12, nil },
		)
		if err != nil || id != 12 || insertCalls != 1 {
			t.Fatalf("resolveSubscriptionIdentity() = id=%d err=%v insertCalls=%d", id, err, insertCalls)
		}
	})

	t.Run("冲突行在刷新前完成后重试插入", func(t *testing.T) {
		t.Parallel()
		insertCalls := 0
		refreshCalls := 0
		id, err := resolveSubscriptionIdentity(
			func() (int64, error) {
				insertCalls++
				if insertCalls == 1 {
					return 0, pgx.ErrNoRows
				}
				return 13, nil
			},
			func() (int64, error) { refreshCalls++; return 0, pgx.ErrNoRows },
		)
		if err != nil || id != 13 || insertCalls != 2 || refreshCalls != 1 {
			t.Fatalf("resolveSubscriptionIdentity() = id=%d err=%v insertCalls=%d refreshCalls=%d", id, err, insertCalls, refreshCalls)
		}
	})

	t.Run("持续变化有硬重试上限", func(t *testing.T) {
		t.Parallel()
		insertCalls := 0
		refreshCalls := 0
		_, err := resolveSubscriptionIdentity(
			func() (int64, error) { insertCalls++; return 0, pgx.ErrNoRows },
			func() (int64, error) { refreshCalls++; return 0, pgx.ErrNoRows },
		)
		if err == nil || insertCalls != 3 || refreshCalls != 3 {
			t.Fatalf("resolveSubscriptionIdentity() err=%v insertCalls=%d refreshCalls=%d", err, insertCalls, refreshCalls)
		}
	})
}

func TestPersistenceScannersRejectCorruptJSON(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	if _, err := scanSubscription(scannerFunc(func(dest ...any) error {
		*dest[0].(*int64) = 7
		*dest[1].(*string) = "alice"
		*dest[2].(*string) = "Fixture"
		*dest[3].(*sql.NullString) = sql.NullString{String: "movie", Valid: true}
		*dest[4].(*sql.NullString) = sql.NullString{String: "2026", Valid: true}
		*dest[5].(**int) = nil
		*dest[6].(*sql.NullString) = sql.NullString{String: "tmdb", Valid: true}
		*dest[7].(*sql.NullString) = sql.NullString{String: "42", Valid: true}
		*dest[8].(*subscription.State) = subscription.PendingSearch
		*dest[9].(*string) = "P"
		*dest[10].(*[]byte) = []byte("{")
		*dest[11].(**time.Time) = nil
		*dest[12].(*time.Time) = now
		*dest[13].(*time.Time) = now
		return nil
	})); err == nil || !strings.Contains(err.Error(), "配置") {
		t.Fatalf("scanSubscription(corrupt config) error = %v", err)
	}

	store := &Store{}
	if _, err := store.scanUser(scannerFunc(func(dest ...any) error {
		*dest[0].(*int64) = 3
		*dest[1].(*string) = "alice"
		*dest[2].(*string) = "password-hash"
		*dest[3].(*bool) = false
		*dest[4].(**string) = nil
		*dest[5].(*[]byte) = []byte("[")
		return nil
	})); err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("scanUser(corrupt permissions) error = %v", err)
	}
}

type scannerFunc func(...any) error

func (scan scannerFunc) Scan(dest ...any) error {
	return scan(dest...)
}

func cloneSubscriptionTestConfig(config map[string]any) map[string]any {
	if config == nil {
		return nil
	}
	cloned := make(map[string]any, len(config))
	for key, value := range config {
		cloned[key] = value
	}
	return cloned
}

func TestBuildSubscriptionListQuery(t *testing.T) {
	t.Parallel()

	const columns = `SELECT id, username, title, media_type, year, season, media_source, media_id, internal_state, v2_state, config, completed_at, created_at, updated_at FROM subscriptions`
	tests := []struct {
		name      string
		username  string
		admin     bool
		completed bool
		wantQuery string
		wantArgs  []any
	}{
		{
			name:      "普通用户活动订阅",
			username:  "alice",
			wantQuery: columns + ` WHERE completed_at IS NULL AND username=$1 ORDER BY updated_at DESC, id DESC`,
			wantArgs:  []any{"alice"},
		},
		{
			name:      "管理员活动订阅",
			admin:     true,
			wantQuery: columns + ` WHERE completed_at IS NULL ORDER BY updated_at DESC, id DESC`,
			wantArgs:  []any{},
		},
		{
			name:      "普通用户完成历史",
			username:  "bob",
			completed: true,
			wantQuery: columns + ` WHERE completed_at IS NOT NULL AND username=$1 ORDER BY completed_at DESC, id DESC`,
			wantArgs:  []any{"bob"},
		},
		{
			name:      "管理员完成历史",
			admin:     true,
			completed: true,
			wantQuery: columns + ` WHERE completed_at IS NOT NULL ORDER BY completed_at DESC, id DESC`,
			wantArgs:  []any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotQuery, gotArgs := buildSubscriptionListQuery(tt.username, tt.admin, tt.completed)
			if gotQuery != tt.wantQuery {
				t.Fatalf("buildSubscriptionListQuery() query = %q, want %q", gotQuery, tt.wantQuery)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("buildSubscriptionListQuery() args = %#v, want %#v", gotArgs, tt.wantArgs)
			}
		})
	}
}

func TestBuildCompletedSubscriptionPageQuery(t *testing.T) {
	t.Parallel()

	const columns = `SELECT id, username, title, media_type, year, season, media_source, media_id, internal_state, v2_state, config, completed_at, created_at, updated_at FROM subscriptions`
	tests := []struct {
		name      string
		username  string
		admin     bool
		mediaType string
		page      int
		count     int
		wantQuery string
		wantArgs  []any
		wantErr   bool
	}{
		{
			name:     "普通用户在数据库内按 owner 和类型分页",
			username: " alice ", mediaType: " 电视剧 ", page: 2, count: 30,
			wantQuery: columns + ` WHERE completed_at IS NOT NULL AND media_type=$1 AND username=$2 ORDER BY completed_at DESC, id DESC LIMIT $3 OFFSET $4`,
			wantArgs:  []any{"电视剧", "alice", 30, int64(30)},
		},
		{
			name:  "管理员查询全局电影历史",
			admin: true, mediaType: "电影", page: 3, count: 20,
			wantQuery: columns + ` WHERE completed_at IS NOT NULL AND media_type=$1 ORDER BY completed_at DESC, id DESC LIMIT $2 OFFSET $3`,
			wantArgs:  []any{"电影", 20, int64(40)},
		},
		{name: "拒绝空媒体类型", username: "alice", page: 1, count: 30, wantErr: true},
		{name: "拒绝空普通用户", mediaType: "电影", page: 1, count: 30, wantErr: true},
		{name: "拒绝零页码", username: "alice", mediaType: "电影", count: 30, wantErr: true},
		{name: "拒绝过大页码", username: "alice", mediaType: "电影", page: maxSubscriptionHistoryPage + 1, count: 30, wantErr: true},
		{name: "拒绝过大分页", username: "alice", mediaType: "电影", page: 1, count: 101, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotQuery, gotArgs, err := buildCompletedSubscriptionPageQuery(tt.username, tt.admin, tt.mediaType, tt.page, tt.count)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidSubscriptionHistoryPage) {
					t.Fatalf("error = %v, want ErrInvalidSubscriptionHistoryPage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if gotQuery != tt.wantQuery {
				t.Fatalf("query = %q, want %q", gotQuery, tt.wantQuery)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("args = %#v, want %#v", gotArgs, tt.wantArgs)
			}
		})
	}
}

func TestSubscriptionV2HistoryUsesCompletionTimeAndTVDefaults(t *testing.T) {
	t.Setenv("MOVIEPILOT_TIMEZONE", "Asia/Shanghai")
	completedAt := time.Date(2026, time.August, 11, 9, 30, 45, 0, time.UTC)
	item := Subscription{
		ID: 8, Name: "示例剧", MediaType: "电视剧", V2State: "", CompletedAt: &completedAt,
		CreatedAt: completedAt.Add(-time.Hour), UpdatedAt: completedAt,
		Config: map[string]any{"total_episode": 12, "lack_episode": 2, "completed_episode": 10},
	}

	got := item.V2History()
	if got["date"] != "2026-08-11 17:30:45" {
		t.Fatalf("date = %#v, want completion time", got["date"])
	}
	if got["state"] != nil || got["total_episode"] != 0 || got["lack_episode"] != 0 || got["completed_episode"] != 10 {
		t.Fatalf("history projection = %#v", got)
	}
}

func TestCompletedSubscriptionEpisodeMatchesV2HistoryRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fields map[string]any
		want   any
	}{
		{name: "普通订阅按缺集计算", fields: map[string]any{"total_episode": float64(12), "lack_episode": float64(2)}, want: 10},
		{name: "全集洗版达到优先级", fields: map[string]any{"total_episode": 12, "start_episode": 3, "best_version": 1, "best_version_full": 1, "current_priority": 100}, want: 12},
		{name: "全集洗版尚未达到优先级", fields: map[string]any{"total_episode": 12, "start_episode": 3, "best_version": 1, "best_version_full": 1, "current_priority": 90}, want: 2},
		{name: "分集洗版按目标范围计数", fields: map[string]any{"total_episode": 5, "start_episode": 3, "best_version": 1, "episode_priority": map[string]any{"2": float64(100), "3": float64(100), "4": float64(90), "5": float64(100)}}, want: 4},
		{name: "旧洗版快照用整体优先级回填", fields: map[string]any{"total_episode": 5, "start_episode": 3, "best_version": 1, "current_priority": 100}, want: 5},
		{name: "布尔洗版与字符串优先级", fields: map[string]any{"total_episode": 3, "best_version": true, "episode_priority": map[string]any{"1": "100", "2": "90", "3": "bad"}}, want: 1},
		{name: "坏项不覆盖已有按集事实", fields: map[string]any{"total_episode": 3, "best_version": true, "current_priority": 100, "episode_priority": map[string]any{"1": "100", "2": nil, "bad": 100}}, want: 1},
		{name: "全集布尔洗版按整体完成", fields: map[string]any{"total_episode": 3, "best_version": true, "best_version_full": true, "current_priority": 100}, want: 3},
		{name: "冲突别名不猜测完成计数", fields: map[string]any{"total_episode": 3, "best_version": 1, "episode_priority": map[string]any{"1": 100, "01": 90}}, want: nil},
		{name: "没有总集数不派生", fields: map[string]any{}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := completedSubscriptionEpisode(tt.fields); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("completedSubscriptionEpisode() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestBuildDeleteSubscriptionQuerySeparatesActiveAndHistory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		admin     bool
		completed bool
		wantQuery string
		wantArgs  []any
	}{
		{name: "普通用户活动订阅", wantQuery: `DELETE FROM subscriptions WHERE id=$1 AND completed_at IS NULL AND username=$2`, wantArgs: []any{int64(7), "alice"}},
		{name: "普通用户历史", completed: true, wantQuery: `DELETE FROM subscriptions WHERE id=$1 AND completed_at IS NOT NULL AND username=$2`, wantArgs: []any{int64(7), "alice"}},
		{name: "管理员活动订阅", admin: true, wantQuery: `DELETE FROM subscriptions WHERE id=$1 AND completed_at IS NULL`, wantArgs: []any{int64(7)}},
		{name: "管理员历史", admin: true, completed: true, wantQuery: `DELETE FROM subscriptions WHERE id=$1 AND completed_at IS NOT NULL`, wantArgs: []any{int64(7)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query, args := buildDeleteSubscriptionQuery(7, " alice ", tt.admin, tt.completed)
			if query != tt.wantQuery || !reflect.DeepEqual(args, tt.wantArgs) {
				t.Fatalf("query=%q args=%#v, want query=%q args=%#v", query, args, tt.wantQuery, tt.wantArgs)
			}
		})
	}
}

func TestBuildSubscriptionStateUpdate(t *testing.T) {
	t.Parallel()

	const (
		ownerQuery = `UPDATE subscriptions SET internal_state=$1, v2_state=$2, updated_at=now() WHERE id=$3 AND completed_at IS NULL AND internal_state<>$4 AND username=$5`
		adminQuery = `UPDATE subscriptions SET internal_state=$1, v2_state=$2, updated_at=now() WHERE id=$3 AND completed_at IS NULL AND internal_state<>$4`
	)
	tests := []struct {
		name      string
		id        int64
		username  string
		admin     bool
		state     string
		wantQuery string
		wantArgs  []any
		wantErr   bool
	}{
		{
			name:      "P 映射为待识别并保留 owner 与 Completed 过滤",
			id:        41,
			username:  "alice",
			state:     "P",
			wantQuery: ownerQuery,
			wantArgs:  []any{subscription.PendingIdentification, "P", int64(41), subscription.Completed, "alice"},
		},
		{
			name:      "R 映射为 Active 并保留 owner 与 Completed 过滤",
			id:        42,
			username:  "bob",
			state:     "R",
			wantQuery: ownerQuery,
			wantArgs:  []any{subscription.Active, "R", int64(42), subscription.Completed, "bob"},
		},
		{
			name:      "S 映射为暂停且管理员仍排除 Completed",
			id:        43,
			username:  "ignored-for-admin",
			admin:     true,
			state:     "S",
			wantQuery: adminQuery,
			wantArgs:  []any{subscription.Paused, "S", int64(43), subscription.Completed},
		},
		{
			name:     "N 不是控制状态",
			id:       44,
			username: "alice",
			state:    "N",
			wantErr:  true,
		},
		{
			name:     "空控制状态被拒绝",
			id:       45,
			username: "alice",
			state:    "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotQuery, gotArgs, err := buildSubscriptionStateUpdate(tt.id, tt.username, tt.admin, tt.state)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidSubscriptionState) {
					t.Fatalf("buildSubscriptionStateUpdate() error = %v, want ErrInvalidSubscriptionState", err)
				}
				if gotQuery != "" || gotArgs != nil {
					t.Fatalf("非法状态仍生成更新: query=%q args=%#v", gotQuery, gotArgs)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildSubscriptionStateUpdate() error = %v", err)
			}
			if gotQuery != tt.wantQuery {
				t.Fatalf("buildSubscriptionStateUpdate() query = %q, want %q", gotQuery, tt.wantQuery)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("buildSubscriptionStateUpdate() args = %#v, want %#v", gotArgs, tt.wantArgs)
			}
		})
	}
}

func TestBuildCompleteSubscriptionUpdate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		id        int64
		username  string
		admin     bool
		wantQuery string
		wantArgs  []any
	}{
		{
			name:      "普通用户按 owner 完成",
			id:        51,
			username:  "alice",
			wantQuery: `UPDATE subscriptions SET internal_state=$1, v2_state=$2, completed_at=now(), updated_at=now() WHERE id=$3 AND completed_at IS NULL AND internal_state=$4 AND username=$5`,
			wantArgs:  []any{subscription.Completed, "", int64(51), subscription.Indexed, "alice"},
		},
		{
			name:      "管理员完成任意 owner",
			id:        52,
			username:  "ignored",
			admin:     true,
			wantQuery: `UPDATE subscriptions SET internal_state=$1, v2_state=$2, completed_at=now(), updated_at=now() WHERE id=$3 AND completed_at IS NULL AND internal_state=$4`,
			wantArgs:  []any{subscription.Completed, "", int64(52), subscription.Indexed},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotQuery, gotArgs := buildCompleteSubscriptionUpdate(tt.id, tt.username, tt.admin)
			if gotQuery != tt.wantQuery {
				t.Fatalf("buildCompleteSubscriptionUpdate() query = %q, want %q", gotQuery, tt.wantQuery)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("buildCompleteSubscriptionUpdate() args = %#v, want %#v", gotArgs, tt.wantArgs)
			}
		})
	}
}
