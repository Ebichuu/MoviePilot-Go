package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/downloader"
)

// DownloadTaskControlSource resolves and controls one task on a selected client.
type DownloadTaskControlSource interface {
	FindDownloadTask(context.Context, string, string) (NamedDownloadTask, bool, error)
	ControlDownloadTask(context.Context, string, string, downloader.TaskControlAction) error
}

func (d Dependencies) startDownload(w http.ResponseWriter, r *http.Request) {
	d.controlDownloadTask(w, r, downloader.TaskControlStart)
}

func (d Dependencies) stopDownload(w http.ResponseWriter, r *http.Request) {
	d.controlDownloadTask(w, r, downloader.TaskControlStop)
}

func (d Dependencies) deleteDownload(w http.ResponseWriter, r *http.Request) {
	d.controlDownloadTask(w, r, downloader.TaskControlDelete)
}

func (d Dependencies) controlDownloadTask(w http.ResponseWriter, r *http.Request, action downloader.TaskControlAction) {
	if _, ok := claimsFromContext(r.Context()); !ok {
		writeError(w, http.StatusForbidden, "用户身份无效")
		return
	}
	hash, ok := normalizeDownloadControlHash(chi.URLParam(r, "hashString"))
	if !ok {
		writeDownloadControlResult(w, false)
		return
	}
	source, ok := d.DownloadTasks.(DownloadTaskControlSource)
	if !ok {
		writeDownloadControlResult(w, false)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	named, found, err := source.FindDownloadTask(r.Context(), name, hash)
	if err != nil || !found || strings.TrimSpace(named.Task.Label) == "" {
		writeDownloadControlResult(w, false)
		return
	}
	if err := source.ControlDownloadTask(r.Context(), named.Downloader, hash, action); err != nil {
		writeDownloadControlResult(w, false)
		return
	}
	writeDownloadControlResult(w, true)
}

func normalizeDownloadControlHash(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", false
	}
	return strings.ToUpper(value), true
}

func writeDownloadControlResult(w http.ResponseWriter, success bool) {
	writeJSON(w, http.StatusOK, Response{Success: success})
}
