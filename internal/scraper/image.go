package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const maxImageBytes = 20 << 20

// imageTypeAliases 复刻 V2 `app/chain/media.py` 的图片类型别名。
var imageTypeAliases = map[string]string{
	"fanart":     "backdrop",
	"background": "backdrop",
	"cdart":      "disc",
}

// knownImageTypes 是 V2 媒体刮削支持的图片类型集合。
var knownImageTypes = map[string]struct{}{
	"poster": {}, "backdrop": {}, "logo": {}, "disc": {},
	"banner": {}, "thumb": {}, "clearart": {}, "landscape": {},
}

var allowedImageExtensions = map[string]string{
	".jpg": "jpg", ".jpeg": "jpeg", ".png": "png", ".webp": "webp",
}

// ImageWriter 按 V2 命名把媒体图片下载到媒体文件同目录。
type ImageWriter struct {
	client       *http.Client
	validatePath func(string) error
	// Overwrite 为真时覆盖已有图片；为假时保留已有文件。
	Overwrite bool
}

// NewImageWriter 创建图片写入器。
func NewImageWriter() *ImageWriter {
	return &ImageWriter{
		client:       &http.Client{Timeout: 60 * time.Second},
		validatePath: defaultPathValidator,
		Overwrite:    false,
	}
}

// CanonicalImageType 归一化 V2 图片类型别名；未知类型返回错误。
func CanonicalImageType(imageType string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(imageType))
	if alias, ok := imageTypeAliases[normalized]; ok {
		normalized = alias
	}
	if _, ok := knownImageTypes[normalized]; !ok {
		return "", fmt.Errorf("unsupported image type %q", imageType)
	}
	return normalized, nil
}

// WriteMediaImage 将 sourceURL 写入 mediaPath 同目录的 `<type>.<ext>`。
// sourceURL 为空表示没有图片可写，返回空路径且不产生副作用。
func (w *ImageWriter) WriteMediaImage(
	ctx context.Context,
	mediaPath string,
	imageType string,
	sourceURL string,
) (string, error) {
	if ctx == nil {
		return "", errors.New("context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if w == nil || w.client == nil {
		return "", errors.New("image writer is not configured")
	}
	canonical, err := CanonicalImageType(imageType)
	if err != nil {
		return "", err
	}
	sourceURL = strings.TrimSpace(sourceURL)
	if sourceURL == "" {
		return "", nil
	}
	extension := "." + imageExtension(sourceURL)
	targetPath := filepath.Join(filepath.Dir(mediaPath), canonical+extension)
	targets := []string{targetPath}
	// V2 同时生成媒体服务器兼容名称；季根目录带 season 前缀的图片不扩展别名。
	for _, alias := range mediaImageAliases[canonical] {
		targets = append(targets, filepath.Join(filepath.Dir(mediaPath), alias+extension))
	}
	if err := w.writeTargets(ctx, mediaPath, sourceURL, targets); err != nil {
		return "", err
	}
	return targetPath, nil
}

func (w *ImageWriter) download(ctx context.Context, sourceURL string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("image request invalid: %w", err)
	}
	response, err := w.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("image request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("image request status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("image request failed")
	}
	if len(body) == 0 || len(body) > maxImageBytes {
		return nil, errors.New("image payload invalid")
	}
	return body, nil
}

func imageExtension(sourceURL string) string {
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return "jpg"
	}
	extension := strings.ToLower(filepath.Ext(parsed.Path))
	if value, ok := allowedImageExtensions[extension]; ok {
		return value
	}
	return "jpg"
}
