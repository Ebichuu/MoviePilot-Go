package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 本地存储的全部安全性都压在这一个函数上：任何能逃出根目录的路径都必须被拒。
func TestResolvePathRejectsEscapes(t *testing.T) {
	root := filepath.FromSlash("/srv/media")

	if got, err := resolvePath(root, ""); err != nil || got != root {
		t.Fatalf("空路径应返回根目录: %q %v", got, err)
	}
	if got, err := resolvePath(root, "电影/2026"); err != nil || got != filepath.Join(root, "电影", "2026") {
		t.Fatalf("相对路径应落在根目录下: %q %v", got, err)
	}
	if got, err := resolvePath(root, "./电影/../音乐"); err != nil || got != filepath.Join(root, "音乐") {
		t.Fatalf("合法的 . / .. 组合应被清理而非拒绝: %q %v", got, err)
	}

	escapes := []string{"..", "../etc", "电影/../../etc", "电影/..", "./../x"}
	for _, requested := range escapes {
		// 「电影/..」会被清理成 "."，等同于根目录，属于合法；其余必须拒绝。
		cleaned := filepath.Clean(filepath.FromSlash(requested))
		if cleaned == "." {
			continue
		}
		if _, err := resolvePath(root, requested); !errors.Is(err, ErrPathRejected) {
			t.Fatalf("%q 应被判为越界，实际 err=%v", requested, err)
		}
	}

	absolute := []string{"/etc/passwd"}
	if runtime.GOOS == "windows" {
		absolute = []string{`C:\Windows\win.ini`, `\\server\share`}
	}
	for _, requested := range absolute {
		if _, err := resolvePath(root, requested); !errors.Is(err, ErrPathRejected) {
			t.Fatalf("绝对路径 %q 必须拒绝，实际 err=%v", requested, err)
		}
	}
}

func TestLocalProviderRegisterAndRoot(t *testing.T) {
	dir := t.TempDir()
	provider := NewLocalProvider(map[string]string{"local": dir})

	root, err := provider.root("local")
	if err != nil {
		t.Fatalf("已注册的存储应可取根目录: %v", err)
	}
	if root != filepath.Clean(dir) {
		t.Fatalf("根目录 = %q，期望 %q", root, filepath.Clean(dir))
	}
	if _, err := provider.root("  local  "); err != nil {
		t.Fatalf("存储名应去空格: %v", err)
	}
	if _, err := provider.root("nope"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未注册的存储应返回 ErrUnsupportedStorage，实际 %v", err)
	}

	// 空名/空目录的注册应被忽略而不是清空已有映射。
	provider.Register("", dir)
	provider.Register("local", "   ")
	if _, err := provider.root("local"); err != nil {
		t.Fatalf("空参数注册不应破坏已有映射: %v", err)
	}

	var nilProvider *LocalProvider
	if _, err := nilProvider.root("local"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("nil 提供者应返回 ErrUnsupportedStorage，实际 %v", err)
	}
	if _, err := nilProvider.List(context.Background(), "local", "", "", ""); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("nil 提供者的 List 不应 panic，应返回 ErrUnsupportedStorage，实际 %v", err)
	}
}

func seedStorageTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "电影"), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	files := map[string]string{
		"b.mkv":       "b",
		"a.txt":       "a",
		"电影/c.nfo":    "c",
		"电影/第10集.mkv": "d",
		"电影/第2集.mkv":  "e",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s 失败: %v", name, err)
		}
	}
	// 固定 mtime，避免「按时间排序」受写入顺序抖动影响。
	old := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	_ = os.Chtimes(filepath.Join(root, "a.txt"), old, old)
	_ = os.Chtimes(filepath.Join(root, "b.mkv"), newer, newer)
	return root
}

