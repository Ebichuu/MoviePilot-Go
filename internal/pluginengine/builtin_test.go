package pluginengine

import (
	"context"
	"errors"
	"testing"
)

// 本文件锁定「5 个原生插件」的形状：清单齐全、动作统一、未接线时如实拒绝，
// 接线后每个能力拿到的是自己那份已归一配置。

func TestBuiltinDefinitionsShape(t *testing.T) {
	defs := BuiltinDefinitions(Services{})
	want := []string{
		PluginSiteSignIn,
		PluginCloudBackup,
		PluginRSSEnhance,
		PluginSubscriptionAssistant,
		PluginMediaLibrarySync,
	}
	if len(defs) != len(want) {
		t.Fatalf("插件数量 = %d, 期望 %d", len(defs), len(want))
	}
	seen := map[string]bool{}
	for index, def := range defs {
		if def.Manifest.ID != want[index] {
			t.Fatalf("第 %d 个插件 ID = %s, 期望 %s", index, def.Manifest.ID, want[index])
		}
		if def.Manifest.Name == "" || def.Manifest.Version == "" || def.Manifest.Description == "" {
			t.Fatalf("插件 %s 的清单不完整: %#v", def.Manifest.ID, def.Manifest)
		}
		if err := def.Manifest.validate(); err != nil {
			t.Fatalf("插件 %s 的清单不合法: %v", def.Manifest.ID, err)
		}
		if _, ok := def.Actions[ActionRun]; !ok {
			t.Fatalf("插件 %s 缺少 %s 动作", def.Manifest.ID, ActionRun)
		}
		if seen[def.Manifest.ID] {
			t.Fatalf("插件 ID 重复: %s", def.Manifest.ID)
		}
		seen[def.Manifest.ID] = true

		// 必填项不能是「要求用户填但没有默认值」的状态过久：这里只断言
		// 每个插件至少有一个带默认值的字段，保证装完即可运行。
		hasDefault := false
		for _, field := range def.Manifest.Fields {
			if field.Default != nil {
				hasDefault = true
			}
		}
		if len(def.Manifest.Fields) > 0 && !hasDefault {
			t.Fatalf("插件 %s 没有任何默认配置，装完无法直接运行", def.Manifest.ID)
		}
	}
}

func TestBuiltinPluginsRefuseUnwiredCapabilities(t *testing.T) {
	ctx := context.Background()
	registry, err := NewBuiltinRegistry(ctx, nil, Services{})
	if err != nil {
		t.Fatalf("建注册表失败: %v", err)
	}
	for _, def := range BuiltinDefinitions(Services{}) {
		id := def.Manifest.ID
		if err := registry.Install(ctx, id); err != nil {
			t.Fatalf("安装 %s 失败: %v", id, err)
		}
		if err := registry.Enable(ctx, id); err != nil {
			t.Fatalf("启用 %s 失败: %v", id, err)
		}
		if _, err := registry.Run(ctx, id, ActionRun, nil); !errors.Is(err, ErrServiceUnavailable) {
			t.Fatalf("%s 未接线时应当返回 ErrServiceUnavailable，实际 %v", id, err)
		}
	}
}

func TestBuiltinPluginsDispatchToServices(t *testing.T) {
	ctx := context.Background()
	var calls []string
	record := func(name string) Capability {
		return func(_ context.Context, in RunInput) (Result, error) {
			calls = append(calls, name)
			return Result{Message: name, Details: map[string]any{"config": in.Config}}, nil
		}
	}
	services := Services{
		SignInAll:          record(PluginSiteSignIn),
		Backup:             record(PluginCloudBackup),
		EnhanceRSS:         record(PluginRSSEnhance),
		AssistSubscription: record(PluginSubscriptionAssistant),
		SyncMediaServers:   record(PluginMediaLibrarySync),
	}
	registry, err := NewBuiltinRegistry(ctx, nil, services)
	if err != nil {
		t.Fatalf("建注册表失败: %v", err)
	}

	for _, id := range []string{
		PluginSiteSignIn,
		PluginCloudBackup,
		PluginRSSEnhance,
		PluginSubscriptionAssistant,
		PluginMediaLibrarySync,
	} {
		if err := registry.Install(ctx, id); err != nil {
			t.Fatalf("安装 %s 失败: %v", id, err)
		}
		if err := registry.Enable(ctx, id); err != nil {
			t.Fatalf("启用 %s 失败: %v", id, err)
		}
		result, err := registry.Run(ctx, id, ActionRun, nil)
		if err != nil {
			t.Fatalf("执行 %s 失败: %v", id, err)
		}
		if result.Message != id {
			t.Fatalf("插件 %s 分派到了错误的能力: %s", id, result.Message)
		}
	}
	if len(calls) != 5 {
		t.Fatalf("调用次数 = %d, 期望 5: %#v", len(calls), calls)
	}

	// 每个插件拿到的必须是自己的默认配置，不能被别的插件串味。
	signIn, _ := registry.Get(PluginSiteSignIn)
	if signIn.Config["interval_hours"] != int64(24) {
		t.Fatalf("site_signin 默认间隔 = %#v", signIn.Config["interval_hours"])
	}
	backup, _ := registry.Get(PluginCloudBackup)
	if backup.Config["keep"] != int64(7) || backup.Config["encrypted"] != true {
		t.Fatalf("cloud_backup 默认配置 = %#v", backup.Config)
	}
	if backup.Config["interval_hours"] != int64(24) {
		t.Fatalf("cloud_backup 默认间隔 = %#v", backup.Config["interval_hours"])
	}
	sync, _ := registry.Get(PluginMediaLibrarySync)
	if sync.Config["servers"] != "" || sync.Config["notify"] != false {
		t.Fatalf("media_library_sync 默认配置 = %#v", sync.Config)
	}
	// 同步周期由媒体服务器设置决定，插件配置里刻意没有这一项。
	if _, ok := sync.Config["interval_minutes"]; ok {
		t.Fatalf("media_library_sync 不应保存自己的同步间隔: %#v", sync.Config)
	}
}
