package httpapi

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moviepilot-go/moviepilot-go/internal/config"
)

// urlEscape 用于把图片地址放进查询参数。
func urlEscape(raw string) string { return url.QueryEscape(raw) }

// pngFixture 是最小 PNG 头，用于让 http.DetectContentType 识别成图片。
var pngFixture = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R'}

// publicImageURL 用公网 IP 字面量绕开 DNS，同时能通过 SSRF 检查；
// 命中缓存时不会真的发起网络请求。
const publicImageURL = "http://8.8.8.8/poster.png"

func TestImageProxyRejectsInvalidAndPrivateTargets(t *testing.T) {
	deps := Dependencies{}
	invalid := []string{"", "  ", "ftp://example.com/a.png", "http://", "not-a-url", "file:///etc/passwd"}
	for _, raw := range invalid {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/system/img/1?imgurl="+strings.ReplaceAll(raw, " ", "%20"), nil)
		deps.systemImageProxy(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "图片地址不合法") {
			t.Fatalf("%q = (%d, %s)", raw, recorder.Code, recorder.Body.String())
		}
	}

	private := []string{
		"http://localhost/a.png",
		"http://sub.localhost/a.png",
		"http://127.0.0.1/a.png",
		"http://10.1.2.3/a.png",
		"http://192.168.1.1/a.png",
		"http://172.16.0.1/a.png",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/a.png",
		"http://0.0.0.0/a.png",
	}
	for _, raw := range private {
		recorder := httptest.NewRecorder()
		deps.systemImageProxy(recorder, httptest.NewRequest(http.MethodGet, "/system/img/1?imgurl="+urlEscape(raw), nil))
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "图片地址不允许访问") {
			t.Fatalf("%q 未被拒绝: (%d, %s)", raw, recorder.Code, recorder.Body.String())
		}
	}
}

func TestImageProxyServesCacheAndHonoursETag(t *testing.T) {
	configDir := t.TempDir()
	deps := Dependencies{Config: config.Config{ConfigDir: configDir}}

	path, ok := deps.imageCachePath(publicImageURL)
	if !ok {
		t.Fatal("配置 ConfigDir 后应返回缓存路径")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建缓存目录失败: %v", err)
	}
	if err := os.WriteFile(path, pngFixture, 0o644); err != nil {
		t.Fatalf("写入缓存失败: %v", err)
	}

	etag := imageProxyETag(publicImageURL)
	recorder := httptest.NewRecorder()
	deps.systemImageProxy(recorder, httptest.NewRequest(http.MethodGet,
		"/system/img/1?imgurl="+urlEscape(publicImageURL), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("缓存命中 status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), pngFixture) {
		t.Fatalf("缓存内容 = %v", recorder.Body.Bytes())
	}
	if recorder.Header().Get("ETag") != etag || recorder.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("响应头 = %v", recorder.Header())
	}
	if !strings.HasPrefix(recorder.Header().Get("Content-Type"), "image/") {
		t.Fatalf("Content-Type = %q", recorder.Header().Get("Content-Type"))
	}

	// If-None-Match 命中 ETag 时直接 304，不读缓存也不回源。
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/system/img/1?imgurl="+urlEscape(publicImageURL), nil)
	request.Header.Set("If-None-Match", "W/"+etag)
	deps.systemImageProxy(recorder, request)
	if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 {
		t.Fatalf("304 = (%d, %q)", recorder.Code, recorder.Body.String())
	}

	// systemCacheImage 用 url 参数走同一套缓存逻辑。
	recorder = httptest.NewRecorder()
	deps.systemCacheImage(recorder, httptest.NewRequest(http.MethodGet, "/system/cache/image?url="+urlEscape(publicImageURL), nil))
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), pngFixture) {
		t.Fatalf("systemCacheImage = (%d, %d bytes)", recorder.Code, recorder.Body.Len())
	}
}

