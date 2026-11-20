package database

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/organization"
)

// 本文件覆盖组织成功快照的规范化与读取闸门：
// 非法所有者/快照数量/版本、legacy 空文件、nil 事务、未就绪 store、
// 连接不可用，以及快照与下载意图成功输入的一致性比对。

func organizationSnapshotFixture(t *testing.T) organization.SuccessSnapshot {
	t.Helper()
	media := organization.MediaContext{Source: "themoviedb", MediaID: "7", TitleYear: "剧 (2026)", MediaType: "电视剧"}
	key, err := organization.BuildGroupKey(media, 1)
	if err != nil {
		t.Fatalf("构建分组键: %v", err)
	}
	return organization.SuccessSnapshot{
		BatchID:  uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		IntentID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		GroupKey: key, Version: organization.CurrentSuccessVersion, Username: "alice",
		NeedNotify: true, Media: media,
		Files: []organization.FileFact{{
			Ordinal: 0, Role: "media", SizeBytes: 1024, Season: 1, Episodes: []int{1},
		}},
	}
}

func TestListOrganizationSuccessSnapshotsGuards(t *testing.T) {
	ctx := context.Background()
	batchID := uuid.New()

	t.Run("非法参数", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			username string
			batchID  uuid.UUID
		}{
			{name: "空所有者", batchID: batchID},
			{name: "nil 批次", username: "alice"},
		} {
			if _, err := (&Store{}).ListOrganizationSuccessSnapshots(ctx, tc.username, tc.batchID); !errors.Is(err, ErrInvalidOrganizationSuccessSnapshot) {
				t.Fatalf("%s error = %v", tc.name, err)
			}
		}
	})

	t.Run("未就绪与 nil 上下文", func(t *testing.T) {
		if _, err := (&Store{}).ListOrganizationSuccessSnapshots(ctx, "alice", batchID); !errors.Is(err, ErrOrganizationSuccessSnapshotPersistence) {
			t.Fatalf("未就绪 error = %v", err)
		}
		pool, err := pgxpool.New(ctx, "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
		if err != nil {
			t.Fatalf("创建不可用连接池: %v", err)
		}
		defer pool.Close()
		store := &Store{db: &DB{Pool: pool}}
		if _, err := store.ListOrganizationSuccessSnapshots(nil, "alice", batchID); !errors.Is(err, ErrOrganizationSuccessSnapshotPersistence) {
			t.Fatalf("nil 上下文 error = %v", err)
		}
		if _, err := store.ListOrganizationSuccessSnapshots(ctx, "alice", batchID); err == nil {
			t.Fatal("连接不可用时应返回错误")
		}
	})
}

func TestCreateOrganizationSuccessSnapshotsTxRequiresTransaction(t *testing.T) {
	snapshot := organizationSnapshotFixture(t)
	store := &Store{}
	if _, err := store.CreateOrganizationSuccessSnapshotsTx(context.Background(), nil, "alice", []organization.SuccessSnapshot{snapshot}); !errors.Is(err, ErrOrganizationSuccessSnapshotPersistence) {
		t.Fatalf("nil 事务 error = %v", err)
	}
	if _, err := store.CreateOrganizationSuccessSnapshotsTx(context.Background(), nil, "", []organization.SuccessSnapshot{snapshot}); !errors.Is(err, ErrInvalidOrganizationSuccessSnapshot) {
		t.Fatalf("空所有者 error = %v", err)
	}
}

func TestNormalizeOrganizationSuccessSnapshotsGuards(t *testing.T) {
	snapshot := organizationSnapshotFixture(t)
	zeroFilesLegacy := snapshot.Clone()
	zeroFilesLegacy.Legacy = true
	zeroFilesLegacy.Version = 0
	zeroFilesLegacy.Files = nil
	wrongUsername := snapshot.Clone()
	wrongUsername.Username = "bob"

	cases := []struct {
		name      string
		username  string
		snapshots []organization.SuccessSnapshot
	}{
		{name: "空所有者", snapshots: []organization.SuccessSnapshot{snapshot}},
		{name: "无快照", username: "alice"},
		{name: "快照数量超限", username: "alice", snapshots: make([]organization.SuccessSnapshot, 10_001)},
		{name: "所有者不一致", username: "alice", snapshots: []organization.SuccessSnapshot{wrongUsername}},
		{name: "legacy 空文件", username: "alice", snapshots: []organization.SuccessSnapshot{zeroFilesLegacy}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := normalizeOrganizationSuccessSnapshots(tc.username, tc.snapshots); !errors.Is(err, ErrInvalidOrganizationSuccessSnapshot) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	username, normalized, encoded, err := normalizeOrganizationSuccessSnapshots(" alice ", []organization.SuccessSnapshot{snapshot})
	if err != nil {
		t.Fatalf("规范化合法快照: %v", err)
	}
	if username != "alice" || len(normalized) != 1 || len(encoded) != 1 {
		t.Fatalf("规范化结果 = %q / %d / %d", username, len(normalized), len(encoded))
	}
}

func TestOrganizationSuccessSnapshotsMatchInput(t *testing.T) {
	snapshot := organizationSnapshotFixture(t)
	input := organization.SuccessInput{
		IntentID: snapshot.IntentID, Version: snapshot.Version, Legacy: snapshot.Legacy,
		Username: snapshot.Username, Manual: snapshot.Manual, Background: snapshot.Background,
		Media: snapshot.Media, ResourceTerm: snapshot.Files[0].ResourceTerm,
	}
	if !organizationSuccessSnapshotsMatchInput([]organization.SuccessSnapshot{snapshot}, input) {
		t.Fatal("一致输入应返回 true")
	}

	mismatched := []struct {
		name   string
		mutate func(*organization.SuccessInput)
	}{
		{name: "版本不一致", mutate: func(in *organization.SuccessInput) { in.Version++ }},
		{name: "媒体不一致", mutate: func(in *organization.SuccessInput) { in.Media.MediaID = "8" }},
		{name: "资源词不一致", mutate: func(in *organization.SuccessInput) { in.ResourceTerm = "不同资源" }},
	}
	for _, tc := range mismatched {
		t.Run(tc.name, func(t *testing.T) {
			clone := input
			tc.mutate(&clone)
			if organizationSuccessSnapshotsMatchInput([]organization.SuccessSnapshot{snapshot}, clone) {
				t.Fatal("不一致输入应返回 false")
			}
		})
	}

	legacySnapshot := snapshot.Clone()
	legacySnapshot.Legacy = true
	legacyInput := input
	legacyInput.Legacy = true
	legacyInput.ResourceTerm = "legacy 不比对资源词"
	if !organizationSuccessSnapshotsMatchInput([]organization.SuccessSnapshot{legacySnapshot}, legacyInput) {
		t.Fatal("legacy 快照跳过资源词比对时应返回 true")
	}
}
