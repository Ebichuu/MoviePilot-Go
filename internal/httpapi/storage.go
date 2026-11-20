package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/moviepilot-go/moviepilot-go/internal/storage"
)

type fileItemRequest struct {
	Path    string `json:"path"`
	Storage string `json:"storage"`
}

func decodeFileItem(w http.ResponseWriter, r *http.Request) (fileItemRequest, bool) {
	var input fileItemRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "存储请求无效")
		return fileItemRequest{}, false
	}
	return input, true
}

func (d Dependencies) storageProvider() storage.Provider {
	return d.Storage
}

func writeStorageFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrUnsupportedStorage):
		writeError(w, http.StatusOK, "存储实现未接入")
	case errors.Is(err, storage.ErrPathRejected), errors.Is(err, storage.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "存储请求无效")
	default:
		writeError(w, http.StatusInternalServerError, "存储操作失败")
	}
}

// storageList 复刻 V2 `/storage/list`（app/api/endpoints/storage.py:90–114）。
func (d Dependencies) storageList(w http.ResponseWriter, r *http.Request) {
	provider := d.storageProvider()
	if provider == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	input, ok := decodeFileItem(w, r)
	if !ok {
		return
	}
	items, err := provider.List(
		r.Context(), input.Storage, input.Path,
		strings.TrimSpace(r.URL.Query().Get("sort")),
		strings.TrimSpace(r.URL.Query().Get("keyword")),
	)
	if err != nil {
		writeStorageFailure(w, err)
		return
	}
	if items == nil {
		items = []storage.Item{}
	}
	writeJSON(w, http.StatusOK, items)
}

// storageMkdir 复刻 V2 `/storage/mkdir`（app/api/endpoints/storage.py:117–134）。
func (d Dependencies) storageMkdir(w http.ResponseWriter, r *http.Request) {
	provider := d.storageProvider()
	if provider == nil {
		writeJSON(w, http.StatusOK, Response{Success: false})
		return
	}
	input, ok := decodeFileItem(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeJSON(w, http.StatusOK, Response{Success: false})
		return
	}
	created, err := provider.Mkdir(r.Context(), input.Storage, input.Path, name)
	if err != nil {
		writeStorageFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: created})
}

// storageDelete 复刻 V2 `/storage/delete`（app/api/endpoints/storage.py:137–149）。
func (d Dependencies) storageDelete(w http.ResponseWriter, r *http.Request) {
	provider := d.storageProvider()
	if provider == nil {
		writeJSON(w, http.StatusOK, Response{Success: false})
		return
	}
	input, ok := decodeFileItem(w, r)
	if !ok {
		return
	}
	deleted, err := provider.Delete(r.Context(), input.Storage, input.Path)
	if err != nil {
		writeStorageFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: deleted})
}

// storageRename 复刻 V2 `/storage/rename`（app/api/endpoints/storage.py:184–205）。
func (d Dependencies) storageRename(w http.ResponseWriter, r *http.Request) {
	provider := d.storageProvider()
	if provider == nil {
		writeJSON(w, http.StatusOK, Response{Success: false})
		return
	}
	input, ok := decodeFileItem(w, r)
	if !ok {
		return
	}
	newName := strings.TrimSpace(r.URL.Query().Get("new_name"))
	if newName == "" {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "新名称为空"})
		return
	}
	recursive := strings.EqualFold(r.URL.Query().Get("recursive"), "true")
	renamed, err := provider.Rename(r.Context(), input.Storage, input.Path, newName, recursive)
	if err != nil {
		writeStorageFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: renamed})
}

// storageUsage 复刻 V2 `/storage/usage/{name}`（app/api/endpoints/storage.py:258–268）。
func (d Dependencies) storageUsage(w http.ResponseWriter, r *http.Request) {
	provider := d.storageProvider()
	if provider == nil {
		writeJSON(w, http.StatusOK, storage.Usage{})
		return
	}
	usage, err := provider.Usage(r.Context(), strings.TrimSpace(chi.URLParam(r, "name")))
	if err != nil {
		writeStorageFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// storageDownload 复刻 V2 `/storage/download`（app/api/endpoints/storage.py:152–165）。
func (d Dependencies) storageDownload(w http.ResponseWriter, r *http.Request) {
	opener, ok := d.storageProvider().(storage.FileOpener)
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false})
		return
	}
	input, ok := decodeFileItem(w, r)
	if !ok {
		return
	}
	file, contentType, err := opener.Open(r.Context(), input.Storage, input.Path)
	if err != nil {
		writeStorageFailure(w, err)
		return
	}
	defer func() { _ = file.Close() }()
	w.Header().Set("Content-Type", contentType)
	_, _ = io.Copy(w, file)
}

