package pluginengine

import (
	"context"
	"testing"
)

// 本文件覆盖配置变更回调：插件配置不是「存起来好看」的，它要能驱动运行时
// （例如按 interval_hours 重建周期任务）。因此回调必须：写成功才触发、
// 拿到的是副本、卸载回到默认值也要触发。

type pluginConfigEvent struct {
	id     string
	config Config
}

func newListenerRegistry(t *testing.T) *Registry {
	t.Helper()
	registry := NewRegistry(NewMemoryStore())
	if err := registry.RegisterAll(BuiltinDefinitions(Services{})); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := registry.Install(context.Background(), PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	return registry
}

func TestConfigListenerNotifiedOnUpdate(t *testing.T) {
	ctx := context.Background()
	registry := newListenerRegistry(t)
	var events []pluginConfigEvent
	registry.SetConfigListener(func(_ context.Context, id string, config Config) {
		events = append(events, pluginConfigEvent{id: id, config: config})
	})

	if err := registry.UpdateConfig(ctx, PluginSiteSignIn, map[string]any{"interval_hours": 6}); err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 1 || events[0].id != PluginSiteSignIn {
		t.Fatalf("回调 = %#v", events)
	}
	if events[0].config["interval_hours"] != int64(6) {
		t.Fatalf("回调配置 = %#v", events[0].config)
	}

	// 回调拿到的是副本：实现方改它不会污染注册表。
	events[0].config["interval_hours"] = int64(99)
	plugin, err := registry.Get(PluginSiteSignIn)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if plugin.Config["interval_hours"] != int64(6) {
		t.Fatalf("注册表被回调改动: %#v", plugin.Config)
	}
}

func TestConfigListenerSilentOnFailure(t *testing.T) {
	ctx := context.Background()
	registry := newListenerRegistry(t)
	calls := 0
	registry.SetConfigListener(func(context.Context, string, Config) { calls++ })

	// 未知键 → 校验失败，配置没变，就不该通知。
	if err := registry.UpdateConfig(ctx, PluginSiteSignIn, map[string]any{"nope": 1}); err == nil {
		t.Fatalf("非法配置应当失败")
	}
	// 未安装的插件 → 直接失败。
	if err := registry.UpdateConfig(ctx, PluginCloudBackup, map[string]any{"keep": 3}); err == nil {
		t.Fatalf("未安装插件应当失败")
	}
	if calls != 0 {
		t.Fatalf("失败时不应回调，实际 %d 次", calls)
	}

	// 取消回调后不再通知。
	registry.SetConfigListener(nil)
	if err := registry.UpdateConfig(ctx, PluginSiteSignIn, map[string]any{"interval_hours": 8}); err != nil {
		t.Fatalf("error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("取消回调后仍被调用 %d 次", calls)
	}
}

func TestConfigListenerOnUninstall(t *testing.T) {
	ctx := context.Background()
	registry := newListenerRegistry(t)
	if err := registry.UpdateConfig(ctx, PluginSiteSignIn, map[string]any{"interval_hours": 6}); err != nil {
		t.Fatalf("error = %v", err)
	}
	var events []pluginConfigEvent
	registry.SetConfigListener(func(_ context.Context, id string, config Config) {
		events = append(events, pluginConfigEvent{id: id, config: config})
	})

	if err := registry.Uninstall(ctx, PluginSiteSignIn); err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("卸载应当回调一次: %#v", events)
	}
	if events[0].config["interval_hours"] != int64(24) {
		t.Fatalf("卸载后配置应回默认值: %#v", events[0].config)
	}

	// 重复卸载没有状态变化，不再回调。
	if err := registry.Uninstall(ctx, PluginSiteSignIn); err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("重复卸载不应回调: %#v", events)
	}
}
