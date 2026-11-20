package database

import (
	"errors"
	"testing"
)

// TestResolveSubscriptionPublicProgress 覆盖公开进度解析：总集数上调同步增加缺失集数、
// 下调不动缺失集数、手动标记透传，以及非法配置与非法总集数。
func TestResolveSubscriptionPublicProgress(t *testing.T) {
	cases := []struct {
		name       string
		config     map[string]any
		total      int
		wantManual bool
		wantLack   int
	}{
		{
			name:   "总集数下调不改缺失集数",
			config: map[string]any{"total_episode": 12, "lack_episode": 12},
			total:  6, wantManual: true, wantLack: 12,
		},
		{
			name:   "总集数上调同步增加缺失集数",
			config: map[string]any{"total_episode": 12, "lack_episode": 4},
			total:  16, wantManual: true, wantLack: 8,
		},
		{
			name:   "总集数不变不标记手动",
			config: map[string]any{"total_episode": 12, "lack_episode": 2},
			total:  12, wantManual: false, wantLack: 2,
		},
		{
			name:   "JSON 浮点数形态的总集数",
			config: map[string]any{"total_episode": float64(8), "lack_episode": float64(3)},
			total:  10, wantManual: true, wantLack: 5,
		},
		{
			name:   "已手动标记保持为真",
			config: map[string]any{"total_episode": 0, "lack_episode": 0, "manual_total_episode": true},
			total:  0, wantManual: true, wantLack: 0,
		},
		{
			name:   "空配置",
			config: nil,
			total:  0, wantManual: false, wantLack: 0,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			progress, err := ResolveSubscriptionPublicProgress(
				Subscription{Config: testCase.config}, testCase.total,
			)
			if err != nil {
				t.Fatalf("ResolveSubscriptionPublicProgress() error = %v", err)
			}
			if progress.ManualTotalEpisode != testCase.wantManual || progress.LackEpisode != testCase.wantLack {
				t.Fatalf("progress = %#v, want manual=%v lack=%d",
					progress, testCase.wantManual, testCase.wantLack)
			}
		})
	}

	if _, err := ResolveSubscriptionPublicProgress(
		Subscription{Config: map[string]any{"manual_total_episode": "yes"}}, 5,
	); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
		t.Fatalf("非法手动标记 error = %v, want ErrInvalidSubscriptionPublicMutation", err)
	}
	if _, err := ResolveSubscriptionPublicProgress(Subscription{}, -1); !errors.Is(err, ErrInvalidSubscriptionPublicMutation) {
		t.Fatalf("负总集数 error = %v, want ErrInvalidSubscriptionPublicMutation", err)
	}
}
