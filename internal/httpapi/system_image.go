package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	imageProxyMaxBytes   = 10 << 20
	imageProxyTimeout    = 15 * time.Second
	imageProxyMaxAgeSpan = 24 * time.Hour

	// imageProxyUserAgent 对齐 V2 的 `settings.NORMAL_USER_AGENT`：图床认浏览器 UA，
	// 自定义标识（原来的 MoviePilot-Go-ImageProxy/1.0）会被豆瓣直接回 418。
	imageProxyUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// systemImageProxy 对应 V2 `GET /system/img/{proxy}?imgurl=...`：由后端代理并缓存图片，
// 避免前端直连图床被防盗链或跨域策略拦掉。`proxy` 路径参数与 `proxy` 查询参数在 V2 里
// 表示是否走网络代理，本实现没有出站代理配置，因此按直连处理。
func (d Dependencies) systemImageProxy(w http.ResponseWriter, r *http.Request) {
	imageURL := strings.TrimSpace(r.URL.Query().Get("imgurl"))
	useCache := true
	if raw := strings.TrimSpace(r.URL.Query().Get("cache")); raw != "" {
		useCache = raw == "1" || strings.EqualFold(raw, "true")
	}
	d.serveProxiedImage(w, r, imageURL, useCache)
}

// systemCacheImage 对应 V2 `GET /system/cache/image?url=...`：只走本地缓存策略。
func (d Dependencies) systemCacheImage(w http.ResponseWriter, r *http.Request) {
	imageURL := strings.TrimSpace(r.URL.Query().Get("url"))
	d.serveProxiedImage(w, r, imageURL, true)
}

func (d Dependencies) serveProxiedImage(w http.ResponseWriter, r *http.Request, rawURL string, useCache bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "图片地址不合法")
		return
	}
	if err := ensurePublicImageHost(parsed.Hostname()); err != nil {
		writeError(w, http.StatusForbidden, "图片地址不允许访问")
		return
	}

	etag := imageProxyETag(rawURL)
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" &&
		strings.TrimPrefix(match, "W/") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	if useCache {
		if path, ok := d.imageCachePath(rawURL); ok {
			if content, err := os.ReadFile(path); err == nil && len(content) > 0 {
				writeImageResponse(w, content, etag)
				return
			}
		}
	}

	content, contentType, err := fetchImage(r, rawURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "图片下载失败")
		return
	}
	if useCache {
		if path, ok := d.imageCachePath(rawURL); ok {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				_ = os.WriteFile(path, content, 0o644)
			}
		}
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	writeImageResponse(w, content, etag)
}

func writeImageResponse(w http.ResponseWriter, content []byte, etag string) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", http.DetectContentType(content))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// imageProxyHeaders 复刻 V2 `ImageHelper._get_request_params` 的请求头：
//
//	ua      = settings.NORMAL_USER_AGENT
//	accept  = image/avif,image/webp,image/apng,image/*,*/*;q=0.8
//	referer = doubanio.com 固定 https://movie.douban.com/
//
// 豆瓣图床校验 Referer，缺了直接回 418，代理再把它翻成 502——
// 前端看到的就是「海报和演员头像全都不显示」。
func imageProxyHeaders(rawURL string) http.Header {
	headers := http.Header{}
	headers.Set("User-Agent", imageProxyUserAgent)
	headers.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	if parsed, err := url.Parse(rawURL); err == nil &&
		strings.Contains(strings.ToLower(parsed.Host), "doubanio.com") {
		headers.Set("Referer", "https://movie.douban.com/")
	}
	return headers
}

func fetchImage(r *http.Request, rawURL string) ([]byte, string, error) {
	ctx := r.Context()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header = imageProxyHeaders(rawURL)
	client := &http.Client{Timeout: imageProxyTimeout}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, "", errors.New("图片源返回非 200")
	}
	contentType := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Type")))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return nil, "", errors.New("响应不是图片")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, imageProxyMaxBytes))
	if err != nil {
		return nil, "", err
	}
	if len(content) == 0 {
		return nil, "", errors.New("图片内容为空")
	}
	return content, contentType, nil
}

// ensurePublicImageHost 拒绝回环、内网与链路本地地址，避免图片代理变成 SSRF 跳板。
func ensurePublicImageHost(host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("回环地址")
	}
	if ip := net.ParseIP(host); ip != nil {
		return ensurePublicIP(ip)
	}
	addresses, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if err := ensurePublicIP(address); err != nil {
			return err
		}
	}
	return nil
}

func ensurePublicIP(ip net.IP) error {
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsUnspecified(), ip.IsMulticast():
		return errors.New("非公网地址")
	}
	return nil
}

func imageProxyETag(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// imageCachePath 返回缓存文件的绝对路径；没有配置 CONFIG_DIR 时不落盘。
func (d Dependencies) imageCachePath(rawURL string) (string, bool) {
	dir := strings.TrimSpace(d.Config.ConfigDir)
	if dir == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(rawURL))
	name := hex.EncodeToString(sum[:]) + ".img"
	return filepath.Join(dir, "cache", "images", name), true
}