func TestImageProxyHelpers(t *testing.T) {
	if _, ok := (Dependencies{}).imageCachePath(publicImageURL); ok {
		t.Fatal("没有 ConfigDir 时不应返回缓存路径")
	}

	dir := t.TempDir()
	deps := Dependencies{Config: config.Config{ConfigDir: dir}}
	first, _ := deps.imageCachePath("https://a.example/1.png")
	second, _ := deps.imageCachePath("https://a.example/2.png")
	if first == second {
		t.Fatalf("不同 URL 复用同一缓存文件: %s", first)
	}
	if !strings.HasPrefix(first, filepath.Join(dir, "cache", "images")) {
		t.Fatalf("缓存路径 = %s", first)
	}

	etag := imageProxyETag(publicImageURL)
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) != 34 {
		t.Fatalf("ETag 格式 = %s", etag)
	}
	if imageProxyETag(publicImageURL) != etag {
		t.Fatal("ETag 不稳定")
	}

	// writeImageResponse 在没有预设 Content-Type 时按内容嗅探。
	recorder := httptest.NewRecorder()
	writeImageResponse(recorder, pngFixture, etag)
	if recorder.Code != http.StatusOK || recorder.Header().Get("ETag") != etag ||
		recorder.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("writeImageResponse = (%d, %v)", recorder.Code, recorder.Header())
	}

	// 预设 Content-Type 时保留调用方设置（fetchImage 返回的原始类型）。
	recorder = httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "image/webp")
	writeImageResponse(recorder, pngFixture, etag)
	if recorder.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("预设 Content-Type 被覆盖: %v", recorder.Header())
	}
}

func TestEnsurePublicIPMatrix(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "192.168.0.1", "172.16.5.5", "169.254.1.1", "0.0.0.0", "::1", "fe80::1", "224.0.0.1"}
	for _, raw := range blocked {
		if err := ensurePublicIP(net.ParseIP(raw)); err == nil {
			t.Fatalf("%s 应被拒绝", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"} {
		if err := ensurePublicIP(net.ParseIP(raw)); err != nil {
			t.Fatalf("%s 应被允许: %v", raw, err)
		}
	}

	// 主机名层面：空/回环直接拒绝。
	for _, host := range []string{"", "localhost", "foo.localhost"} {
		if err := ensurePublicImageHost(host); err == nil {
			t.Fatalf("%q 应被拒绝", host)
		}
	}
	if err := ensurePublicImageHost("127.0.0.1"); err == nil {
		t.Fatal("回环 IP 应被拒绝")
	}
}

func TestFetchImageBranches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngFixture)
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("not an image"))
		case "/empty":
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		case "/no-header":
			// 没有 Content-Type 时按内容嗅探，不因缺失头报错。
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	request := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/", nil)
	}

	// 正常图片：返回内容与声明的 Content-Type。
	content, contentType, err := fetchImage(request(), server.URL+"/ok")
	if err != nil || !bytes.Equal(content, pngFixture) || contentType != "image/png" {
		t.Fatalf("ok: (%d bytes, %q, %v)", len(content), contentType, err)
	}

	// 非图片、空内容、非 200 都要报错。
	for _, path := range []string{"/text", "/empty", "/broken"} {
		if _, _, err := fetchImage(request(), server.URL+path); err == nil {
			t.Fatalf("%s 应报错", path)
		}
	}

	// 上游不可达。
	if _, _, err := fetchImage(request(), "http://127.0.0.1:1/x"); err == nil {
		t.Fatal("连接失败应报错")
	}

	// 非法 URL 在构造请求时失败。
	if _, _, err := fetchImage(request(), "http://[::1"); err == nil {
		t.Fatal("非法 URL 应报错")
	}
}

// 豆瓣图床校验 Referer，缺了回 418；这里锁住 V2 ImageHelper 的三条请求头，
// 否则海报、演员头像会以「代理 502」的形式整片消失。
func TestImageProxyHeadersMatchV2ImageHelper(t *testing.T) {
	douban := imageProxyHeaders("https://img3.doubanio.com/view/photo/m_ratio_poster/public/p1.webp")
	if got := douban.Get("Referer"); got != "https://movie.douban.com/" {
		t.Fatalf("豆瓣图 Referer = %q", got)
	}
	if got := douban.Get("User-Agent"); !strings.HasPrefix(got, "Mozilla/5.0") {
		t.Fatalf("User-Agent = %q，应为浏览器 UA", got)
	}
	if got := douban.Get("Accept"); !strings.Contains(got, "image/webp") {
		t.Fatalf("Accept = %q", got)
	}

	// TMDB 等其他图床不带 Referer，跟 V2 的 `None` 一致。
	other := imageProxyHeaders("https://image.tmdb.org/t/p/w500/abc.jpg")
	if got := other.Get("Referer"); got != "" {
		t.Fatalf("非豆瓣图不该带 Referer，实际 %q", got)
	}
	if other.Get("User-Agent") == "" || other.Get("Accept") == "" {
		t.Fatal("其他图床也要带 UA 和 Accept")
	}
}
