//go:build linux

package organizer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishViaHardLink(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := publishViaHardLink(source, target); err != nil {
		t.Fatalf("硬链接发布失败: %v", err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "payload" {
		t.Fatalf("目标内容 = %q %v", content, err)
	}
	if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("发布后源文件应被移除")
	}

	// 目标已存在时 os.Link 失败，源文件必须保留。
	sourceAgain := filepath.Join(dir, "source-again")
	if err := os.WriteFile(sourceAgain, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publishViaHardLink(sourceAgain, target); err == nil {
		t.Fatal("目标已存在应报错")
	}
	if _, err := os.Lstat(sourceAgain); err != nil {
		t.Fatal("失败时不应删除源文件")
	}

	// 源不存在。
	if err := publishViaHardLink(filepath.Join(dir, "missing"), filepath.Join(dir, "other")); err == nil {
		t.Fatal("源不存在应报错")
	}
}

func TestPublishNoReplace(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := publishNoReplace(source, target); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "payload" {
		t.Fatalf("目标内容 = %q %v", content, err)
	}
	if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("发布后源文件应被移除")
	}
}

func TestOpenParentDirNoFollow(t *testing.T) {
	if _, _, err := openParentDirNoFollow("relative/path"); err == nil ||
		!strings.Contains(err.Error(), "路径必须为绝对路径") {
		t.Fatalf("相对路径 = %v", err)
	}
	if _, _, err := openParentDirNoFollow(string(filepath.Separator)); err == nil ||
		!strings.Contains(err.Error(), "路径缺少文件名") {
		t.Fatalf("根路径 = %v", err)
	}

	dir := t.TempDir()
	handle, base, err := openParentDirNoFollow(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("打开父目录失败: %v", err)
	}
	if base != "file.txt" {
		t.Fatalf("文件名 = %q", base)
	}
	if name := handle.Name(); name != dir {
		t.Fatalf("目录句柄 = %q，期望 %q", name, dir)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("关闭句柄: %v", err)
	}

	// 祖先包含软链接时 O_NOFOLLOW 直接失败。
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link-dir")
	createDirectorySymlinkOrSkip(t, realDir, linkDir)
	if _, _, err := openParentDirNoFollow(filepath.Join(linkDir, "file.txt")); err == nil {
		t.Fatal("软链接祖先应报错")
	}

	// 祖先是普通文件时 openat 返回 ENOTDIR。
	fileAncestor := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(fileAncestor, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openParentDirNoFollow(filepath.Join(fileAncestor, "child")); err == nil {
		t.Fatal("文件祖先应报错")
	}
}

func TestVerifyParentDirNoFollow(t *testing.T) {
	dir := t.TempDir()
	if err := verifyParentDirNoFollow(filepath.Join(dir, "file.txt")); err != nil {
		t.Fatalf("正常路径 = %v", err)
	}
	if err := verifyParentDirNoFollow(filepath.Join(dir, "missing", "file.txt")); err != nil {
		t.Fatalf("目标目录尚未创建应通过: %v", err)
	}

	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "link-dir")
	createDirectorySymlinkOrSkip(t, realDir, linkDir)
	if err := verifyParentDirNoFollow(filepath.Join(linkDir, "file.txt")); err == nil {
		t.Fatal("软链接祖先应报错")
	}
	if err := verifyParentDirNoFollow("relative/path"); err == nil {
		t.Fatal("相对路径应报错")
	}
}