// storageImage 复刻 V2 `/storage/image`（app/api/endpoints/storage.py:168–181）。
func (d Dependencies) storageImage(w http.ResponseWriter, r *http.Request) {
	opener, ok := d.storageProvider().(storage.FileOpener)
	if !ok {
		writeError(w, http.StatusInternalServerError, "图片读取出错")
		return
	}
	input, ok := decodeFileItem(w, r)
	if !ok {
		return
	}
	file, _, err := opener.Open(r.Context(), input.Storage, input.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "图片读取出错")
		return
	}
	defer func() { _ = file.Close() }()
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = io.Copy(w, file)
}

// StorageAuthManager 是可选能力：远端存储的扫码/OAuth 登录与配置保存/重置。
type StorageAuthManager interface {
	QRCode(ctx context.Context, name string) (any, string, error)
	AuthURL(ctx context.Context, name string) (any, string, error)
	CheckLogin(ctx context.Context, name, ck, token string) (any, string, error)
	SaveConfig(ctx context.Context, name string, conf map[string]any) error
	ResetConfig(ctx context.Context, name string) error
}

func (d Dependencies) storageAuthManager() (StorageAuthManager, bool) {
	manager, ok := d.storageProvider().(StorageAuthManager)
	return manager, ok
}

// storageQRCode 复刻 V2 `/storage/qrcode/{name}`（app/api/endpoints/storage.py:29–37）。
func (d Dependencies) storageQRCode(w http.ResponseWriter, r *http.Request) {
	manager, ok := d.storageAuthManager()
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储实现未接入"})
		return
	}
	data, message, err := manager.QRCode(r.Context(), strings.TrimSpace(chi.URLParam(r, "name")))
	if err != nil && !errors.Is(err, storage.ErrOperationFailed) {
		writeStorageFailure(w, err)
		return
	}
	if data == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: message})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: data, Message: message})
}

// storageAuthURL 复刻 V2 `/storage/auth_url/{name}`（app/api/endpoints/storage.py:40–50）。
func (d Dependencies) storageAuthURL(w http.ResponseWriter, r *http.Request) {
	manager, ok := d.storageAuthManager()
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储实现未接入"})
		return
	}
	data, message, err := manager.AuthURL(r.Context(), strings.TrimSpace(chi.URLParam(r, "name")))
	if err != nil && !errors.Is(err, storage.ErrOperationFailed) {
		writeStorageFailure(w, err)
		return
	}
	if data == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: message})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: data})
}

// storageCheck 复刻 V2 `/storage/check/{name}`（app/api/endpoints/storage.py:53–69）。
func (d Dependencies) storageCheck(w http.ResponseWriter, r *http.Request) {
	manager, ok := d.storageAuthManager()
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: "存储实现未接入"})
		return
	}
	data, message, err := manager.CheckLogin(
		r.Context(), strings.TrimSpace(chi.URLParam(r, "name")),
		r.URL.Query().Get("ck"), r.URL.Query().Get("t"),
	)
	if err != nil && !errors.Is(err, storage.ErrOperationFailed) {
		writeStorageFailure(w, err)
		return
	}
	if data == nil {
		writeJSON(w, http.StatusOK, Response{Success: false, Message: message})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: data})
}

// storageSave 复刻 V2 `/storage/save/{name}`（app/api/endpoints/storage.py:72–78）。
func (d Dependencies) storageSave(w http.ResponseWriter, r *http.Request) {
	manager, ok := d.storageAuthManager()
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: true})
		return
	}
	var conf map[string]any
	if err := json.NewDecoder(r.Body).Decode(&conf); err != nil {
		writeError(w, http.StatusBadRequest, "存储配置请求无效")
		return
	}
	if err := manager.SaveConfig(r.Context(), strings.TrimSpace(chi.URLParam(r, "name")), conf); err != nil {
		writeStorageFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// storageReset 复刻 V2 `/storage/reset/{name}`（app/api/endpoints/storage.py:81–87）。
func (d Dependencies) storageReset(w http.ResponseWriter, r *http.Request) {
	manager, ok := d.storageAuthManager()
	if !ok {
		writeJSON(w, http.StatusOK, Response{Success: true})
		return
	}
	if err := manager.ResetConfig(r.Context(), strings.TrimSpace(chi.URLParam(r, "name"))); err != nil {
		writeStorageFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true})
}

// storageTransType 复刻 V2 `/storage/transtype/{name}`（app/api/endpoints/storage.py:271–285）。
func (d Dependencies) storageTransType(w http.ResponseWriter, r *http.Request) {
	provider := d.storageProvider()
	if provider == nil {
		writeJSON(w, http.StatusOK, map[string]any{"transtype": map[string]any{}})
		return
	}
	transtype, err := provider.TransType(r.Context(), strings.TrimSpace(chi.URLParam(r, "name")))
	if err != nil {
		writeStorageFailure(w, err)
		return
	}
	if transtype == nil {
		transtype = map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"transtype": transtype})
}