func TestLocalProviderListProjectsEntries(t *testing.T) {
	root := seedStorageTree(t)
	provider := NewLocalProvider(map[string]string{"local": root})

	items, err := provider.List(context.Background(), "local", "", "name", "")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("根目录应有 3 个条目，实际 %d: %+v", len(items), items)
	}
	// sortKey=name 走自然排序：目录在前还是文件在前不保证，但顺序必须稳定且可预测。
	if items[0].Name != "a.txt" {
		t.Fatalf("按名称排序后第一项应为 a.txt，实际 %q", items[0].Name)
	}
	byName := make(map[string]Item, len(items))
	for _, item := range items {
		byName[item.Name] = item
		if item.Storage != "local" {
			t.Fatalf("条目应带存储名: %+v", item)
		}
		if item.ModifyTime == nil || *item.ModifyTime <= 0 {
			t.Fatalf("条目应带修改时间: %+v", item)
		}
		if item.Children == nil {
			t.Fatalf("Children 必须是空切片而不是 nil（避免序列化成 null）: %+v", item)
		}
	}
	if dir := byName["电影"]; dir.Type != "dir" || dir.Size != nil {
		t.Fatalf("目录条目类型/体积不对: %+v", dir)
	}
	if file := byName["b.mkv"]; file.Type != "file" || file.Extension != "mkv" || file.Size == nil || *file.Size != 1 {
		t.Fatalf("文件条目投影不对: %+v", file)
	}
	if file := byName["b.mkv"]; file.Path != "b.mkv" || file.Basename != "b" || file.FileID != "b.mkv" {
		t.Fatalf("文件条目路径/基名不对: %+v", file)
	}
}

func TestLocalProviderListSortsByModifyTimeByDefault(t *testing.T) {
	root := seedStorageTree(t)
	provider := NewLocalProvider(map[string]string{"local": root})

	items, err := provider.List(context.Background(), "local", "", "", "")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	// 默认按修改时间倒序：b.mkv（-1h）应排在 a.txt（-2h）之前。
	position := make(map[string]int, len(items))
	for index, item := range items {
		position[item.Name] = index
	}
	if position["b.mkv"] > position["a.txt"] {
		t.Fatalf("应按修改时间倒序，实际顺序 %+v", items)
	}
}

func TestLocalProviderListFiltersByGlobKeyword(t *testing.T) {
	root := seedStorageTree(t)
	provider := NewLocalProvider(map[string]string{"local": root})

	items, err := provider.List(context.Background(), "local", "电影", "name", "*.MKV")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("关键字 *.MKV 应匹配 2 个文件（大小写不敏感），实际 %d: %+v", len(items), items)
	}
	for _, item := range items {
		if !strings.HasSuffix(item.Name, ".mkv") {
			t.Fatalf("不应匹配 %q", item.Name)
		}
		if item.Path != "电影/"+item.Name {
			t.Fatalf("子目录条目的 Path 应带父路径: %+v", item)
		}
	}
}

func TestLocalProviderListInputValidation(t *testing.T) {
	root := seedStorageTree(t)
	provider := NewLocalProvider(map[string]string{"local": root})

	if _, err := provider.List(nil, "local", "", "", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil context 应返回 ErrInvalidRequest，实际 %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.List(cancelled, "local", "", "", ""); err == nil {
		t.Fatal("已取消的 context 应返回错误")
	}
	if _, err := provider.List(context.Background(), "local", "../etc", "", ""); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("越界路径应返回 ErrPathRejected，实际 %v", err)
	}
	if _, err := provider.List(context.Background(), "local", "不存在", "", ""); !errors.Is(err, ErrOperationFailed) {
		t.Fatalf("不存在的目录应返回 ErrOperationFailed，实际 %v", err)
	}
}

