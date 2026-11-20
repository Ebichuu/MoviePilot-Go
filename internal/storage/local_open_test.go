package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openLocalProvider(t *testing.T) (*LocalProvider, string) {
	t.Helper()
	root := t.TempDir()
	return NewLocalProvider(map[string]string{"local": root}), root
}

func TestLocalOpenServesFileWithContentType(t *testing.T) {
	provider, root := openLocalProvider(t)
	content := []byte("fake-mp4-payload")
	if err := os.MkdirAll(filepath.Join(root, "电影"), 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "电影", "a.mp4"), content, 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	reader, contentType, err := provider.Open(context.Background(), "local", "电影/a.mp4")
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if contentType != "video/mp4" {
		t.Fatalf("内容类型 = %q，期望 video/mp4", contentType)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("内容不匹配: %q", got)
	}
}

func TestLocalOpenFallsBackToOctetStream(t *testing.T) {
	provider, root := openLocalProvider(t)
	if err := os.WriteFile(filepath.Join(root, "blob.weird"), []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	reader, contentType, err := provider.Open(context.Background(), "local", "blob.weird")
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	_ = reader.Close()
	if contentType != "application/octet-stream" {
		t.Fatalf("未知扩展名应回落到 octet-stream，实际 %q", contentType)
	}
}

func TestLocalOpenRejectsInvalidTargets(t *testing.T) {
	provider, root := openLocalProvider(t)
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := map[string]struct {
		ctx     context.Context
		storage string
		path    string
		want    error
	}{
		"文件不存在":    {context.Background(), "local", "missing.mp4", ErrOperationFailed},
		"目标是目录":    {context.Background(), "local", "dir", ErrOperationFailed},
		"路径越界":     {context.Background(), "local", "../../etc/passwd", ErrPathRejected},
		"未知存储名":    {context.Background(), "nope", "a.mp4", ErrUnsupportedStorage},
		"上下文为 nil": {nil, "local", "a.mp4", ErrInvalidRequest},
		"上下文已取消":   {cancelled, "local", "a.mp4", context.Canceled},
	}
	for name, testCase := range cases {
		reader, _, err := provider.Open(testCase.ctx, testCase.storage, testCase.path)
		if reader != nil {
			_ = reader.Close()
			t.Fatalf("%s 不应返回可读流", name)
		}
		if !errors.Is(err, testCase.want) {
			t.Fatalf("%s 错误 = %v，期望 %v", name, err, testCase.want)
		}
	}
}

func TestLocalAuthSurfacesAreStable(t *testing.T) {
	provider, _ := openLocalProvider(t)
	ctx := context.Background()

	// 本地存储不支持扫码/OAuth/登录确认：必须返回 nil 负载 + 可读说明 + nil 错误，
	// 而不是报一个「实现缺失」把前端打成错误态。
	if payload, message, err := provider.QRCode(ctx, "local"); err != nil || payload != nil || message == "" {
		t.Fatalf("QRCode = %v %q %v", payload, message, err)
	}
	if payload, message, err := provider.AuthURL(ctx, "local"); err != nil || payload != nil || message == "" {
		t.Fatalf("AuthURL = %v %q %v", payload, message, err)
	}
	if payload, message, err := provider.CheckLogin(ctx, "local", "ck", "token"); err != nil || payload != nil || message == "" {
		t.Fatalf("CheckLogin = %v %q %v", payload, message, err)
	}
	if err := provider.SaveConfig(ctx, "local", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("SaveConfig 对本地存储应为无操作成功: %v", err)
	}
	if err := provider.ResetConfig(ctx, "local"); err != nil {
		t.Fatalf("ResetConfig 对本地存储应为无操作成功: %v", err)
	}

	// 未知存储名必须报错，不能被当成「不支持但成功」。
	if _, _, err := provider.QRCode(ctx, "nope"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 QRCode 应报错，实际 %v", err)
	}
	if _, _, err := provider.AuthURL(ctx, "nope"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 AuthURL 应报错，实际 %v", err)
	}
	if _, _, err := provider.CheckLogin(ctx, "nope", "", ""); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 CheckLogin 应报错，实际 %v", err)
	}
	if err := provider.SaveConfig(ctx, "nope", nil); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 SaveConfig 应报错，实际 %v", err)
	}
	if err := provider.ResetConfig(ctx, "nope"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未知存储 ResetConfig 应报错，实际 %v", err)
	}
}

func TestSortStringsInPlace(t *testing.T) {
	values := []string{"b", "a", "c"}
	sortStrings(values)
	for index, want := range []string{"a", "b", "c"} {
		if values[index] != want {
			t.Fatalf("排序结果 = %v", values)
		}
	}
	// 空切片与单元素不应 panic。
	sortStrings(nil)
	sortStrings([]string{"only"})
}

func TestModifyTimeValueHandlesNil(t *testing.T) {
	if got := modifyTimeValue(Item{}); got != 0 {
		t.Fatalf("nil 修改时间应视为 0，实际 %v", got)
	}
	stamp := float64(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC).Unix())
	if got := modifyTimeValue(Item{ModifyTime: &stamp}); got != stamp {
		t.Fatalf("修改时间 = %v，期望 %v", got, stamp)
	}
}
