package scraper

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeasonDirectoryNumber(t *testing.T) {
	cases := []struct {
		name   string
		number int
		ok     bool
	}{
		{"Season 1", 1, true},
		{"season  02", 2, true},
		{"S03", 3, true},
		{"s04", 4, true},
		{"第1季", 1, true},
		{"第12季", 12, true},
		{"Specials", 0, true},
		{"special", 0, true},
		{"Extras", 0, false},
		{"", 0, false},
		{"Season abc", 0, false},
		{"Season 1000", 0, false},
		{"S02.extras", 0, false},
	}
	for _, tc := range cases {
		number, ok := seasonDirectoryNumber(tc.name)
		if ok != tc.ok || (ok && number != tc.number) {
			t.Fatalf("seasonDirectoryNumber(%q) = (%d, %v)，期望 (%d, %v)",
				tc.name, number, ok, tc.number, tc.ok)
		}
	}
}

func TestWriteImageAtomicWritesContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "poster.jpg")
	if err := writeImageAtomic(target, []byte("image-bytes")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(payload) != "image-bytes" {
		t.Fatalf("内容 = %q", payload)
	}
	// 不应残留临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("目录里应只剩目标文件，实际 %d 个", len(entries))
	}
}

func TestWriteImageAtomicNoReplaceKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "poster.jpg")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	// 语义是「不覆盖」而不是「报错」：目标已存在时静默跳过，原文件保持不变。
	if err := writeImageAtomicNoReplace(target, []byte("replacement")); err != nil {
		t.Fatalf("目标已存在时应安静跳过而不是报错: %v", err)
	}
	payload, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("读取失败: %v", readErr)
	}
	if string(payload) != "original" {
		t.Fatalf("原文件被改动: %q", payload)
	}
}

func TestWriteImageAtomicNoReplaceCreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "backdrop.jpg")
	if err := writeImageAtomicNoReplace(target, []byte("fresh")); err != nil {
		t.Fatalf("目标不存在时应写入成功: %v", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(payload) != "fresh" {
		t.Fatalf("内容 = %q", payload)
	}
}
