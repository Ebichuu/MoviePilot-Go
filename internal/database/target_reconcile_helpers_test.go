package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func targetReconcileGuardStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

// 本文件覆盖订阅目标核对与媒体索引匹配的纯函数分支及闸门：
// 坐标规范化边界、目标快照一致性、nil 事务、nil 上下文与连接不可用。

func reconcileTargetInput(t *testing.T) SubscriptionTargetReconcile {
	t.Helper()
	snapshot := subscription.TVTargetSnapshot{Season: 1, StartEpisode: 1, TotalEpisode: 12}
	resolution, err := subscription.DescribeTVTarget(snapshot)
	if err != nil {
		t.Fatalf("描述目标快照: %v", err)
	}
	return SubscriptionTargetReconcile{
		Username: "alice", SubscriptionID: 1, ExpectedUpdatedAt: time.Now().UTC(), Target: resolution,
	}
}

func TestNormalizeSubscriptionTargetCoordinatesBranches(t *testing.T) {
	if _, _, err := normalizeSubscriptionTargetCoordinates(make([]subscription.Episode, maxSubscriptionEpisodes+1)); !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("坐标数量超限 error = %v", err)
	}

	invalid := []struct {
		name  string
		items []subscription.Episode
	}{
		{name: "季号为负", items: []subscription.Episode{{Season: -1, Episode: 1}}},
		{name: "集号为零", items: []subscription.Episode{{Season: 1, Episode: 0}}},
		{name: "季号超限", items: []subscription.Episode{{Season: maxSubscriptionEpisodeSeason + 1, Episode: 1}}},
		{name: "集号超限", items: []subscription.Episode{{Season: 1, Episode: maxSubscriptionEpisodeNumber + 1}}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := normalizeSubscriptionTargetCoordinates(tc.items); !errors.Is(err, ErrInvalidSubscriptionTarget) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	seasons, episodes, err := normalizeSubscriptionTargetCoordinates([]subscription.Episode{
		{Season: 2, Episode: 1}, {Season: 1, Episode: 3}, {Season: 1, Episode: 2}, {Season: 1, Episode: 2},
	})
	if err != nil {
		t.Fatalf("规范化坐标: %v", err)
	}
	wantSeasons := []int32{1, 1, 2}
	wantEpisodes := []int32{2, 3, 1}
	if len(seasons) != len(wantSeasons) || len(episodes) != len(wantEpisodes) {
		t.Fatalf("坐标长度 = %d/%d", len(seasons), len(episodes))
	}
	for index := range wantSeasons {
		if seasons[index] != wantSeasons[index] || episodes[index] != wantEpisodes[index] {
			t.Fatalf("坐标 = %v/%v", seasons, episodes)
		}
	}
}

func TestBuildReconcileSubscriptionTargetMismatch(t *testing.T) {
	input := reconcileTargetInput(t)
	input.Target.Knowledge = subscription.TVTargetKnown
	input.Target.Snapshot.TotalEpisode = 0
	if _, _, err := buildReconcileSubscriptionTarget(input); !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("知识不一致 error = %v", err)
	}

	input = reconcileTargetInput(t)
	input.Target.TargetEpisodes = append(input.Target.TargetEpisodes, subscription.Episode{Season: 9, Episode: 9})
	if _, _, err := buildReconcileSubscriptionTarget(input); !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("目标集不一致 error = %v", err)
	}

	if _, _, err := buildReconcileSubscriptionTarget(reconcileTargetInput(t)); err != nil {
		t.Fatalf("合法输入 error = %v", err)
	}
}

func TestReconcileSubscriptionTargetGuards(t *testing.T) {
	input := reconcileTargetInput(t)
	store := targetReconcileGuardStore(t)

	if _, err := store.ReconcileSubscriptionTarget(nil, input); !errors.Is(err, ErrSubscriptionTargetPersistence) {
		t.Fatalf("nil 上下文 error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReconcileSubscriptionTarget(canceled, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消上下文 error = %v", err)
	}

	if _, err := store.ReconcileSubscriptionTarget(context.Background(), input); err == nil {
		t.Fatal("连接不可用时应返回错误")
	}
}

func TestVerifySubscriptionTargetVersionTxGuards(t *testing.T) {
	ctx := context.Background()
	store := &Store{}
	now := time.Now().UTC()

	if err := store.VerifySubscriptionTargetVersionTx(ctx, nil, "alice", 1, now); !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("nil 事务 error = %v", err)
	}
	if err := store.VerifySubscriptionTargetVersionTx(ctx, nil, "  ", 1, now); !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("空所有者 error = %v", err)
	}
	if err := store.VerifySubscriptionTargetVersionTx(ctx, nil, "alice", 0, now); !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("订阅 ID 为零 error = %v", err)
	}
	if err := store.VerifySubscriptionTargetVersionTx(ctx, nil, "alice", 1, time.Time{}); !errors.Is(err, ErrInvalidSubscriptionTarget) {
		t.Fatalf("零时间戳 error = %v", err)
	}
}

