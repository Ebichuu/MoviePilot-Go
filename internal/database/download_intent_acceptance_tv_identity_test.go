package database

import (
	"encoding/json"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/subscription"
)

func TestTVQualityAcceptanceWriterMatchesFrozenV2IdentityGuards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                         string
		parentSource, parentID       string
		downloadSource, downloadID   string
		downloadType                 string
		legacy, full, ordinary, deny bool
	}{
		{name: "TMDB 改媒体拒绝本轮 writer", parentSource: "themoviedb", parentID: "43", downloadSource: "tmdb", downloadID: "42", deny: true},
		{name: "豆瓣改媒体拒绝本轮 writer", parentSource: "douban", parentID: "43", downloadSource: "douban", downloadID: "42", deny: true},
		{name: "同 provider 别名同 ID", parentSource: "themoviedb", parentID: "42", downloadSource: " TMDB ", downloadID: "42"},
		{name: "父订阅缺 ID 原版允许", parentSource: "tmdb", downloadSource: "tmdb", downloadID: "42"},
		{name: "本轮缺 ID 原版允许", parentSource: "tmdb", parentID: "43", downloadSource: "tmdb"},
		{name: "跨 provider 原版缺少同类 ID 允许", parentSource: "douban", parentID: "43", downloadSource: "tmdb", downloadID: "42"},
		{name: "未在原版比较的 provider 不新增过滤", parentSource: "bangumi", parentID: "43", downloadSource: "bangumi", downloadID: "42"},
		{name: "历史没有通知身份保留原版未知语义", parentSource: "tmdb", parentID: "43", legacy: true},
		{name: "captured movie 不能写 TV", parentSource: "tmdb", parentID: "42", downloadSource: "tmdb", downloadID: "42", downloadType: "电影", deny: true},
		{name: "整季拒绝 writer 不提升总质量", parentSource: "tmdb", parentID: "43", downloadSource: "tmdb", downloadID: "42", full: true, deny: true},
		{name: "普通拒绝 writer 仍按 lefts 完成", parentSource: "tmdb", parentID: "43", downloadSource: "tmdb", downloadID: "42", ordinary: true, deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			season := 1
			parent := downloadIntentAcceptanceSubscription{
				mediaType: "电视剧", mediaSource: tc.parentSource, mediaID: tc.parentID, season: &season,
				config: map[string]any{"best_version": !tc.ordinary, "best_version_full": tc.full, "total_episode": 2, "current_priority": 90, "note": []int{1}, "episode_priority": map[string]int{"1": 90}},
			}
			mediaType := tc.downloadType
			if mediaType == "" {
				mediaType = "电视剧"
			}
			facts := downloadIntentTVFacts{
				priority: r08MoviePriority(100), coverage: []subscription.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}, fullCoverage: true,
				mediaSource: tc.downloadSource, mediaID: tc.downloadID, mediaType: mediaType, mediaKnown: !tc.legacy,
			}
			if allowed := downloadIntentTVWriterAcceptsIdentity(parent, facts); allowed == tc.deny {
				t.Fatalf("writer allowed=%v want=%v", allowed, !tc.deny)
			}
			next, encoded, err := buildTVQualityAcceptancePatch(parent, nil, 0, facts)
			if err != nil {
				t.Fatal(err)
			}
			var patch map[string]any
			if err := json.Unmarshal(encoded, &patch); err != nil {
				t.Fatal(err)
			}
			if tc.deny {
				for _, field := range []string{"note", "episode_priority"} {
					if _, exists := patch[field]; exists {
						t.Fatalf("身份变化仍写入 %s: %s", field, encoded)
					}
				}
				if tc.full {
					if _, exists := patch["current_priority"]; exists {
						t.Fatalf("身份变化提升整季质量: %s", encoded)
					}
				}
				if !tc.ordinary && (next != subscription.Active || patch["lack_episode"] != float64(1)) {
					t.Fatalf("拒绝 writer 后进度未按现有事实: next=%s patch=%s", next, encoded)
				}
			} else if patch["episode_priority"] == nil || next != subscription.Completed {
				t.Fatalf("允许 writer 后未记录质量: next=%s patch=%s", next, encoded)
			}
			if tc.ordinary && next != subscription.Completed {
				t.Fatalf("普通模式不应因 writer 过滤中断 lefts 完成: %s", next)
			}
		})
	}
}

func TestTVQualityAcceptanceEmptyTargetKeepsLegacyOverallPriority(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		current *int
		want    subscription.State
	}{
		{"未知总体质量", nil, subscription.Active},
		{"已知零总体质量", r08MoviePriority(0), subscription.Active},
		{"总体质量90", r08MoviePriority(90), subscription.Active},
		{"总体质量100", r08MoviePriority(100), subscription.Completed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := map[string]any{"best_version": true, "total_episode": 2, "start_episode": 3}
			current := 0
			if tc.current != nil {
				current = *tc.current
				config["current_priority"] = current
			}
			next, encoded, err := buildTVQualityAcceptancePatch(
				downloadIntentAcceptanceSubscription{mediaType: "电视剧", config: config}, nil, 0,
				downloadIntentTVFacts{priority: r08MoviePriority(100), coverage: []subscription.Episode{}},
			)
			if err != nil || next != tc.want {
				t.Fatalf("空目标完成未沿用原版总体质量: next=%s want=%s patch=%s err=%v", next, tc.want, encoded, err)
			}
			var patch map[string]any
			if err := json.Unmarshal(encoded, &patch); err != nil {
				t.Fatal(err)
			}
			if patch["current_priority"] != float64(current) || patch["lack_episode"] != float64(0) {
				t.Fatalf("空目标总体质量或缺集被改写: %s", encoded)
			}
		})
	}
}

func TestTVQualityAcceptanceWithoutTargetIntersectionDoesNotWriteDownloadFacts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		coverage []subscription.Episode
	}{
		{"已知空覆盖", []subscription.Episode{}},
		{"实际覆盖在当前目标之外", []subscription.Episode{{Season: 1, Episode: 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent := downloadIntentAcceptanceSubscription{mediaType: "电视剧", config: map[string]any{
				"best_version": true, "total_episode": 2, "current_priority": 90,
				"note": []any{"junk", 1}, "episode_priority": map[string]any{"foo": 90, "1": 90},
			}}
			next, encoded, err := buildTVQualityAcceptancePatch(parent, nil, 0, downloadIntentTVFacts{priority: r08MoviePriority(100), coverage: tc.coverage})
			if err != nil || next != subscription.Active {
				t.Fatalf("无交集仍应按已有事实结算: next=%s patch=%s err=%v", next, encoded, err)
			}
			var patch map[string]any
			if err := json.Unmarshal(encoded, &patch); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"note", "episode_priority"} {
				if _, exists := patch[field]; exists {
					t.Fatalf("无交集覆盖不应重写 %s: %s", field, encoded)
				}
			}
			if patch["current_priority"] != float64(0) || patch["lack_episode"] != float64(1) {
				t.Fatalf("无交集时未独立刷新原版进度: %s", encoded)
			}
		})
	}
}
