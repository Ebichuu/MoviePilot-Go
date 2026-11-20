package database

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/moviepilot-go/moviepilot-go/internal/auth"
	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

// 本文件覆盖媒体服务器与下载器设置存储的闸门与编解码失败分支：
// 未就绪 store、连接不可用、加密器缺失、非法 JSON、无效配置。

func settingsGuardStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("创建不可用连接池: %v", err)
	}
	t.Cleanup(pool.Close)
	return &Store{db: &DB{Pool: pool}}
}

func settingsGuardBox(t *testing.T) *auth.SecretBox {
	t.Helper()
	box, err := auth.NewSecretBox("settings-guard-master-key")
	if err != nil {
		t.Fatalf("创建加密器: %v", err)
	}
	return box
}

func TestLoadMediaServersGuards(t *testing.T) {
	ctx := context.Background()

	var nilStore *Store
	if _, found, err := nilStore.LoadMediaServers(ctx); err == nil || found {
		t.Fatalf("nil store = found %v, err %v", found, err)
	}
	if _, found, err := settingsGuardStore(t).LoadMediaServers(ctx); err == nil || found {
		t.Fatalf("连接不可用 = found %v, err %v", found, err)
	}
}

func TestSaveMediaServersGuards(t *testing.T) {
	ctx := context.Background()

	if _, err := (&Store{}).SaveMediaServers(ctx, nil); err == nil {
		t.Fatal("未就绪 store 应报错")
	}

	duplicated := []mediaconfig.Server{{Name: "同一台"}, {Name: "同一台"}}
	if _, _, err := prepareMediaServersSetting(nil, duplicated); err == nil {
		t.Fatal("重名服务器应被拒绝")
	}

	if _, _, err := prepareMediaServersSetting(nil, nil); err == nil {
		t.Fatal("缺少加密器应报错")
	}

	store := &Store{db: settingsGuardStore(t).db, secretBox: settingsGuardBox(t)}
	if _, err := store.SaveMediaServers(ctx, nil); err == nil {
		t.Fatal("连接不可用应报错")
	}
	if err := store.saveMediaServersSetting(ctx, json.RawMessage("[]")); err == nil {
		t.Fatal("保存时连接不可用应报错")
	}
}

func TestMediaServersSettingCodecGuards(t *testing.T) {
	if _, _, err := decodeMediaServersSnapshot(json.RawMessage("{")); err == nil {
		t.Fatal("非法 JSON 快照应报错")
	}
	if _, err := encodeMediaServersSetting(nil, json.RawMessage("[]")); err == nil {
		t.Fatal("缺少加密器应报错")
	}
	if _, err := encodeMediaServersSetting(settingsGuardBox(t), json.RawMessage("{")); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := decodeMediaServersSetting(nil, json.RawMessage("{"), false); err == nil {
		t.Fatal("非加密记录的非法 JSON 应报错")
	}
	if _, err := decodeMediaServersSetting(nil, json.RawMessage("[]"), true); err == nil {
		t.Fatal("加密记录缺少解密器应报错")
	}
	if _, err := decodeMediaServersSetting(settingsGuardBox(t), json.RawMessage("{}"), true); err == nil {
		t.Fatal("密文格式无效应报错")
	}
}

func TestDownloadersSettingGuards(t *testing.T) {
	ctx := context.Background()

	var nilStore *Store
	if _, found, err := nilStore.LoadDownloaders(ctx); err == nil || found {
		t.Fatalf("nil store = found %v, err %v", found, err)
	}
	if _, _, _, err := nilStore.loadDownloadersSetting(ctx); err == nil {
		t.Fatal("nil store 读取设置应报错")
	}
	if _, _, _, err := settingsGuardStore(t).loadDownloadersSetting(ctx); err == nil {
		t.Fatal("连接不可用读取设置应报错")
	}
	if err := nilStore.saveDownloadersSetting(ctx, json.RawMessage("[]")); err == nil {
		t.Fatal("nil store 保存设置应报错")
	}

	store := &Store{db: settingsGuardStore(t).db, secretBox: settingsGuardBox(t)}
	if err := store.saveDownloadersSetting(ctx, json.RawMessage("[]")); err == nil {
		t.Fatal("保存时连接不可用应报错")
	}

	withoutBox := &Store{db: settingsGuardStore(t).db}
	if err := withoutBox.saveDownloadersSetting(ctx, json.RawMessage("[]")); err == nil {
		t.Fatal("缺少加密器保存应报错")
	}
}

func TestSaveDownloadersRejectsInvalidSettings(t *testing.T) {
	ctx := context.Background()
	if err := (&Store{}).SaveDownloaders(ctx, []downloadconfig.Downloader{{Name: "同名"}, {Name: "同名"}}); err == nil {
		t.Fatal("重名下载器应被拒绝")
	}
	if err := (&Store{}).SaveDownloaders(ctx, []downloadconfig.Downloader{
		{Name: "qb", Config: map[string]any{"password": 123}},
	}); err == nil {
		t.Fatal("非字符串密码应被拒绝")
	}
	// 类型齐备时规范化通过，敏感字段清理仍会拒绝非字符串密码。
	if err := (&Store{}).SaveDownloaders(ctx, []downloadconfig.Downloader{{
		Name: "qb", Type: "qbittorrent", Config: map[string]any{"password": 123},
	}}); err == nil {
		t.Fatal("敏感字段类型非法应被拒绝")
	}
}
