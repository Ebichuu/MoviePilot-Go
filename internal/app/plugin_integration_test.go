//go:build integration

package app

import (
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/pluginengine"
)

// 这个集成测试验证真正的持久化路径：插件状态要能落进 settings 表，
// 并在进程重启（重建注册表）后原样读回。单测用的是内存存储，
// JSON 往返与设置表键名这两件事只有连真库才能证明。
func TestPluginStatePersistsInSettingsTable(t *testing.T) {
	store := newAppIntegrationStore(t)
	ctx := t.Context()

	stateStore, err := pluginStateStore(store)
	if err != nil {
		t.Fatalf("初始化状态存储: %v", err)
	}
	registry, err := newPluginRegistry(ctx, stateStore, nil, nil)
	if err != nil {
		t.Fatalf("初始化注册表: %v", err)
	}

	if err := registry.Install(ctx, pluginengine.PluginCloudBackup); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := registry.Enable(ctx, pluginengine.PluginCloudBackup); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	config := map[string]any{"path": "/data/backup", "keep": 3, "encrypted": false}
	if err := registry.UpdateConfig(ctx, pluginengine.PluginCloudBackup, config); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	// settings 表里应当有一条以默认键为名的记录。
	var raw map[string]any
	found, err := store.GetSetting(ctx, pluginengine.DefaultStateSettingKey, &raw)
	if err != nil {
		t.Fatalf("读取设置: %v", err)
	}
	if !found || len(raw) == 0 {
		t.Fatalf("未写入 %s", pluginengine.DefaultStateSettingKey)
	}

	// 重建注册表 = 模拟进程重启，状态必须原样回来。
	rebuilt, err := newPluginRegistry(ctx, stateStore, nil, nil)
	if err != nil {
		t.Fatalf("重建注册表: %v", err)
	}
	plugin, err := rebuilt.Get(pluginengine.PluginCloudBackup)
	if err != nil {
		t.Fatalf("读取插件: %v", err)
	}
	if !plugin.Installed || !plugin.Enabled {
		t.Fatalf("安装/启用状态未持久化: %#v", plugin)
	}
	if plugin.Config["path"] != "/data/backup" {
		t.Fatalf("path 未持久化: %#v", plugin.Config)
	}
	if plugin.Config["keep"] != int64(3) {
		t.Fatalf("keep 未持久化: %#v", plugin.Config)
	}
	if plugin.Config["encrypted"] != false {
		t.Fatalf("布尔配置未持久化: %#v", plugin.Config)
	}

	// 卸载要把「装没装、开没开」一起复位，避免留下半截状态。
	if err := rebuilt.Uninstall(ctx, pluginengine.PluginCloudBackup); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	after, err := rebuilt.Get(pluginengine.PluginCloudBackup)
	if err != nil {
		t.Fatalf("读取插件: %v", err)
	}
	if after.Installed || after.Enabled {
		t.Fatalf("卸载后状态 = %#v", after)
	}
	if strings.TrimSpace(after.Config["path"].(string)) != "" {
		t.Fatalf("卸载后配置应回到默认值: %#v", after.Config)
	}
}
