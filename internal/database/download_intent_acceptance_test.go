package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestNormalizeDownloadIntentAcceptance(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	v2Hash := strings.Repeat("ab", 32)
	tests := []struct {
		name         string
		username     string
		intentID     uuid.UUID
		torrentHash  string
		wantUsername string
		wantHash     string
		wantErr      error
	}{
		{
			name:         "规范化 owner 和 v1 hash",
			username:     "  alice\t",
			intentID:     intentID,
			torrentHash:  "  " + strings.ToLower(databaseIntentV1Hash) + "\n",
			wantUsername: "alice",
			wantHash:     databaseIntentV1Hash,
		},
		{
			name:         "规范化 v2 hash",
			username:     "alice",
			intentID:     intentID,
			torrentHash:  v2Hash,
			wantUsername: "alice",
			wantHash:     strings.ToUpper(v2Hash),
		},
		{name: "拒绝空 owner", intentID: intentID, torrentHash: databaseIntentV1Hash, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝空 intent id", username: "alice", torrentHash: databaseIntentV1Hash, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝空 hash", username: "alice", intentID: intentID, wantErr: ErrInvalidDownloadIntent},
		{name: "拒绝非十六进制 hash", username: "alice", intentID: intentID, torrentHash: strings.Repeat("z", 40), wantErr: ErrInvalidDownloadIntent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			username, hash, err := normalizeDownloadIntentAcceptance(
				tt.username,
				tt.intentID,
				tt.torrentHash,
			)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("normalizeDownloadIntentAcceptance() error = %v, want %v", err, tt.wantErr)
				}
				if username != "" || hash != "" {
					t.Fatalf("非法输入返回 owner/hash = (%q, %q)", username, hash)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDownloadIntentAcceptance() error = %v", err)
			}
			if username != tt.wantUsername || hash != tt.wantHash {
				t.Fatalf(
					"normalizeDownloadIntentAcceptance() = (%q, %q), want (%q, %q)",
					username,
					hash,
					tt.wantUsername,
					tt.wantHash,
				)
			}
		})
	}
}

func TestAcceptedDownloadIntent(t *testing.T) {
	t.Parallel()

	base := mustDatabaseDownloadIntent(t, "alice", "acceptance-state")
	differentHash := "FEDCBA9876543210FEDCBA9876543210FEDCBA98"
	tests := []struct {
		name       string
		state      downloader.IntentState
		storedHash string
		acceptHash string
		wantState  downloader.IntentState
		wantHash   string
		wantErr    error
	}{
		{
			name:       "submitting 首次接受",
			state:      downloader.IntentSubmitting,
			acceptHash: databaseIntentV1Hash,
			wantState:  downloader.IntentDownloading,
			wantHash:   databaseIntentV1Hash,
		},
		{
			name:       "submitting 相同 hash 重放",
			state:      downloader.IntentSubmitting,
			storedHash: strings.ToLower(databaseIntentV1Hash),
			acceptHash: databaseIntentV1Hash,
			wantState:  downloader.IntentDownloading,
			wantHash:   databaseIntentV1Hash,
		},
		{
			name:       "downloading 相同 hash 重放",
			state:      downloader.IntentDownloading,
			storedHash: databaseIntentV1Hash,
			acceptHash: databaseIntentV1Hash,
			wantState:  downloader.IntentDownloading,
			wantHash:   databaseIntentV1Hash,
		},
		{
			name:       "拒绝不同 hash",
			state:      downloader.IntentSubmitting,
			storedHash: databaseIntentV1Hash,
			acceptHash: differentHash,
			wantErr:    ErrDownloadIntentStateConflict,
		},
		{name: "拒绝 planned", state: downloader.IntentPlanned, acceptHash: databaseIntentV1Hash, wantErr: ErrDownloadIntentStateConflict},
		{name: "拒绝 completed", state: downloader.IntentCompleted, storedHash: databaseIntentV1Hash, acceptHash: databaseIntentV1Hash, wantErr: ErrDownloadIntentStateConflict},
		{name: "拒绝 failed", state: downloader.IntentFailed, storedHash: databaseIntentV1Hash, acceptHash: databaseIntentV1Hash, wantErr: ErrDownloadIntentStateConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			intent := base
			intent.State = tt.state
			intent.TorrentHash = tt.storedHash
			accepted, err := acceptedDownloadIntent(intent, tt.acceptHash)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("acceptedDownloadIntent() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("acceptedDownloadIntent() error = %v", err)
			}
			if accepted.ID != base.ID || accepted.State != tt.wantState || accepted.TorrentHash != tt.wantHash {
				t.Fatalf("acceptedDownloadIntent() = %#v", accepted)
			}
		})
	}
}

