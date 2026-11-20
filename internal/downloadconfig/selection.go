package downloadconfig

import (
	"strings"

	"github.com/google/uuid"
)

// SelectEnabledQBittorrent resolves an explicit name or ID, otherwise the
// configured default. Only enabled qBittorrent instances with stable IDs are
// eligible.
func SelectEnabledQBittorrent(settings []Downloader, selector string) (Downloader, bool) {
	selector = strings.TrimSpace(selector)
	if selector != "" {
		for _, configured := range settings {
			if strings.EqualFold(strings.TrimSpace(configured.Name), selector) ||
				strings.EqualFold(strings.TrimSpace(configured.ID), selector) {
				if UsableQBittorrent(configured) {
					return configured, true
				}
				return Downloader{}, false
			}
		}
		return Downloader{}, false
	}
	for _, configured := range settings {
		if configured.Default && UsableQBittorrent(configured) {
			return configured, true
		}
	}
	return Downloader{}, false
}

// UsableQBittorrent validates the runtime identity required by durable plans.
func UsableQBittorrent(configured Downloader) bool {
	parsedID, err := uuid.Parse(strings.TrimSpace(configured.ID))
	return configured.Enabled &&
		strings.EqualFold(strings.TrimSpace(configured.Type), "qbittorrent") &&
		err == nil && parsedID != uuid.Nil
}

// supportedDownloaderTypes 是 X01 已具备种子字节闭环的下载器类型。
var supportedDownloaderTypes = map[string]struct{}{
	"qbittorrent":  {},
	"transmission": {},
	"rtorrent":     {},
}

// SelectEnabledDownloader 解析显式名称/ID 或默认项，支持 qBittorrent、
// Transmission 与 rTorrent；只有启用且带稳定 ID 的实例可用。
func SelectEnabledDownloader(settings []Downloader, selector string) (Downloader, bool) {
	selector = strings.TrimSpace(selector)
	if selector != "" {
		for _, configured := range settings {
			if strings.EqualFold(strings.TrimSpace(configured.Name), selector) ||
				strings.EqualFold(strings.TrimSpace(configured.ID), selector) {
				if UsableDownloader(configured) {
					return configured, true
				}
				return Downloader{}, false
			}
		}
		return Downloader{}, false
	}
	for _, configured := range settings {
		if configured.Default && UsableDownloader(configured) {
			return configured, true
		}
	}
	return Downloader{}, false
}

// UsableDownloader 校验运行期身份与受支持的下载器类型。
func UsableDownloader(configured Downloader) bool {
	parsedID, err := uuid.Parse(strings.TrimSpace(configured.ID))
	if !configured.Enabled || err != nil || parsedID == uuid.Nil {
		return false
	}
	_, ok := supportedDownloaderTypes[strings.ToLower(strings.TrimSpace(configured.Type))]
	return ok
}
