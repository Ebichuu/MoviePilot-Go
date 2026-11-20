package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaticHandler 覆盖前端静态资源兜底：方法限制、路径穿越拒绝、
// 资源缓存头、目录与缺失路径回落到 index.html，以及缺 index.html 时的 404。
func TestStaticHandler(t *testing.T) {
	dir := t.TempDir()
	writeStaticFile(t, dir, "index.html", "<html>index</html>")
	writeStaticFile(t, dir, "assets/app-1.js", "console.log(1)")
	writeStaticFile(t, dir, "data.json", "{}")
	handler := staticHandler(dir)

	request := func(method, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
		return recorder
	}

	if got := request(http.MethodPost, "/index.html").Code; got != http.StatusNotFound {
		t.Fatalf("POST 状态码 = %d, want 404", got)
	}
	if got := request(http.MethodHead, "/assets/app-1.js").Code; got != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d, want 200", got)
	}

	bundle := request(http.MethodGet, "/assets/app-1.js")
	if bundle.Code != http.StatusOK {
		t.Fatalf("静态资源状态码 = %d", bundle.Code)
	}
	if contentType := bundle.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Fatalf("静态资源 Content-Type = %q", contentType)
	}
	if cache := bundle.Header().Get("Cache-Control"); cache != "public, max-age=31536000, immutable" {
		t.Fatalf("静态资源 Cache-Control = %q", cache)
	}

	data := request(http.MethodGet, "/data.json")
	if data.Code != http.StatusOK || !strings.Contains(data.Header().Get("Content-Type"), "json") {
		t.Fatalf("JSON 资源 = (%d, %q)", data.Code, data.Header().Get("Content-Type"))
	}

	index := request(http.MethodGet, "/")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "index") {
		t.Fatalf("根路径 = (%d, %q)", index.Code, index.Body.String())
	}
	if cache := index.Header().Get("Cache-Control"); cache != "no-cache" {
		t.Fatalf("index Cache-Control = %q", cache)
	}

	fallback := request(http.MethodGet, "/some/deep/route")
	if fallback.Code != http.StatusOK || !strings.Contains(fallback.Body.String(), "index") {
		t.Fatalf("前端路由回落 = (%d, %q)", fallback.Code, fallback.Body.String())
	}
	if cache := fallback.Header().Get("Cache-Control"); cache != "no-cache" {
		t.Fatalf("前端路由 Cache-Control = %q", cache)
	}

	traversal := httptest.NewRecorder()
	traversalRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	traversalRequest.URL.Path = "/../secret.txt"
	handler.ServeHTTP(traversal, traversalRequest)
	if traversal.Code != http.StatusNotFound {
		t.Fatalf("路径穿越状态码 = %d, want 404", traversal.Code)
	}

	empty := staticHandler(t.TempDir())
	missing := httptest.NewRecorder()
	empty.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("缺少 index.html 状态码 = %d, want 404", missing.Code)
	}
}

func writeStaticFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("创建静态资源目录: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("写入静态资源 %s: %v", name, err)
	}
}