func TestDownloadIntentAcceptanceCanSettleParent(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	otherIntentID := uuid.MustParse("11111111-aaaa-bbbb-cccc-222222222222")
	tests := []struct {
		name      string
		state     subscription.State
		current   *uuid.UUID
		completed bool
		want      bool
	}{
		{name: "acquired current intent", state: subscription.Acquired, current: &intentID, want: true},
		{name: "downloading current intent", state: subscription.Downloading, current: &intentID, want: true},
		{name: "旧 intent 不结算新 current", state: subscription.Acquired, current: &otherIntentID},
		{name: "已释放 current 不重复结算", state: subscription.PendingSearch},
		{name: "已完成 parent 不重复结算", state: subscription.Completed, current: &intentID, completed: true},
		{name: "legacy pending organization 不前移", state: subscription.PendingOrganization, current: &intentID},
		{name: "legacy organizing 不前移", state: subscription.Organizing, current: &intentID},
		{name: "legacy pending index 不前移", state: subscription.PendingIndex, current: &intentID},
		{name: "legacy indexed 不前移", state: subscription.Indexed, current: &intentID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := downloadIntentAcceptanceSubscription{
				state:         tt.state,
				currentIntent: tt.current,
				completed:     tt.completed,
			}
			if got := downloadIntentAcceptanceCanSettleParent(parent, intentID); got != tt.want {
				t.Fatalf("downloadIntentAcceptanceCanSettleParent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDownloadIntentAcceptanceCanPreservePending(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("abababab-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	otherIntentID := uuid.MustParse("cdcdcdcd-dddd-4eee-8fff-111111111111")
	tests := []struct {
		name      string
		state     subscription.State
		current   *uuid.UUID
		completed bool
		want      bool
	}{
		{name: "pending-identification current intent", state: subscription.PendingIdentification, current: &intentID, want: true},
		{name: "active is normal completion path", state: subscription.Active, current: &intentID},
		{name: "old intent", state: subscription.PendingIdentification, current: &otherIntentID},
		{name: "released current", state: subscription.PendingIdentification},
		{name: "completed parent", state: subscription.PendingIdentification, current: &intentID, completed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := downloadIntentAcceptanceSubscription{
				state: tt.state, currentIntent: tt.current, completed: tt.completed,
			}
			if got := downloadIntentAcceptanceCanPreservePending(parent, intentID); got != tt.want {
				t.Fatalf("downloadIntentAcceptanceCanPreservePending() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDownloadIntentAcceptanceParentState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mediaType string
		missing   int64
		want      subscription.State
		wantErr   error
	}{
		{name: "电影直接完成", mediaType: "电影", missing: 1, want: subscription.Completed},
		{name: "英文 movie 兼容", mediaType: " Movie ", want: subscription.Completed},
		{name: "TV 无缺集完成", mediaType: "电视剧", want: subscription.Completed},
		{name: "TV 仍缺集继续搜索", mediaType: "电视剧", missing: 2, want: subscription.PendingSearch},
		{name: "英文 tv 兼容", mediaType: " TV ", missing: 1, want: subscription.PendingSearch},
		{name: "拒绝未知媒体类型", mediaType: "unknown", wantErr: ErrDownloadIntentPersistence},
		{name: "拒绝负缺集数", mediaType: "电视剧", missing: -1, wantErr: ErrDownloadIntentPersistence},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := downloadIntentAcceptanceParentState(tt.mediaType, tt.missing)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("downloadIntentAcceptanceParentState() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("downloadIntentAcceptanceParentState() = (%q, %v), want (%q, nil)", got, err, tt.want)
			}
		})
	}
}

func TestDownloadIntentAcceptanceUnknownTVTargetCannotComplete(t *testing.T) {
	t.Parallel()
	got, err := downloadIntentAcceptanceParentState("电视剧", 0, false)
	if err != nil || got != subscription.PendingSearch {
		t.Fatalf("unknown target state=(%q,%v), want pending_search", got, err)
	}
}

func TestDownloadIntentAcceptanceTVTargetUsesCurrentSeasonAndRange(t *testing.T) {
	t.Parallel()
	season := 3
	target, legacy, err := downloadIntentAcceptanceTVTarget(&season, map[string]any{
		"start_episode": 2.0, "total_episode": 4.0, "lack_episode": 3.0,
		"manual_total_episode": 1.0, "note": []any{2.0},
	})
	want := []subscription.Episode{{Season: 3, Episode: 2}, {Season: 3, Episode: 3}, {Season: 3, Episode: 4}}
	if err != nil || legacy || !reflect.DeepEqual(target.TargetEpisodes, want) {
		t.Fatalf("target=%#v legacy=%v err=%v", target, legacy, err)
	}
	unknown, legacy, err := downloadIntentAcceptanceTVTarget(&season, map[string]any{})
	if err != nil || !legacy || unknown.Knowledge != subscription.TVTargetUnknown {
		t.Fatalf("legacy unknown=%#v legacy=%v err=%v", unknown, legacy, err)
	}
}

func TestBuildDownloadIntentAcceptanceProgressPatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mediaType string
		config    map[string]any
		consumed  []subscription.Episode
		missing   int64
		wantNote  []int
		wantLack  int
		wantEmpty bool
		wantErr   error
	}{
		{
			name:      "普通 TV 合并去重并排序",
			mediaType: "电视剧",
			config:    map[string]any{"note": []any{float64(3), "1", float64(3)}},
			consumed: []subscription.Episode{
				{Season: 2, Episode: 2},
				{Season: 2, Episode: 3},
			},
			missing:  4,
			wantNote: []int{1, 2, 3},
			wantLack: 4,
		},
		{
			name:      "普通电影记录单一条目",
			mediaType: " movie ",
			config:    map[string]any{"note": []any{float64(1), "invalid"}},
			missing:   9,
			wantNote:  []int{1},
		},
		{
			name:      "损坏旧 note 不阻塞并由接受事实修复",
			mediaType: "电视剧",
			config:    map[string]any{"note": "corrupt"},
			consumed:  []subscription.Episode{{Season: 1, Episode: 2}},
			missing:   3,
			wantNote:  []int{2},
			wantLack:  3,
		},
		{
			name:      "非法及越界旧 note 条目被忽略",
			mediaType: "电视剧",
			config: map[string]any{"note": []any{
				float64(-1), float64(0), float64(maxSubscriptionEpisodeNumber),
				float64(maxSubscriptionEpisodeNumber + 1), "invalid", float64(3),
			}},
			consumed: []subscription.Episode{{Season: 1, Episode: 2}},
			missing:  2,
			wantNote: []int{2, 3, maxSubscriptionEpisodeNumber},
			wantLack: 2,
		},
		{
			name:      "洗版留给独立优先级切片",
			mediaType: "电视剧",
			config:    map[string]any{"best_version": float64(1), "note": []any{float64(1)}},
			consumed:  []subscription.Episode{{Season: 1, Episode: 2}},
			wantEmpty: true,
		},
		{
			name:      "拒绝负缺集",
			mediaType: "电视剧",
			missing:   -1,
			wantErr:   ErrDownloadIntentPersistence,
		},
		{
			name:      "拒绝未知媒体类型",
			mediaType: "unknown",
			wantErr:   ErrDownloadIntentPersistence,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := buildDownloadIntentAcceptanceProgressPatch(
				tt.mediaType,
				tt.config,
				tt.consumed,
				tt.missing,
			)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("buildDownloadIntentAcceptanceProgressPatch() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildDownloadIntentAcceptanceProgressPatch() error = %v", err)
			}
			var patch struct {
				Note        []int `json:"note"`
				LackEpisode *int  `json:"lack_episode"`
			}
			if err := json.Unmarshal(encoded, &patch); err != nil {
				t.Fatalf("解析 progress patch: %v", err)
			}
			if tt.wantEmpty {
				if string(encoded) != "{}" {
					t.Fatalf("洗版 progress patch = %s, want {}", encoded)
				}
				return
			}
			if !reflect.DeepEqual(patch.Note, tt.wantNote) || patch.LackEpisode == nil || *patch.LackEpisode != tt.wantLack {
				t.Fatalf("progress patch = note:%v lack:%v, want note:%v lack:%d", patch.Note, patch.LackEpisode, tt.wantNote, tt.wantLack)
			}
		})
	}
}

func TestDownloadIntentAcceptanceSQLContracts(t *testing.T) {
	t.Parallel()

	intentLock := compactDatabaseIntentSQL(lockDownloadIntentForAcceptanceSQL)
	parentLock := compactDatabaseIntentSQL(lockDownloadIntentSubscriptionForAcceptanceSQL)
	episodeLock := compactDatabaseIntentSQL(lockDownloadIntentEpisodesForAcceptanceSQL)
	for name, query := range map[string]string{
		"intent":   intentLock,
		"parent":   parentLock,
		"episodes": episodeLock,
	} {
		if !strings.Contains(query, "FOR UPDATE") {
			t.Fatalf("%s acceptance query 未加行锁: %s", name, query)
		}
	}
	if !strings.Contains(intentLock, "WHERE di.id=$1 AND di.username=$2") ||
		!strings.Contains(intentLock, "LEFT JOIN download_intent_plans AS p ON p.intent_id=di.id") ||
		!strings.Contains(intentLock, "FOR UPDATE OF di") {
		t.Fatalf("intent acceptance 锁未限定 owner: %s", intentLock)
	}
	if !strings.Contains(episodeLock, "download_intent_id=$2 ORDER BY id FOR UPDATE") {
		t.Fatalf("episode acceptance 未按 intent 和稳定顺序加锁: %s", episodeLock)
	}

	consume := compactDatabaseIntentSQL(consumeDownloadIntentEpisodesSQL)
	if !strings.Contains(consume, "download_intent_id=$2") ||
		!strings.Contains(consume, "state IN ('acquired', 'downloading')") ||
		strings.Contains(consume, "download_intent_id IS NULL") {
		t.Fatalf("acceptance 消费越过自身 intent 边界: %s", consume)
	}
	currentMissing := compactDatabaseIntentSQL(countMissingCurrentTargetEpisodesSQL)
	for _, required := range []string{
		"$2::integer AS season",
		"unnest($3::integer[])",
		"LEFT JOIN subscription_episodes",
		"subscription_id=$1",
		"se.id IS NULL OR se.state='missing'",
	} {
		if !strings.Contains(currentMissing, required) {
			t.Fatalf("current target missing query lacks %q: %s", required, currentMissing)
		}
	}
	for _, query := range []string{
		compactDatabaseIntentSQL(completeDownloadIntentSubscriptionSQL),
		compactDatabaseIntentSQL(continueDownloadIntentSubscriptionSQL),
	} {
		for _, required := range []string{
			"current_download_intent_id=$3",
			"completed_at IS NULL",
			"internal_state IN ('acquired', 'downloading')",
			"current_download_intent_id=NULL",
			"config=config || $6::jsonb",
		} {
			if !strings.Contains(query, required) {
				t.Fatalf("parent acceptance 更新缺少约束 %q: %s", required, query)
			}
		}
	}
}

func TestAcceptDownloadIntentPersistenceBoundaries(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("99999999-8888-7777-6666-555555555555")
	var store *Store
	if _, _, err := store.AcceptDownloadIntent(
		t.Context(),
		"alice",
		intentID,
		databaseIntentV1Hash,
	); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("nil store acceptance error = %v, want ErrDownloadIntentPersistence", err)
	}
	if _, _, err := store.AcceptDownloadIntent(
		nil,
		"alice",
		intentID,
		databaseIntentV1Hash,
	); !errors.Is(err, ErrDownloadIntentPersistence) {
		t.Fatalf("nil context acceptance error = %v, want ErrDownloadIntentPersistence", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := store.AcceptDownloadIntent(
		canceled,
		"alice",
		intentID,
		databaseIntentV1Hash,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acceptance error = %v, want context.Canceled", err)
	}
}

func TestDownloadIntentAcceptanceWasAlreadyReleased(t *testing.T) {
	t.Parallel()

	intentID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	otherIntentID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	tests := []struct {
		name   string
		parent downloadIntentAcceptanceSubscription
		want   bool
	}{
		{name: "pending search without current", parent: downloadIntentAcceptanceSubscription{state: subscription.PendingSearch}, want: true},
		{name: "best movie active without current", parent: downloadIntentAcceptanceSubscription{state: subscription.Active}, want: true},
		{name: "active same current remains unsettled", parent: downloadIntentAcceptanceSubscription{state: subscription.Active, currentIntent: &intentID}},
		{name: "pending identification with different current", parent: downloadIntentAcceptanceSubscription{state: subscription.PendingIdentification, currentIntent: &otherIntentID}, want: true},
		{name: "completed without current", parent: downloadIntentAcceptanceSubscription{state: subscription.Completed, completed: true}, want: true},
		{name: "same current intent", parent: downloadIntentAcceptanceSubscription{state: subscription.PendingSearch, currentIntent: &intentID}},
		{name: "paused parent", parent: downloadIntentAcceptanceSubscription{state: subscription.Paused}},
		{name: "unsettled acquired parent", parent: downloadIntentAcceptanceSubscription{state: subscription.Acquired}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := downloadIntentAcceptanceWasAlreadyReleased(tt.parent, intentID); got != tt.want {
				t.Fatalf("downloadIntentAcceptanceWasAlreadyReleased() = %v, want %v", got, tt.want)
			}
		})
	}
}