func TestListSubscriptionEpisodesForTargetHelperGuards(t *testing.T) {
	ctx := context.Background()
	store := targetReconcileGuardStore(t)
	now := time.Now().UTC()
	episodes := []subscription.Episode{{Season: 1, Episode: 1}}
	states := []subscription.SubscriptionEpisodeState{subscription.SubscriptionEpisodeMissing}

	invalid := []struct {
		name      string
		username  string
		id        int64
		updatedAt time.Time
		targets   []subscription.Episode
		states    []subscription.SubscriptionEpisodeState
		limit     int
	}{
		{name: "空所有者", id: 1, updatedAt: now, targets: episodes, states: states, limit: 10},
		{name: "订阅 ID 为零", username: "alice", updatedAt: now, targets: episodes, states: states, limit: 10},
		{name: "零时间戳", username: "alice", id: 1, targets: episodes, states: states, limit: 10},
		{name: "无状态", username: "alice", id: 1, updatedAt: now, targets: episodes, limit: 10},
		{name: "页大小为零", username: "alice", id: 1, updatedAt: now, targets: episodes, states: states},
		{name: "坐标非法", username: "alice", id: 1, updatedAt: now,
			targets: []subscription.Episode{{Season: -1, Episode: 1}}, states: states, limit: 10},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ListSubscriptionEpisodesForTarget(ctx, tc.username, tc.id, tc.updatedAt, tc.targets, tc.states, tc.limit); !errors.Is(err, ErrInvalidSubscriptionTarget) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("非法状态", func(t *testing.T) {
		if _, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 1, now, episodes,
			[]subscription.SubscriptionEpisodeState{"bogus"}, 10); !errors.Is(err, ErrInvalidSubscriptionTarget) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("nil 上下文", func(t *testing.T) {
		if _, err := store.ListSubscriptionEpisodesForTarget(nil, "alice", 1, now, episodes, states, 10); !errors.Is(err, ErrSubscriptionTargetPersistence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("连接不可用", func(t *testing.T) {
		if _, err := store.ListSubscriptionEpisodesForTarget(ctx, "alice", 1, now, episodes, states, 10); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})
}

func TestMatchMediaIndexFilesBranches(t *testing.T) {
	stored := []mediaIndexOrganizationFile{
		{Ordinal: 0, TargetPath: "/tv/a.mkv", ExpectedSize: 10, ExpectedSHA256: "sha-a"},
		{Ordinal: 1, TargetPath: "/tv/b.mkv", ExpectedSize: 20, ExpectedSHA256: "sha-b"},
	}
	if err := matchMediaIndexFiles(stored, nil); !errors.Is(err, ErrOrganizationStateConflict) {
		t.Fatalf("空计划 error = %v", err)
	}

	plan := make([]mediaindex.TargetFile, 0, len(stored))
	for _, file := range stored {
		plan = append(plan, mediaindex.TargetFile{
			LocalPath: file.TargetPath, ExpectedSize: file.ExpectedSize, ExpectedSHA256: file.ExpectedSHA256,
		})
	}
	if err := matchMediaIndexFiles(stored, plan); err != nil {
		t.Fatalf("匹配 error = %v", err)
	}
	if err := matchMediaIndexFiles(stored, append(plan, mediaindex.TargetFile{LocalPath: "/tv/other.mkv"})); !errors.Is(err, ErrOrganizationStateConflict) {
		t.Fatalf("计划多于已存 error = %v", err)
	} else if err := matchMediaIndexFiles(stored, plan[:1]); err != nil {
		t.Fatalf("部分计划 error = %v", err)
	}

	duplicated := []mediaIndexOrganizationFile{stored[0], stored[0]}
	if err := matchMediaIndexFiles(duplicated, plan[:1]); !errors.Is(err, ErrOrganizationStateConflict) {
		t.Fatalf("重复匹配 error = %v", err)
	}
}

func TestCanonicalMediaIndexRemotePathBranches(t *testing.T) {
	windows := canonicalMediaIndexRemotePath(` /tv//show\\season//x.mkv `, "windows", true)
	if windows != "windows\x00"+`\tv\show\season\x.mkv` {
		t.Fatalf("windows 归一 = %q", windows)
	}
	unix := canonicalMediaIndexRemotePath(`\tv\\show/season//x.mkv`, "unix", true)
	if unix != "unix\x00/tv/show/season/x.mkv" {
		t.Fatalf("unix 归一 = %q", unix)
	}
	if got := canonicalMediaIndexRemotePath("/", "unix", false); got != "unix\x00/" {
		t.Fatalf("根路径归一 = %q", got)
	}
	if got := canonicalMediaIndexRemotePath("/TV/Show.MKV", "unix", false); got != "unix\x00/tv/show.mkv" {
		t.Fatalf("大小写归一 = %q", got)
	}
}
