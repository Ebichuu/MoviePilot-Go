package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// 本文件覆盖插件动作依赖的三个手工入队入口：未启用、nil 上下文、已取消上下文
// 都要在入队前就被挡住——否则任务会插进队列后才在 worker 里失败。

func TestPluginEnqueueGuards(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name        string
		manager     *Manager
		ctx         context.Context
		unavailable error
		execution   error
		call        func(*Manager, context.Context) error
	}{
		{
			name:        "签到",
			manager:     &Manager{},
			ctx:         context.Background(),
			unavailable: ErrSiteSigninSweepUnavailable,
			execution:   ErrSiteSigninSweepExecution,
			call:        func(m *Manager, ctx context.Context) error { return m.EnqueueSiteSigninSweep(ctx) },
		},
		{
			name:        "备份",
			manager:     &Manager{},
			ctx:         context.Background(),
			unavailable: ErrBackupSweepUnavailable,
			execution:   ErrBackupSweepExecution,
			call:        func(m *Manager, ctx context.Context) error { return m.EnqueueBackupSweep(ctx, " ", 3) },
		},
		{
			name:        "订阅精准搜索",
			manager:     &Manager{},
			ctx:         context.Background(),
			unavailable: ErrSubscriptionSearchUnavailable,
			execution:   ErrSubscriptionSearchExecution,
			call:        func(m *Manager, ctx context.Context) error { return m.EnqueueSubscriptionSearchSweep(ctx) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(test.manager, nil); !errors.Is(err, test.execution) {
				t.Fatalf("nil 上下文 error = %v", err)
			}
			if err := test.call(test.manager, canceled); !errors.Is(err, context.Canceled) {
				t.Fatalf("已取消上下文 error = %v", err)
			}
			if err := test.call(test.manager, test.ctx); !errors.Is(err, test.unavailable) {
				t.Fatalf("未启用/未装配 error = %v", err)
			}
			// 启用标记为真但没有 River 客户端时仍然拒绝，避免插入无 worker 的任务。
			enabled := &Manager{siteSigninSweepEnabled: true, backupSweepEnabled: true, subscriptionSearchEnabled: true}
			if err := test.call(enabled, test.ctx); !errors.Is(err, test.unavailable) {
				t.Fatalf("无客户端 error = %v", err)
			}
		})
	}
}

func TestBackupSweepArgsCarryKeep(t *testing.T) {
	encoded, err := json.Marshal(BackupSweepArgs{Dir: "/backup", Keep: 3})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded BackupSweepArgs
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if decoded.Dir != "/backup" || decoded.Keep != 3 {
		t.Fatalf("args = %#v", decoded)
	}
	// 空值不落 JSON：老队列里的历史任务没有这两个字段，反序列化必须照常工作。
	empty, err := json.Marshal(BackupSweepArgs{})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if string(empty) != "{}" {
		t.Fatalf("空参数 JSON = %s", empty)
	}
}
