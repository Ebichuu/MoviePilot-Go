package pluginengine

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
)

// 本文件覆盖注册表的完整生命周期：注册闸门、安装/启停、配置更新、动作分派、
// 持久化往返与「保存失败必须回滚」的写入纪律。

func testDefinition(id string, capability Capability) Definition {
	return Definition{
		Manifest: Manifest{
			ID:      id,
			Name:    "测试插件 " + id,
			Version: "1.0.0",
			Fields: []Field{
				{Key: "text", Label: "文本", Type: FieldString, Required: true},
				{Key: "count", Label: "数量", Type: FieldInteger, Default: int64(3)},
			},
		},
		Actions: map[string]ActionFunc{ActionRun: capability},
	}
}

func mustRegistry(t *testing.T, defs ...Definition) *Registry {
	t.Helper()
	registry := NewRegistry(nil)
	if err := registry.RegisterAll(defs); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	return registry
}

func TestRegistryRegisterGuards(t *testing.T) {
	ctx := context.Background()
	registry := mustRegistry(t, testDefinition("alpha", nil))

	t.Run("重复注册", func(t *testing.T) {
		err := registry.Register(testDefinition("alpha", nil))
		if !errors.Is(err, ErrPluginExists) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("清单不合法时不入表", func(t *testing.T) {
		bad := Definition{Manifest: Manifest{ID: "bad", Version: "1.0.0"}}
		if err := registry.Register(bad); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("error = %v", err)
		}
		if _, err := registry.Get("bad"); !errors.Is(err, ErrPluginNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("批量注册是原子的", func(t *testing.T) {
		defs := []Definition{
			testDefinition("beta", nil),
			{Manifest: Manifest{ID: "gamma", Name: "", Version: "1.0.0"}},
		}
		if err := registry.RegisterAll(defs); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("error = %v", err)
		}
		if _, err := registry.Get("beta"); !errors.Is(err, ErrPluginNotFound) {
			t.Fatalf("beta 不应被注册: %v", err)
		}
	})

	t.Run("批量内重复", func(t *testing.T) {
		defs := []Definition{testDefinition("delta", nil), testDefinition("delta", nil)}
		if err := registry.RegisterAll(defs); !errors.Is(err, ErrPluginExists) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("未注册的插件", func(t *testing.T) {
		if _, err := registry.Get("nope"); !errors.Is(err, ErrPluginNotFound) {
			t.Fatalf("error = %v", err)
		}
		for _, call := range []error{
			registry.Install(ctx, "nope"),
			registry.Uninstall(ctx, "nope"),
			registry.Enable(ctx, "nope"),
			registry.Disable(ctx, "nope"),
			registry.UpdateConfig(ctx, "nope", nil),
		} {
			if !errors.Is(call, ErrPluginNotFound) {
				t.Fatalf("error = %v", call)
			}
		}
	})

	t.Run("列表顺序与动作名", func(t *testing.T) {
		list := registry.List()
		if len(list) != 1 || list[0].ID != "alpha" {
			t.Fatalf("list = %#v", list)
		}
		if len(list[0].Actions) != 1 || list[0].Actions[0] != ActionRun {
			t.Fatalf("actions = %#v", list[0].Actions)
		}
	})
}

func TestRegistryLifecycle(t *testing.T) {
	ctx := context.Background()
	var gotConfig Config
	var gotArgs map[string]any
	registry := mustRegistry(t, testDefinition("alpha", func(_ context.Context, in RunInput) (Result, error) {
		gotConfig = in.Config
		gotArgs = in.Args
		return Result{Message: "完成", Details: map[string]any{"ok": true}}, nil
	}))

	if plugin, _ := registry.Get("alpha"); plugin.Installed || plugin.Enabled {
		t.Fatalf("新注册的插件应当是未安装未启用: %#v", plugin)
	}

	if err := registry.Enable(ctx, "alpha"); !errors.Is(err, ErrPluginNotInstalled) {
		t.Fatalf("未安装不应能启用: %v", err)
	}
	if _, err := registry.Run(ctx, "alpha", ActionRun, nil); !errors.Is(err, ErrPluginNotInstalled) {
		t.Fatalf("未安装不应能执行: %v", err)
	}

	if err := registry.Install(ctx, "alpha"); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := registry.Install(ctx, "alpha"); err != nil {
		t.Fatalf("重复安装应当是空操作: %v", err)
	}
	if _, err := registry.Run(ctx, "alpha", ActionRun, nil); !errors.Is(err, ErrPluginDisabled) {
		t.Fatalf("未启用不应能执行: %v", err)
	}

	if err := registry.Enable(ctx, "alpha"); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if plugin, _ := registry.Get("alpha"); plugin.Config["text"] != nil {
		t.Fatalf("没有默认值的必填项不应被伪造: %#v", plugin.Config)
	}
	_ = gotConfig

	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{"text": "hello"}); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	result, err := registry.Run(ctx, "alpha", ActionRun, map[string]any{"reason": "manual"})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Message != "完成" || result.Details["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
	if gotConfig["text"] != "hello" || gotConfig["count"] != int64(3) {
		t.Fatalf("执行时拿到的配置不对: %#v", gotConfig)
	}
	if gotArgs["reason"] != "manual" {
		t.Fatalf("args 未透传: %#v", gotArgs)
	}

	if err := registry.Disable(ctx, "alpha"); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, err := registry.Run(ctx, "alpha", ActionRun, nil); !errors.Is(err, ErrPluginDisabled) {
		t.Fatalf("停用后不应能执行: %v", err)
	}

	if err := registry.Uninstall(ctx, "alpha"); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	plugin, _ := registry.Get("alpha")
	if plugin.Installed || plugin.Enabled {
		t.Fatalf("卸载后应当是未安装未启用: %#v", plugin)
	}
	if len(plugin.Config) != 1 || plugin.Config["count"] != int64(3) {
		t.Fatalf("卸载后应回到默认配置: %#v", plugin.Config)
	}
}

func TestRegistryRunErrorOrder(t *testing.T) {
	ctx := context.Background()
	registry := mustRegistry(t,
		testDefinition("alpha", nil),
		testDefinition("beta", func(context.Context, RunInput) (Result, error) {
			return Result{}, errors.New("业务失败")
		}),
	)

	if _, err := registry.Run(ctx, "alpha", ActionRun, nil); !errors.Is(err, ErrPluginNotInstalled) {
		t.Fatalf("error = %v", err)
	}
	if err := registry.Install(ctx, "alpha"); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{"text": "a"}); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if err := registry.Enable(ctx, "alpha"); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	t.Run("上下文为 nil", func(t *testing.T) {
		var nilCtx context.Context
		if _, err := registry.Run(nilCtx, "alpha", ActionRun, nil); err == nil {
			t.Fatalf("nil 上下文应当被拒绝")
		}
	})

	t.Run("动作不存在", func(t *testing.T) {
		if _, err := registry.Run(ctx, "alpha", "explode", nil); !errors.Is(err, ErrUnknownAction) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("能力未接线", func(t *testing.T) {
		if _, err := registry.Run(ctx, "alpha", ActionRun, nil); !errors.Is(err, ErrServiceUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("业务错误原样上抛", func(t *testing.T) {
		if err := registry.Install(ctx, "beta"); err != nil {
			t.Fatalf("安装失败: %v", err)
		}
		if err := registry.UpdateConfig(ctx, "beta", map[string]any{"text": "b"}); err != nil {
			t.Fatalf("更新配置失败: %v", err)
		}
		if err := registry.Enable(ctx, "beta"); err != nil {
			t.Fatalf("启用失败: %v", err)
		}
		if _, err := registry.Run(ctx, "beta", ActionRun, nil); err == nil || err.Error() != "业务失败" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRegistryConfigGuards(t *testing.T) {
	ctx := context.Background()
	registry := mustRegistry(t, testDefinition("alpha", nil))

	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{"text": "a"}); !errors.Is(err, ErrPluginNotInstalled) {
		t.Fatalf("未安装不应能改配置: %v", err)
	}
	if err := registry.Install(ctx, "alpha"); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{"unknown": 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("error = %v", err)
	}
	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("缺少必填应当被拒绝: %v", err)
	}
	before, _ := registry.Get("alpha")
	if _, present := before.Config["text"]; present {
		t.Fatalf("安装后不应凭空出现必填项: %#v", before.Config)
	}
	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{"text": "a"}); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	after, _ := registry.Get("alpha")
	if after.Config["text"] != "a" {
		t.Fatalf("after = %#v", after.Config)
	}

	// 浏览器里的数字输入框交回字符串：这类配置必须能存下来并被归一成整数。
	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{"text": "a", "count": "3"}); err != nil {
		t.Fatalf("数字字符串应当被接受: %v", err)
	}
	numeric, _ := registry.Get("alpha")
	if numeric.Config["count"] != int64(3) {
		t.Fatalf("count = %#v", numeric.Config["count"])
	}

	// 快照必须是副本：调用方改它不能影响注册表内部状态。
	after.Config["text"] = "mutated"
	again, _ := registry.Get("alpha")
	if again.Config["text"] != "a" {
		t.Fatalf("内部配置被外部改动: %#v", again.Config)
	}
}

func TestRegistryPersistence(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	first := NewRegistry(store)
	if err := first.RegisterAll(BuiltinDefinitions(Services{})); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := first.Install(ctx, PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := first.UpdateConfig(ctx, PluginSiteSignIn, map[string]any{"interval_hours": 6, "notify": true}); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if err := first.Enable(ctx, PluginSiteSignIn); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if err := first.Uninstall(ctx, PluginCloudBackup); err != nil {
		t.Fatalf("卸载未安装的插件应当是空操作: %v", err)
	}

	second, err := NewBuiltinRegistry(ctx, store, Services{})
	if err != nil {
		t.Fatalf("恢复状态失败: %v", err)
	}
	restored, err := second.Get(PluginSiteSignIn)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !restored.Installed || !restored.Enabled {
		t.Fatalf("状态未恢复: %#v", restored)
	}
	if restored.Config["interval_hours"] != int64(6) || restored.Config["notify"] != true {
		t.Fatalf("配置未恢复: %#v", restored.Config)
	}
	others, _ := second.Get(PluginRSSEnhance)
	if others.Installed {
		t.Fatalf("未安装的插件不应被恢复成已安装: %#v", others)
	}

	// 清空状态后再次恢复，应当回到「全部未安装」。
	if err := store.Save(ctx, map[string]PersistedState{}); err != nil {
		t.Fatalf("清空状态失败: %v", err)
	}
	third := NewRegistry(store)
	if err := third.RegisterAll(BuiltinDefinitions(Services{})); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := third.Restore(ctx); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if plugin, _ := third.Get(PluginSiteSignIn); plugin.Installed {
		t.Fatalf("空状态不应恢复出已安装: %#v", plugin)
	}
}

func TestRegistryRestoreFallsBackOnStaleConfig(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.Save(ctx, map[string]PersistedState{
		"alpha": {Installed: true, Enabled: true, Config: Config{"text": "old", "dropped": "x"}},
	}); err != nil {
		t.Fatalf("写入状态失败: %v", err)
	}
	registry := NewRegistry(store)
	if err := registry.RegisterAll([]Definition{testDefinition("alpha", nil)}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	err := registry.Restore(ctx)
	if err == nil {
		t.Fatalf("清单变更导致旧配置失效时应当返回告警")
	}
	plugin, _ := registry.Get("alpha")
	if !plugin.Installed || !plugin.Enabled {
		t.Fatalf("安装与启用状态应当保留: %#v", plugin)
	}
	if len(plugin.Config) != 1 || plugin.Config["count"] != int64(3) {
		t.Fatalf("配置应当回退默认值: %#v", plugin.Config)
	}

	// 未知插件的遗留状态不应阻塞恢复。
	if err := store.Save(ctx, map[string]PersistedState{"gone": {Installed: true}}); err != nil {
		t.Fatalf("写入状态失败: %v", err)
	}
	if err := registry.Restore(ctx); err != nil {
		t.Fatalf("遗留状态不应报错: %v", err)
	}
}

type failingStore struct{}

func (failingStore) Load(context.Context) (map[string]PersistedState, error) {
	return nil, errors.New("读取失败")
}

func (failingStore) Save(context.Context, map[string]PersistedState) error {
	return errors.New("写入失败")
}

func TestRegistryRollsBackWhenSaveFails(t *testing.T) {
	ctx := context.Background()
	registry := NewRegistry(failingStore{})
	if err := registry.RegisterAll([]Definition{testDefinition("alpha", nil)}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := registry.Restore(ctx); err == nil {
		t.Fatalf("读取失败应当上抛")
	}
	if err := registry.Install(ctx, "alpha"); err == nil {
		t.Fatalf("写入失败应当上抛")
	}
	if plugin, _ := registry.Get("alpha"); plugin.Installed {
		t.Fatalf("落盘失败后内存状态必须回滚: %#v", plugin)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	registry := mustRegistry(t,
		testDefinition("alpha", func(context.Context, RunInput) (Result, error) {
			return Result{Message: "ok"}, nil
		}),
		testDefinition("beta", nil),
	)
	if err := registry.Install(ctx, "alpha"); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := registry.UpdateConfig(ctx, "alpha", map[string]any{"text": "a"}); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}

	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 50; j++ {
				_, _ = registry.Get("alpha")
				_ = registry.List()
				_ = registry.Enable(ctx, "alpha")
				_, _ = registry.Run(ctx, "alpha", ActionRun, nil)
				_ = registry.Disable(ctx, "alpha")
			}
		}()
	}
	wait.Wait()

	ids := make([]string, 0, 2)
	for _, plugin := range registry.List() {
		ids = append(ids, plugin.ID)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "alpha" || ids[1] != "beta" {
		t.Fatalf("ids = %#v", ids)
	}
}
