package pluginengine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// 本文件覆盖「设置表版」StateStore：它把插件状态整条存进 settings 表，
// 因此必须验证三件事：默认键、缺失记录、以及读写失败会如实上抛。

// fakeSettingsStore 用 JSON 往返模拟 database.Store 的行为（存 JSON、读反序列化）。
type fakeSettingsStore struct {
	values  map[string]any
	failGet error
	failSet error
	getHits int
}

func (f *fakeSettingsStore) GetSetting(_ context.Context, key string, target any) (bool, error) {
	f.getHits++
	if f.failGet != nil {
		return false, f.failGet
	}
	value, ok := f.values[key]
	if !ok {
		return false, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeSettingsStore) SetSetting(_ context.Context, key string, value any, _ bool) error {
	if f.failSet != nil {
		return f.failSet
	}
	if f.values == nil {
		f.values = map[string]any{}
	}
	f.values[key] = value
	return nil
}

func TestNewSettingsStateStoreGuards(t *testing.T) {
	if _, err := NewSettingsStateStore(nil, ""); err == nil {
		t.Fatalf("nil 设置存储应当被拒绝")
	}
	store := &fakeSettingsStore{}
	stateStore, err := NewSettingsStateStore(store, "   ")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if err := stateStore.Save(context.Background(), map[string]PersistedState{}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, present := store.values[DefaultStateSettingKey]; !present {
		t.Fatalf("空键应当回退到默认键 %s: %#v", DefaultStateSettingKey, store.values)
	}
}

func TestSettingsStateStoreLoadMissing(t *testing.T) {
	stateStore, err := NewSettingsStateStore(&fakeSettingsStore{}, "CustomKey")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	state, err := stateStore.Load(context.Background())
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if state == nil || len(state) != 0 {
		t.Fatalf("缺失记录应当返回空表而不是 nil: %#v", state)
	}
}

func TestSettingsStateStoreErrors(t *testing.T) {
	loadErr := errors.New("读取失败")
	stateStore, err := NewSettingsStateStore(&fakeSettingsStore{failGet: loadErr}, "k")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if _, err := stateStore.Load(context.Background()); !errors.Is(err, loadErr) {
		t.Fatalf("error = %v", err)
	}

	saveErr := errors.New("写入失败")
	stateStore, err = NewSettingsStateStore(&fakeSettingsStore{failSet: saveErr}, "k")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if err := stateStore.Save(context.Background(), nil); !errors.Is(err, saveErr) {
		t.Fatalf("error = %v", err)
	}
}

func TestRegistryWithSettingsStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	settings := &fakeSettingsStore{}
	stateStore, err := NewSettingsStateStore(settings, "")
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	first := NewRegistry(stateStore)
	if err := first.RegisterAll(BuiltinDefinitions(Services{})); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := first.Install(ctx, PluginSiteSignIn); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if err := first.UpdateConfig(ctx, PluginSiteSignIn, map[string]any{"interval_hours": 12}); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}
	if err := first.Enable(ctx, PluginSiteSignIn); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	second, err := NewBuiltinRegistry(ctx, stateStore, Services{})
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	restored, err := second.Get(PluginSiteSignIn)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !restored.Installed || !restored.Enabled {
		t.Fatalf("状态未恢复: %#v", restored)
	}
	if restored.Config["interval_hours"] != int64(12) {
		t.Fatalf("配置未恢复: %#v", restored.Config)
	}
	if settings.getHits == 0 {
		t.Fatalf("恢复过程应当读过设置表")
	}
}
