package pluginengine

import (
	"context"
	"testing"
)

// 本文件覆盖内存状态存储的复制语义：读出来的是副本，写进去的也是副本，
// 这样注册表返回的配置被调用方改动时不会反过来污染存储。

func TestMemoryStoreCopiesOnBothSides(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	written := map[string]PersistedState{
		"alpha": {Installed: true, Enabled: true, Config: Config{"text": "a"}},
	}
	if err := store.Save(ctx, written); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	written["alpha"].Config["text"] = "mutated-after-save"

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if loaded["alpha"].Config["text"] != "a" {
		t.Fatalf("保存时应当复制配置: %#v", loaded["alpha"].Config)
	}
	loaded["alpha"].Config["text"] = "mutated-after-load"
	loaded["beta"] = PersistedState{Installed: true}

	again, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if again["alpha"].Config["text"] != "a" {
		t.Fatalf("读取应当返回副本: %#v", again["alpha"].Config)
	}
	if _, present := again["beta"]; present {
		t.Fatalf("调用方新增的条目不应进入存储")
	}
}

func TestMemoryStoreEmptyAndNil(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("空存储应当返回空表: %#v", loaded)
	}
	if err := store.Save(ctx, nil); err != nil {
		t.Fatalf("保存 nil 失败: %v", err)
	}
	loaded, err = store.Load(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("保存 nil 后应当为空: %#v", loaded)
	}
}

func TestCloneConfig(t *testing.T) {
	if cloneConfig(nil) != nil {
		t.Fatalf("nil 配置应当复制为 nil")
	}
	original := Config{"a": 1, "b": "x"}
	copied := cloneConfig(original)
	copied["a"] = 2
	if original["a"] != 1 {
		t.Fatalf("复制后改动影响了原值: %#v", original)
	}
}
