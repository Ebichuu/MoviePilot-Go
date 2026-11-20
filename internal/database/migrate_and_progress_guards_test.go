package database

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖进度补丁与迁移入口的失败分支，以及候选排序输入的读取闸门。

func TestBuildDownloadIntentAcceptanceProgressPatchInvalidEpisode(t *testing.T) {
	// 电视剧：消费集数必须落在合法区间内。
	if _, err := buildDownloadIntentAcceptanceProgressPatch(
		"tv",
		map[string]any{},
		[]subscription.Episode{{Season: 1, Episode: 0}},
		2,
	); err == nil {
		t.Fatal("非法消费集数应报错")
	}

	patch, err := buildDownloadIntentAcceptanceProgressPatch(
		"tv",
		map[string]any{"note": []any{1}},
		[]subscription.Episode{{Season: 1, Episode: 3}},
		2,
	)
	if err != nil {
		t.Fatalf("合法输入 error = %v", err)
	}
	if len(patch) == 0 {
		t.Fatal("补丁不应为空")
	}

	// 电影：忽略消费集数，缺集数归零。
	moviePatch, err := buildDownloadIntentAcceptanceProgressPatch("movie", map[string]any{}, nil, 5)
	if err != nil {
		t.Fatalf("电影补丁 error = %v", err)
	}
	if string(moviePatch) != `{"lack_episode":0,"note":[1]}` {
		t.Fatalf("电影补丁 = %s", moviePatch)
	}
}

func TestLoadV2CandidateSortInputsRejectsUnreadyStore(t *testing.T) {
	// GetSetting 内部直接解引用连接池，未就绪 store 会 panic，因此只用
	// 「连接不可用」的 store 覆盖错误返回分支。
	if _, _, _, err := settingsGuardStore(t).LoadV2CandidateSortInputs(context.Background()); err == nil {
		t.Fatal("连接不可用应报错")
	}
}

func TestNormalizeOrganizationPlanRejectsNulPath(t *testing.T) {
	if _, _, _, _, _, err := normalizeOrganizationPlan(
		"alice",
		uuid.New(),
		"C:\\media\\\x00bad",
		organizer.Plan{},
	); err == nil {
		t.Fatal("路径含 NUL 应报错")
	}
}

func TestMigrateReportsConnectionFailures(t *testing.T) {
	if err := Migrate(context.Background(), config.Config{DatabaseURL: "definitely-not-a-dsn"}); err == nil {
		t.Fatal("非法 DSN 应报错")
	}
	unreachable := "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	if err := Migrate(context.Background(), config.Config{DatabaseURL: unreachable}); err == nil {
		t.Fatal("不可达数据库应报错")
	}
}
