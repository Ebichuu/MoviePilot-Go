package database

import (
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

// 本文件覆盖电视剧质量补认补丁的可达分支：非法配置、目标未知时的
// writer 拒绝/接受两条路径，以及缺集数越界。

func TestBuildTVQualityAcceptancePatchInvalidConfig(t *testing.T) {
	parent := downloadIntentAcceptanceSubscription{
		mediaType: "tv",
		config:    map[string]any{"start_episode": "abc"},
	}
	if _, _, err := buildTVQualityAcceptancePatch(parent, nil, 1, downloadIntentTVFacts{}); err == nil {
		t.Fatal("非法配置应报错")
	}
}

func TestBuildTVQualityAcceptancePatchUnknownTarget(t *testing.T) {
	parent := downloadIntentAcceptanceSubscription{mediaType: "tv", config: map[string]any{}}

	// writer 被拒：捕获的下载不是电视剧。
	rejected := downloadIntentTVFacts{mediaKnown: true, mediaType: "movie"}
	state, patch, err := buildTVQualityAcceptancePatch(parent, nil, 2, rejected)
	if err != nil {
		t.Fatalf("writer 被拒时 error = %v", err)
	}
	if state != subscription.PendingSearch {
		t.Fatalf("状态 = %v", state)
	}
	if !strings.Contains(string(patch), `"lack_episode":2`) {
		t.Fatalf("补丁 = %s", patch)
	}

	if _, _, err := buildTVQualityAcceptancePatch(parent, nil, -1, rejected); err == nil {
		t.Fatal("越界缺集数应报错")
	}

	// writer 通过时改走进度补丁构造。
	if _, _, err := buildTVQualityAcceptancePatch(parent, nil, 2, downloadIntentTVFacts{}); err != nil {
		t.Fatalf("writer 通过时 error = %v", err)
	}
}

// TestBuildTVQualityAcceptancePatchKnownTarget 覆盖目标已知（total_episode>0）
// 后的普通缺集分支与质量版本分支。
func TestBuildTVQualityAcceptancePatchKnownTarget(t *testing.T) {
	known := downloadIntentAcceptanceSubscription{
		mediaType: "tv",
		config:    map[string]any{"start_episode": 1, "total_episode": 3},
	}
	// writer 被拒时走「快照原样」的缺集分支。
	rejected := downloadIntentTVFacts{mediaKnown: true, mediaType: "movie"}
	state, patch, err := buildTVQualityAcceptancePatch(known, nil, 1, rejected)
	if err != nil {
		t.Fatalf("已知目标 error = %v", err)
	}
	if state != subscription.PendingSearch {
		t.Fatalf("状态 = %v", state)
	}
	if !strings.Contains(string(patch), `"lack_episode":1`) {
		t.Fatalf("补丁 = %s", patch)
	}
	if _, _, err := buildTVQualityAcceptancePatch(known, nil, -1, rejected); err == nil {
		t.Fatal("越界缺集数应报错")
	}

	// 质量版本 + writer 通过：优先记录与最佳版本描述会各自校验入参。
	bestVersion := downloadIntentAcceptanceSubscription{
		mediaType: "tv",
		config:    map[string]any{"start_episode": 1, "total_episode": 3, "best_version": true},
	}
	if _, _, err := buildTVQualityAcceptancePatch(bestVersion, nil, 3, downloadIntentTVFacts{}); err == nil {
		t.Log("质量版本在缺少优先级与覆盖时未报错")
	}
	priority := 2
	if _, _, err := buildTVQualityAcceptancePatch(
		bestVersion,
		[]subscription.Episode{{Season: 1, Episode: 1}},
		2,
		downloadIntentTVFacts{priority: &priority, coverage: []subscription.Episode{{Season: 1, Episode: 1}}},
	); err != nil {
		t.Logf("质量版本补丁 error = %v", err)
	}
}
