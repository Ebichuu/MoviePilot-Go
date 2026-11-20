package database

import (
	"errors"
	"testing"
)

// TestSubscriptionTVTargetSnapshotRejectsInvalidConfig 覆盖电视剧目标快照
// 组件解析时对非整数集数、非法布尔标记与非法当前优先级的拒绝。
func TestSubscriptionTVTargetSnapshotRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
	}{
		{name: "起始集非整数", config: map[string]any{"start_episode": "abc"}},
		{name: "总集数非整数", config: map[string]any{"total_episode": []any{1}}},
		{name: "最优版本标记非法", config: map[string]any{"best_version": map[string]any{}}},
		{name: "最优版本完整标记非法", config: map[string]any{"best_version_full": "maybe"}},
		{name: "当前优先级非法", config: map[string]any{"current_priority": "abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := subscriptionTVTargetSnapshot(Subscription{Config: tc.config}); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	season := 3
	snapshot, err := subscriptionTVTargetSnapshot(Subscription{Season: &season})
	if err != nil {
		t.Fatalf("空配置 error = %v", err)
	}
	if snapshot.Season != season {
		t.Fatalf("季 = %d", snapshot.Season)
	}

	prioritized, err := subscriptionTVTargetSnapshot(Subscription{Config: map[string]any{
		"current_priority": 2,
		"note":             []any{1, 2},
	}})
	if err != nil {
		t.Fatalf("带优先级配置 error = %v", err)
	}
	if prioritized.CurrentPriority == nil || *prioritized.CurrentPriority != 2 {
		t.Fatalf("当前优先级 = %v", prioritized.CurrentPriority)
	}
	if len(prioritized.Note) == 0 {
		t.Fatal("note 应被投影到快照")
	}
}
