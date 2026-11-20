package storage

import (
	"errors"
	"testing"
)

func TestNewRcloneProviderGuards(t *testing.T) {
	// 缺少 rclone rc 地址或远端文件系统时必须报错，不能构造出一个永远失败的网盘。
	if _, err := NewRcloneProvider(RcloneConfig{FileSystem: "remote:path"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("缺 URL = %v", err)
	}
	if _, err := NewRcloneProvider(RcloneConfig{URL: "http://127.0.0.1:5572"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("缺文件系统 = %v", err)
	}
	// 只有空白也算缺失。
	if _, err := NewRcloneProvider(RcloneConfig{URL: "   ", FileSystem: "  "}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("全空白 = %v", err)
	}
}

func TestNewRcloneProviderDefaults(t *testing.T) {
	provider, err := NewRcloneProvider(RcloneConfig{
		URL:        " http://127.0.0.1:5572/ ",
		FileSystem: " remote:media ",
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if provider == nil {
		t.Fatal("provider 为空")
	}
	// 未指定名称时用默认名（前端下拉框靠它区分多个网盘）。
	if got := provider.Name(); got != defaultRcloneName {
		t.Fatalf("默认名称 = %q", got)
	}

	custom, err := NewRcloneProvider(RcloneConfig{
		Name: " 我的网盘 ", URL: "http://127.0.0.1:5572", FileSystem: "remote:media",
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if got := custom.Name(); got != "我的网盘" {
		t.Fatalf("自定义名称 = %q", got)
	}
}
