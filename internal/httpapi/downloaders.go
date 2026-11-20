package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/downloadconfig"
)

const defaultDownloaderRequestBodyLimit int64 = 1 << 20

const downloaderReloadTimeout = 5 * time.Second

// DownloaderSettingStore 是 HTTP 下载器设置接口需要的最小持久化契约。
type DownloaderSettingStore interface {
	LoadDownloaders(context.Context) ([]downloadconfig.Downloader, bool, error)
	SaveDownloaders(context.Context, []downloadconfig.Downloader) error
}

type downloaderSetting = downloadconfig.Downloader

type downloaderClient struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (d Dependencies) getDownloadersSetting(w http.ResponseWriter, r *http.Request) {
	store, err := d.downloaderSettingStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器设置失败")
		return
	}

	settings, found, err := store.LoadDownloaders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器设置失败")
		return
	}
	if !found {
		writeSuccess(w, map[string]any{"value": nil})
		return
	}

	changed, err := normalizeDownloaderSettings(settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器设置失败")
		return
	}
	secretMetadataChanged, err := sanitizeStoredDownloaderSecrets(settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器设置失败")
		return
	}
	if changed || secretMetadataChanged {
		if err := store.SaveDownloaders(r.Context(), settings); err != nil {
			writeError(w, http.StatusInternalServerError, "保存下载器设置失败")
			return
		}
	}

	redacted, err := redactDownloaderSecrets(settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器设置失败")
		return
	}
	writeSuccess(w, map[string]any{"value": redacted})
}

func (d Dependencies) setDownloadersSetting(w http.ResponseWriter, r *http.Request) {
	incoming, err := readDownloaderSettingsRequest(r, d.Config.MaxRequestBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, "下载器设置无效")
		return
	}
	if _, err := normalizeDownloaderSettings(incoming); err != nil {
		writeError(w, http.StatusBadRequest, "下载器设置无效")
		return
	}

	store, err := d.downloaderSettingStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存下载器设置失败")
		return
	}
	existing, found, err := store.LoadDownloaders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存下载器设置失败")
		return
	}
	if found {
		if _, err := normalizeDownloaderSettings(existing); err != nil {
			writeError(w, http.StatusInternalServerError, "保存下载器设置失败")
			return
		}
		if _, err := sanitizeStoredDownloaderSecrets(existing); err != nil {
			writeError(w, http.StatusInternalServerError, "保存下载器设置失败")
			return
		}
	}

	if err := mergeDownloaderSecrets(incoming, existing); err != nil {
		writeError(w, http.StatusBadRequest, "下载器设置无效")
		return
	}
	if _, err := normalizeDownloaderSettings(incoming); err != nil {
		writeError(w, http.StatusBadRequest, "下载器设置无效")
		return
	}
	if _, err := sanitizeStoredDownloaderSecrets(incoming); err != nil {
		writeError(w, http.StatusBadRequest, "下载器设置无效")
		return
	}
	if err := store.SaveDownloaders(r.Context(), incoming); err != nil {
		writeError(w, http.StatusInternalServerError, "保存下载器设置失败")
		return
	}
	if d.ReloadDownloaders != nil {
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), downloaderReloadTimeout)
		reloadErr := d.ReloadDownloaders(reloadCtx)
		cancel()
		if reloadErr != nil {
			writeJSON(w, http.StatusServiceUnavailable, Response{
				Success: false,
				Message: "下载器配置已持久化，但运行态重载失败，旧快照仍在使用，请稍后重试",
				Data:    map[string]any{"reload": false},
			})
			return
		}
	}
	writeSuccess(w, nil)
}

func (d Dependencies) downloadClients(w http.ResponseWriter, r *http.Request) {
	store, err := d.downloaderSettingStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器列表失败")
		return
	}
	settings, found, err := store.LoadDownloaders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器列表失败")
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, []downloaderClient{})
		return
	}
	if _, err := normalizeDownloaderSettings(settings); err != nil {
		writeError(w, http.StatusInternalServerError, "读取下载器列表失败")
		return
	}

	clients := make([]downloaderClient, 0, len(settings))
	for _, setting := range settings {
		if setting.Enabled {
			clients = append(clients, downloaderClient{Name: setting.Name, Type: setting.Type})
		}
	}
	writeJSON(w, http.StatusOK, clients)
}

func (d Dependencies) downloaderSettingStore() (DownloaderSettingStore, error) {
	if d.DownloaderSettings != nil {
		return d.DownloaderSettings, nil
	}
	if d.Store != nil {
		return d.Store, nil
	}
	return nil, errors.New("下载器设置存储不可用")
}

func readDownloaderSettingsRequest(r *http.Request, maxBytes int64) ([]downloaderSetting, error) {
	if maxBytes <= 0 {
		maxBytes = defaultDownloaderRequestBodyLimit
	}
	value, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil || int64(len(value)) > maxBytes {
		return nil, errors.New("读取下载器设置请求失败")
	}
	return decodeDownloaderSettings(value)
}

func decodeDownloaderSettings(value json.RawMessage) ([]downloaderSetting, error) {
	return downloadconfig.Decode(value)
}

func normalizeDownloaderSettings(settings []downloaderSetting) (bool, error) {
	return downloadconfig.Normalize(settings)
}

func mergeDownloaderSecrets(incoming, existing []downloaderSetting) error {
	return downloadconfig.MergeSecrets(incoming, existing)
}

func sanitizeStoredDownloaderSecrets(settings []downloaderSetting) (bool, error) {
	return downloadconfig.SanitizeStoredSecrets(settings)
}

func redactDownloaderSecrets(settings []downloaderSetting) ([]downloaderSetting, error) {
	return downloadconfig.RedactSecrets(settings)
}
