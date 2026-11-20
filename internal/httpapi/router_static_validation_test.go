package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateSubscriptionRequest 覆盖订阅请求的全部校验理由。
func TestValidateSubscriptionRequest(t *testing.T) {
	season := func(value int) *int { return &value }
	valid := subscriptionRequest{Name: "某剧", Type: "电视剧", Sites: []int{1, 2}}
	if err := validateSubscriptionRequest(valid); err != nil {
		t.Fatalf("合法请求被拒: %v", err)
	}

	cases := []struct {
		input   subscriptionRequest
		message string
	}{
		{input: subscriptionRequest{Type: "电视剧"}, message: "订阅名称或类型无效"},
		{input: subscriptionRequest{Name: "某剧", Type: "动画"}, message: "订阅名称或类型无效"},
		{input: subscriptionRequest{Name: "某剧", Type: "电视剧", Season: season(-1)}, message: "订阅季号不能为负数"},
		{input: subscriptionRequest{Name: "某剧", Type: "电视剧", TotalEpisode: 5001}, message: "订阅集数超出有效范围"},
		{input: subscriptionRequest{Name: "某剧", Type: "电视剧", StartEpisode: -1}, message: "订阅集数超出有效范围"},
		{input: subscriptionRequest{Name: "某剧", Type: "电视剧", TMDBID: -1}, message: "订阅媒体 ID 无效"},
		{input: subscriptionRequest{Name: "某剧", Type: "电视剧", MediaSource: "themoviedb"},
			message: "订阅媒体来源和媒体 ID 必须同时提供"},
		{input: subscriptionRequest{Name: "某剧", Type: "电视剧", Sites: make([]int, 1025)},
			message: "订阅站点数量超出上限"},
		{input: subscriptionRequest{Name: "某剧", Type: "电视剧", Sites: []int{0}}, message: "订阅站点 ID 无效"},
	}
	for _, tc := range cases {
		if err := validateSubscriptionRequest(tc.input); err == nil || err.Error() != tc.message {
			t.Fatalf("%+v = %v，期望 %q", tc.input, err, tc.message)
		}
	}

	// 站点数量边界与季号 0 都算合法。
	if err := validateSubscriptionRequest(subscriptionRequest{Name: "某剧", Type: "电影", Season: season(0)}); err != nil {
		t.Fatalf("季号 0 应合法: %v", err)
	}
}

// TestNormalizeSubscriptionIdentity 覆盖媒体身份的归一顺序。
func TestSubscriptionIdentityNormalization(t *testing.T) {
	cases := []struct {
		name   string
		input  subscriptionRequest
		source string
		id     string
	}{
		{name: "显式来源优先", input: subscriptionRequest{MediaSource: "Bangumi", MediaID: " 42 "},
			source: "bangumi", id: "42"},
		{name: "TMDB", input: subscriptionRequest{TMDBID: 11}, source: "themoviedb", id: "11"},
		{name: "豆瓣", input: subscriptionRequest{DoubanID: " 1292052 "}, source: "douban", id: "1292052"},
		{name: "Bangumi", input: subscriptionRequest{BangumiID: 22}, source: "bangumi", id: "22"},
		{name: "AniList", input: subscriptionRequest{AniListID: 33}, source: "anilist", id: "33"},
		{name: "mediaid 键", input: subscriptionRequest{MediaKey: "tmdb:44"}, source: "themoviedb", id: "44"},
		{name: "无来源", input: subscriptionRequest{MediaKey: "44"}, source: "", id: ""},
		{name: "未知来源", input: subscriptionRequest{MediaKey: "unknown:44"}, source: "", id: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, id := normalizeSubscriptionIdentity(tc.input)
			if source != tc.source || id != tc.id {
				t.Fatalf("= (%q,%q)，期望 (%q,%q)", source, id, tc.source, tc.id)
			}
		})
	}
	if normalizeSubscriptionMediaSource(" TMDB ") != "themoviedb" {
		t.Fatal("tmdb 别名未归一")
	}
}

// TestStaticHandlerBranches 覆盖静态前端路由的缓存头、首页回落与目录穿越拦截。
func TestStaticHandlerBranches(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>index</html>"), 0o600); err != nil {
		t.Fatalf("写入首页: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("写入脚本: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatalf("创建子目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "index.html"), []byte("<html>sub</html>"), 0o600); err != nil {
		t.Fatalf("写入子首页: %v", err)
	}
	handler := staticHandler(root)

	cases := []struct {
		name        string
		method      string
		path        string
		code        int
		body        string
		cache       string
		contentType string
	}{
		{name: "首页不缓存", method: http.MethodGet, path: "/", code: http.StatusOK,
			body: "index", cache: "no-cache", contentType: "text/html"},
		{name: "静态资源长缓存", method: http.MethodGet, path: "/app.js", code: http.StatusOK,
			body: "console.log", cache: "immutable", contentType: "javascript"},
		{name: "目录回落到根首页", method: http.MethodGet, path: "/sub/", code: http.StatusOK, body: "index"},
		{name: "缺失文件回落到首页", method: http.MethodGet, path: "/missing", code: http.StatusOK, body: "index"},
		{name: "HEAD 允许", method: http.MethodHead, path: "/app.js", code: http.StatusOK},
		{name: "方法不允许", method: http.MethodPost, path: "/app.js", code: http.StatusNotFound},
		{name: "目录穿越被拒", method: http.MethodGet, path: "/../etc/passwd", code: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d", recorder.Code, tc.code)
			}
			if tc.body != "" && !strings.Contains(recorder.Body.String(), tc.body) {
				t.Fatalf("响应体 = %q，期望包含 %q", recorder.Body.String(), tc.body)
			}
			if tc.cache != "" && !strings.Contains(recorder.Header().Get("Cache-Control"), tc.cache) {
				t.Fatalf("Cache-Control = %q，期望包含 %q", recorder.Header().Get("Cache-Control"), tc.cache)
			}
			if tc.contentType != "" && !strings.Contains(recorder.Header().Get("Content-Type"), tc.contentType) {
				t.Fatalf("Content-Type = %q，期望包含 %q", recorder.Header().Get("Content-Type"), tc.contentType)
			}
		})
	}
}

// TestNotFoundHandlerBranches 覆盖未匹配路径的 API/前端分流。
func TestNotFoundHandlerBranches(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>index</html>"), 0o600); err != nil {
		t.Fatalf("写入首页: %v", err)
	}
	handler := notFoundHandler(root)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "接口不存在") {
		t.Fatalf("API 404 = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "index") {
		t.Fatalf("前端回落 = %d %s", recorder.Code, recorder.Body.String())
	}
}
