package scraper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func newImageServer(t *testing.T, payload []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		if payload == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func newTestImageWriter(server *httptest.Server, overwrite bool) *ImageWriter {
	return &ImageWriter{
		client:       server.Client(),
		validatePath: func(string) error { return nil },
		Overwrite:    overwrite,
	}
}

func TestImageExtensionFallsBackToJPG(t *testing.T) {
	cases := map[string]string{
		"https://img.example/a.jpg":     "jpg",
		"https://img.example/a.JPEG":    "jpeg",
		"https://img.example/a.png?x=1": "png",
		"https://img.example/a.WEBP":    "webp",
		"https://img.example/a.gif":     "jpg",
		"https://img.example/noext":     "jpg",
		"https://img.example/a.svg":     "jpg",
		"://bad url":                    "jpg",
	}
	for source, want := range cases {
		if got := imageExtension(source); got != want {
			t.Fatalf("imageExtension(%q) = %q，期望 %q", source, got, want)
		}
	}
}

func TestCanonicalImageType(t *testing.T) {
	cases := map[string]string{
		"poster":     "poster",
		"  POSTER  ": "poster",
		"fanart":     "backdrop",
		"background": "backdrop",
		"cdart":      "disc",
		"logo":       "logo",
		"thumb":      "thumb",
	}
	for input, want := range cases {
		got, err := CanonicalImageType(input)
		if err != nil {
			t.Fatalf("CanonicalImageType(%q) 报错: %v", input, err)
		}
		if got != want {
			t.Fatalf("CanonicalImageType(%q) = %q，期望 %q", input, got, want)
		}
	}
	for _, bad := range []string{"", "wallpaper", "poster2"} {
		if _, err := CanonicalImageType(bad); err == nil {
			t.Fatalf("%q 应被判为不支持的图片类型", bad)
		}
	}
}

func TestWriteMediaImageWritesAliases(t *testing.T) {
	payload := []byte("image-bytes")
	server, hits := newImageServer(t, payload)
	root := t.TempDir()
	media := filepath.Join(root, "电影", "a.mkv")
	writer := newTestImageWriter(server, false)

	target, err := writer.WriteMediaImage(context.Background(), media, "backdrop", server.URL+"/b.jpg")
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 返回值是规范名，别名只是附带的兼容文件。
	if target != filepath.Join(root, "电影", "backdrop.jpg") {
		t.Fatalf("target = %q", target)
	}
	for _, name := range []string{"backdrop.jpg", "fanart.jpg"} {
		content, readErr := os.ReadFile(filepath.Join(root, "电影", name))
		if readErr != nil {
			t.Fatalf("缺少 %s: %v", name, readErr)
		}
		if string(content) != string(payload) {
			t.Fatalf("%s 内容不匹配: %q", name, content)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("同一图片应只下载一次，实际 %d 次", got)
	}
}

func TestWriteMediaImageIgnoresEmptySource(t *testing.T) {
	server, hits := newImageServer(t, []byte("x"))
	root := t.TempDir()
	writer := newTestImageWriter(server, false)

	target, err := writer.WriteMediaImage(context.Background(), filepath.Join(root, "a.mkv"), "poster", "   ")
	if err != nil || target != "" {
		t.Fatalf("空图片地址应返回空路径且不报错: %q %v", target, err)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("空图片地址不应发起请求")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("不应产生任何文件: %v", entries)
	}
}

func TestWriteMediaImageGuards(t *testing.T) {
	server, _ := newImageServer(t, []byte("x"))
	writer := newTestImageWriter(server, false)
	media := filepath.Join(t.TempDir(), "a.mkv")

	if _, err := writer.WriteMediaImage(nil, media, "poster", server.URL+"/p.jpg"); err == nil {
		t.Fatal("nil 上下文应报错")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := writer.WriteMediaImage(cancelled, media, "poster", server.URL+"/p.jpg"); err == nil {
		t.Fatal("已取消的上下文应报错")
	}
	var unconfigured *ImageWriter
	if _, err := unconfigured.WriteMediaImage(context.Background(), media, "poster", server.URL+"/p.jpg"); err == nil {
		t.Fatal("未配置写入器应报错")
	}
	if _, err := writer.WriteMediaImage(context.Background(), media, "wallpaper", server.URL+"/p.jpg"); err == nil {
		t.Fatal("未知图片类型应报错")
	}
}

func TestWriteTargetsSkipsExistingUnlessOverwrite(t *testing.T) {
	payload := []byte("new-bytes")
	server, hits := newImageServer(t, payload)
	root := t.TempDir()
	media := filepath.Join(root, "a.mkv")
	target := filepath.Join(root, "poster.jpg")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("准备已存在图片失败: %v", err)
	}

	// 默认不覆盖：已有文件保留，且不下载。
	writer := newTestImageWriter(server, false)
	if err := writer.writeTargets(context.Background(), media, server.URL+"/p.jpg", []string{target}); err != nil {
		t.Fatalf("跳过已有文件不应报错: %v", err)
	}
	content, _ := os.ReadFile(target)
	if string(content) != "old" {
		t.Fatalf("不应覆盖已有图片，实际 %q", content)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("全部目标已存在时不应下载")
	}

	// 允许覆盖时必须替换。
	writer = newTestImageWriter(server, true)
	if err := writer.writeTargets(context.Background(), media, server.URL+"/p.jpg", []string{target}); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	content, _ = os.ReadFile(target)
	if string(content) != string(payload) {
		t.Fatalf("覆盖后内容 = %q", content)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("应下载一次，实际 %d", atomic.LoadInt32(hits))
	}
}

func TestWriteTargetsRejectsDirectoryTarget(t *testing.T) {
	server, _ := newImageServer(t, []byte("x"))
	root := t.TempDir()
	target := filepath.Join(root, "poster.jpg")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	writer := newTestImageWriter(server, false)
	err := writer.writeTargets(context.Background(), filepath.Join(root, "a.mkv"), server.URL+"/p.jpg", []string{target})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("目标是目录时必须报错，实际 %v", err)
	}
}

func TestWriteTargetsValidatesPaths(t *testing.T) {
	server, _ := newImageServer(t, []byte("x"))
	root := t.TempDir()
	writer := newTestImageWriter(server, false)
	writer.validatePath = func(string) error { return os.ErrPermission }

	err := writer.writeTargets(context.Background(), filepath.Join(root, "a.mkv"), server.URL+"/p.jpg", []string{filepath.Join(root, "poster.jpg")})
	if err == nil || !strings.Contains(err.Error(), "invalid image path") {
		t.Fatalf("路径校验失败时必须报错，实际 %v", err)
	}
}

func TestDownloadErrorPaths(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "a.mkv")

	notFound, _ := newImageServer(t, nil)
	writer := newTestImageWriter(notFound, false)
	if err := writer.writeTargets(context.Background(), media, notFound.URL+"/p.jpg", []string{filepath.Join(root, "poster.jpg")}); err == nil {
		t.Fatal("404 应报错")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer empty.Close()
	writer = newTestImageWriter(empty, false)
	if err := writer.writeTargets(context.Background(), media, empty.URL+"/p.jpg", []string{filepath.Join(root, "poster.jpg")}); err == nil {
		t.Fatal("空响应体应判为非法载荷")
	}
}

func TestWriteEpisodeImage(t *testing.T) {
	server, _ := newImageServer(t, []byte("thumb"))
	root := t.TempDir()
	media := filepath.Join(root, "S01E01.mkv")
	writer := newTestImageWriter(server, false)

	target, err := writer.WriteEpisodeImage(context.Background(), media, server.URL+"/e.png")
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if target != filepath.Join(root, "S01E01.png") {
		t.Fatalf("target = %q", target)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "thumb" {
		t.Fatalf("截图内容不对: %q %v", content, err)
	}

	// 空地址是同名文件不存在的合法情形。
	if target, err := writer.WriteEpisodeImage(context.Background(), media, ""); err != nil || target != "" {
		t.Fatalf("空地址应返回空路径: %q %v", target, err)
	}
}

func TestWriteSeasonImageTargets(t *testing.T) {
	server, _ := newImageServer(t, []byte("season"))
	root := t.TempDir()
	seasonDir := filepath.Join(root, "Show", "Season 2")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatalf("准备季目录失败: %v", err)
	}
	media := filepath.Join(seasonDir, "S02E01.mkv")
	writer := newTestImageWriter(server, false)

	targets, err := writer.WriteSeasonImage(context.Background(), media, 2, server.URL+"/s.jpg")
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 季目录内 poster + 剧集根目录 seasonXX-poster，两处都要有。
	if len(targets) != 2 {
		t.Fatalf("targets = %v", targets)
	}
	want := map[string]bool{
		filepath.Join(root, "Show", "season02-poster.jpg"): true,
		filepath.Join(seasonDir, "poster.jpg"):             true,
	}
	for _, target := range targets {
		if !want[target] {
			t.Fatalf("多余的季海报目标: %q", target)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("目标未写入: %q (%v)", target, err)
		}
	}

	// 特别篇使用固定名；这里用不在季目录下的媒体文件，避免触发季号一致性校验。
	specialsMedia := filepath.Join(root, "Show", "EP00.mkv")
	targets, err = writer.WriteSeasonImage(context.Background(), specialsMedia, 0, server.URL+"/s.jpg")
	if err != nil {
		t.Fatalf("特别篇写入失败: %v", err)
	}
	if len(targets) != 1 || !strings.Contains(targets[0], "season-specials-poster") {
		t.Fatalf("特别篇目标 = %v", targets)
	}
}

func TestWriteSeasonImageValidation(t *testing.T) {
	server, hits := newImageServer(t, []byte("x"))
	root := t.TempDir()
	seasonDir := filepath.Join(root, "Season 3")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	media := filepath.Join(seasonDir, "E01.mkv")
	writer := newTestImageWriter(server, false)

	if _, err := writer.WriteSeasonImage(context.Background(), media, -1, server.URL+"/s.jpg"); err == nil {
		t.Fatal("负数季号应报错")
	}
	if _, err := writer.WriteSeasonImage(context.Background(), media, 1000, server.URL+"/s.jpg"); err == nil {
		t.Fatal("超过 999 的季号应报错")
	}
	// 目录写着 Season 3，却要求写第 4 季：拒绝，避免把海报放进错的季。
	if _, err := writer.WriteSeasonImage(context.Background(), media, 4, server.URL+"/s.jpg"); err == nil {
		t.Fatal("季号与目录不一致时应报错")
	}
	if targets, err := writer.WriteSeasonImage(context.Background(), media, 3, "  "); err != nil || targets != nil {
		t.Fatalf("空地址应返回 nil: %v %v", targets, err)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatal("校验失败路径不应下载")
	}

	// 没有季目录时只写剧集根目录。
	media = filepath.Join(root, "Show", "E01.mkv")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	targets, err := writer.WriteSeasonImage(context.Background(), media, 1, server.URL+"/s.jpg")
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if len(targets) != 1 || filepath.Base(targets[0]) != "season01-poster.jpg" {
		t.Fatalf("无季目录时目标 = %v", targets)
	}
}

func TestSeasonDirectoryNumberParsing(t *testing.T) {
	cases := map[string]struct {
		number int
		ok     bool
	}{
		"Season 1":       {1, true},
		"season2":        {2, true},
		"s03":            {3, true},
		"S 4":            {4, true},
		"Season_05":      {5, true},
		"第6季":            {6, true},
		"Special":        {0, true},
		"specials":       {0, true},
		"  Season 7  ":   {7, true},
		"Movies":         {0, false},
		"Season":         {0, false},
		"Season 1 extra": {0, false},
		"第X季":            {0, false},
		"即使999":          {0, false},
	}
	for name, want := range cases {
		number, ok := seasonDirectoryNumber(name)
		if ok != want.ok || (ok && number != want.number) {
			t.Fatalf("seasonDirectoryNumber(%q) = %d,%v，期望 %d,%v", name, number, ok, want.number, want.ok)
		}
	}
}

func TestWriteImageAtomicPreservesPermissions(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "poster.jpg")
	if err := writeImageAtomic(target, []byte("data")); err != nil {
		t.Fatalf("原子写入失败: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("目标不存在: %v", err)
	}
	// Windows 的文件权限只有「只读」一位：Go 把可写文件报告成 0666，
	// 属主/组/其它三段在那里无从表达，0644 是 UNIX 语义。
	wantPerm := os.FileMode(0o644)
	if runtime.GOOS == "windows" {
		wantPerm = 0o666
	}
	if info.Mode().Perm() != wantPerm {
		t.Fatalf("权限 = %v，期望 %v", info.Mode().Perm(), wantPerm)
	}
	// 目录里不应残留临时文件。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "poster.jpg" {
		t.Fatalf("残留临时文件: %v", entries)
	}
}

func TestWriteImageAtomicNoReplaceSemantics(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "poster.jpg")
	if err := writeImageAtomicNoReplace(target, []byte("first")); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	// 第二次写入必须静默保留原内容（并发补齐季海报时不能让后到者覆盖）。
	if err := writeImageAtomicNoReplace(target, []byte("second")); err != nil {
		t.Fatalf("重复写入不应报错: %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "first" {
		t.Fatalf("内容 = %q，期望保留 first", content)
	}

	// 目标是目录时明确报错。
	dirTarget := filepath.Join(root, "poster-dir.jpg")
	if err := os.MkdirAll(dirTarget, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	err = writeImageAtomicNoReplace(dirTarget, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("目录目标应报错，实际 %v", err)
	}
}
