package storage

import (
	"context"
	"errors"
	"testing"
)

func newAlistTestProvider(name, root string) *AlistProvider {
	return &AlistProvider{config: AlistConfig{Name: name, URL: "http://alist.local", Root: root}}
}

func TestAlistRemotePathStaysInsideRoot(t *testing.T) {
	cases := []struct {
		label string
		root  string
		input string
		want  string
	}{
		{"根存储空路径", "/", "", "/"},
		{"根存储子路径", "/", "a/b", "/a/b"},
		{"带根前缀空路径", "/media", "", "/media"},
		{"带根前缀子路径", "/media", "a/b", "/media/a/b"},
		{"带根前缀带首斜杠", "/media", "/abs", "/media/abs"},
		{"清理多余层级", "/media", "a/../b", "/media/b"},
		{"越界被折叠", "/media", "../etc", "/media/etc"},
		{"多级越界被折叠", "/media", "../../etc/passwd", "/media/etc/passwd"},
	}
	for _, testCase := range cases {
		provider := newAlistTestProvider("alist", testCase.root)
		if got := provider.remotePath(testCase.input); got != testCase.want {
			t.Fatalf("%s: remotePath(%q) = %q, 期望 %q", testCase.label, testCase.input, got, testCase.want)
		}
	}
}

func TestAlistProjectItemsMapsEntries(t *testing.T) {
	provider := newAlistTestProvider("alist", "/media")
	entries := []alistFile{
		{Name: "电影", IsDir: true, Modified: "2026-11-19T08:00:00Z"},
		{Name: "a.mkv", Size: 2048, Modified: "2026-11-19T09:30:00Z"},
		{Name: "无扩展名", Size: 1},
	}

	items := provider.projectItems("剧集", entries)
	if len(items) != 3 {
		t.Fatalf("应投影 3 个条目, 实际 %d", len(items))
	}

	dir := items[0]
	if dir.Type != "dir" || dir.Size != nil {
		t.Fatalf("目录不应带体积: %+v", dir)
	}
	if dir.Path != "剧集/电影" || dir.Storage != "alist" || dir.FileID != "电影" {
		t.Fatalf("目录路径/存储/ID 不对: %+v", dir)
	}
	if dir.Children == nil {
		t.Fatal("Children 必须是空切片而不是 nil")
	}
	if dir.ModifyTime == nil {
		t.Fatal("合法时间应被解析")
	}

	file := items[1]
	if file.Type != "file" || file.Extension != "mkv" || file.Size == nil || *file.Size != 2048 {
		t.Fatalf("文件投影不对: %+v", file)
	}
	if file.Basename != "a" {
		t.Fatalf("Basename 应去掉扩展名: %+v", file)
	}

	if items[2].ModifyTime != nil {
		t.Fatalf("缺失的修改时间不应被伪造: %+v", items[2])
	}
	if items[2].Extension != "" {
		t.Fatalf("无扩展名文件不应有 Extension: %+v", items[2])
	}

	if root := provider.projectItems("", entries)[0]; root.Path != "电影" {
		t.Fatalf("根目录条目路径 = %q, 期望 电影", root.Path)
	}
}

// NewAlistProvider 负责把配置归一化：URL 去尾斜杠、Root 补前斜杠去尾斜杠、
// Name 缺省填空。归一化错了，后续所有 remotePath 都会拼错。
func TestNewAlistProviderNormalizesConfig(t *testing.T) {
	provider, err := NewAlistProvider(AlistConfig{URL: " http://alist.local/ ", Root: "media/"})
	if err != nil {
		t.Fatalf("创建提供者失败: %v", err)
	}
	if provider.client == nil {
		t.Fatal("应创建 HTTP 客户端")
	}
	if provider.config.URL != "http://alist.local" {
		t.Fatalf("URL 未归一化: %q", provider.config.URL)
	}
	if provider.config.Root != "/media" {
		t.Fatalf("Root 未归一化: %q", provider.config.Root)
	}
	if provider.config.Name == "" {
		t.Fatal("Name 应有默认值")
	}
	if got := provider.remotePath("a"); got != "/media/a" {
		t.Fatalf("归一化后路径拼接错误: %q", got)
	}

	if _, err := NewAlistProvider(AlistConfig{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("缺少 URL 应返回 ErrInvalidRequest, 实际 %v", err)
	}

	// 根路径缺省是 "/"。
	if rootProvider, err := NewAlistProvider(AlistConfig{URL: "http://alist.local"}); err != nil {
		t.Fatalf("创建提供者失败: %v", err)
	} else if rootProvider.config.Root != "/" {
		t.Fatalf("默认根路径应为 /, 实际 %q", rootProvider.config.Root)
	}
}

func TestAlistRequireChecksStorageName(t *testing.T) {
	provider, err := NewAlistProvider(AlistConfig{URL: "http://alist.local", Root: "/media"})
	if err != nil {
		t.Fatalf("创建提供者失败: %v", err)
	}
	if err := provider.require(context.Background(), provider.config.Name); err != nil {
		t.Fatalf("匹配的存储名应通过: %v", err)
	}
	if err := provider.require(context.Background(), "other"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("不匹配的存储名应返回 ErrUnsupportedStorage, 实际 %v", err)
	}
	nilProvider := (*AlistProvider)(nil)
	if err := nilProvider.require(context.Background(), "alist"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("nil 提供者应返回 ErrUnsupportedStorage, 实际 %v", err)
	}
}