func TestLocalProviderMkdir(t *testing.T) {
	root := t.TempDir()
	provider := NewLocalProvider(map[string]string{"local": root})

	ok, err := provider.Mkdir(context.Background(), "local", "a", "b")
	if err != nil || !ok {
		t.Fatalf("创建目录失败: %v %v", ok, err)
	}
	if info, err := os.Stat(filepath.Join(root, "a", "b")); err != nil || !info.IsDir() {
		t.Fatalf("目录未创建: %v", err)
	}

	for _, name := range []string{"", "   ", "a/b", `a\b`} {
		if ok, err := provider.Mkdir(context.Background(), "local", "", name); !errors.Is(err, ErrInvalidRequest) || ok {
			t.Fatalf("非法目录名 %q 应被拒: %v %v", name, ok, err)
		}
	}
	if _, err := provider.Mkdir(context.Background(), "local", "../out", "b"); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("越界父路径应被拒，实际 %v", err)
	}
}

func TestLocalProviderDelete(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	provider := NewLocalProvider(map[string]string{"local": root})

	ok, err := provider.Delete(context.Background(), "local", "a")
	if err != nil || !ok {
		t.Fatalf("删除失败: %v %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatalf("目录应已被删除: %v", err)
	}

	if ok, err := provider.Delete(context.Background(), "local", ""); !errors.Is(err, ErrInvalidRequest) || ok {
		t.Fatalf("空路径应被拒: %v %v", ok, err)
	}
	// 删除根目录是明确禁止的操作，必须拒绝而不是把整个存储清空。
	if ok, err := provider.Delete(context.Background(), "local", "."); !errors.Is(err, ErrPathRejected) || ok {
		t.Fatalf("删除根目录应被拒: %v %v", ok, err)
	}
	if ok, err := provider.Delete(context.Background(), "local", "../x"); !errors.Is(err, ErrPathRejected) || ok {
		t.Fatalf("越界路径应被拒: %v %v", ok, err)
	}
	if ok, err := provider.Delete(context.Background(), "missing", "a"); !errors.Is(err, ErrUnsupportedStorage) || ok {
		t.Fatalf("未注册存储应被拒: %v %v", ok, err)
	}
}

func TestLocalProviderRename(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	provider := NewLocalProvider(map[string]string{"local": root})

	ok, err := provider.Rename(context.Background(), "local", "a.txt", "b.txt", false)
	if err != nil || !ok {
		t.Fatalf("重命名失败: %v %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Fatalf("新名字应存在: %v", err)
	}

	for _, name := range []string{"", "x/y", `x\y`} {
		if ok, err := provider.Rename(context.Background(), "local", "b.txt", name, false); !errors.Is(err, ErrInvalidRequest) || ok {
			t.Fatalf("非法新名 %q 应被拒: %v %v", name, ok, err)
		}
	}
	if ok, err := provider.Rename(context.Background(), "local", ".", "x", false); !errors.Is(err, ErrPathRejected) || ok {
		t.Fatalf("重命名根目录应被拒: %v %v", ok, err)
	}
}

func TestLocalProviderUsageAndTransType(t *testing.T) {
	root := t.TempDir()
	provider := NewLocalProvider(map[string]string{"local": root})

	usage, err := provider.Usage(context.Background(), "local")
	if err != nil {
		t.Fatalf("Usage 失败: %v", err)
	}
	// 有些文件系统取不到容量，此时返回零值也算成功；但绝不能是负数。
	if usage.Total < 0 || usage.Available < 0 {
		t.Fatalf("容量不应为负: %+v", usage)
	}

	transType, err := provider.TransType(context.Background(), "local")
	if err != nil {
		t.Fatalf("TransType 失败: %v", err)
	}
	if transType == nil {
		t.Fatal("本地存储的整理方式应为空对象而不是 nil")
	}

	if _, err := provider.Usage(context.Background(), "missing"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未注册存储的 Usage 应报错，实际 %v", err)
	}
	if _, err := provider.TransType(context.Background(), "missing"); !errors.Is(err, ErrUnsupportedStorage) {
		t.Fatalf("未注册存储的 TransType 应报错，实际 %v", err)
	}
}
