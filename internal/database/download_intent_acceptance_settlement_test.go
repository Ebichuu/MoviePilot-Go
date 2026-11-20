package database

import (
	"context"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖意图确认结算阶段里电视剧目标解析的失败透传：
// 恢复目标分支、完整覆盖分支与进度计算分支都在缺少合法目标配置时报错。

func TestBuildDownloadIntentAcceptanceSettlementRejectsInvalidTVTarget(t *testing.T) {
	ctx := context.Background()
	invalidConfig := downloadIntentAcceptanceSubscription{
		mediaType: "tv",
		config:    map[string]any{"start_episode": "abc"},
	}
	recovered := &subscription.TVRecoveredTarget{
		TotalEpisode:     2,
		MissingEpisodes:  []subscription.Episode{{Season: 1, Episode: 2}},
		ExpectedCoverage: []subscription.Episode{{Season: 1, Episode: 1}},
	}

	if _, _, err := buildDownloadIntentAcceptanceSettlement(ctx, nil, 1, invalidConfig, nil, nil,
		downloadIntentTVFacts{recovered: recovered}); err == nil {
		t.Fatal("恢复目标分支应透传目标解析错误")
	}

	if _, _, err := buildDownloadIntentAcceptanceSettlement(ctx, nil, 1, invalidConfig, nil, nil,
		downloadIntentTVFacts{coverage: []subscription.Episode{}, fullCoverage: true}); err == nil {
		t.Fatal("完整覆盖分支应透传目标解析错误")
	}

	if _, _, err := buildDownloadIntentAcceptanceSettlement(ctx, nil, 1, invalidConfig, nil, nil,
		downloadIntentTVFacts{coverage: []subscription.Episode{{Season: 1, Episode: 1}}}); err == nil {
		t.Fatal("进度计算分支应透传目标解析错误")
	}
}

func TestBuildDownloadIntentAcceptanceSettlementRejectsInvalidRecoveredTarget(t *testing.T) {
	// 目标快照没有总集数时进入恢复目标归一化，非法恢复目标被拒绝。
	parent := downloadIntentAcceptanceSubscription{mediaType: "tv"}
	if _, _, err := buildDownloadIntentAcceptanceSettlement(context.Background(), nil, 1, parent, nil, nil,
		downloadIntentTVFacts{recovered: &subscription.TVRecoveredTarget{}}); err == nil {
		t.Fatal("非法恢复目标应报错")
	}
}

func TestDecodeDownloadIntentTVRecoveredTargetRejectsInvalidValues(t *testing.T) {
	if _, err := decodeDownloadIntentTVRecoveredTarget([]byte("{")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := decodeDownloadIntentTVRecoveredTarget([]byte("null")); err == nil {
		t.Fatal("null 目标应报错")
	}
	if _, err := decodeDownloadIntentTVRecoveredTarget([]byte(`{"total_episode":0}`)); err == nil {
		t.Fatal("非法恢复目标应报错")
	}
}
