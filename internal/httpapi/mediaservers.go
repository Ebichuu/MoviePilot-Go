package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
)

const mediaServersReloadTimeout = 5 * time.Second

// MediaServerSettingStore 是 MediaServers 专用加密设置的最小持久化契约。
type MediaServerSettingStore interface {
	LoadMediaServers(context.Context) (mediaconfig.Snapshot, bool, error)
	SaveMediaServers(context.Context, []mediaconfig.Server) (mediaconfig.Snapshot, error)
}

type MediaServerClient struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type MediaServerClientSource interface {
	ListMediaServerClients(context.Context) []MediaServerClient
}

type MediaServerPlayback struct {
	URL        string `json:"url"`
	ItemID     string `json:"item_id"`
	ServerID   string `json:"server_id"`
	ServerType string `json:"server_type"`
}

type MediaServerPlaybackSource interface {
	PlayMediaServerItem(context.Context, string) (MediaServerPlayback, bool, error)
}

type MediaServerLibrary struct {
	Name       string   `json:"name"`
	Server     string   `json:"server"`
	Type       string   `json:"type,omitempty"`
	Collection string   `json:"collection_type,omitempty"`
	Locations  []string `json:"locations,omitempty"`
	ItemID     string   `json:"item_id,omitempty"`
	Hidden     bool     `json:"hidden,omitempty"`
}

type MediaServerPlayItem struct {
	ItemID     string `json:"item_id,omitempty"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
	ServerID   string `json:"server_id,omitempty"`
	ServerType string `json:"server_type,omitempty"`
}

type MediaServerLibrarySource interface {
	ListMediaServerLibraries(context.Context, string, bool) ([]MediaServerLibrary, error)
	ListMediaServerLatest(context.Context, string, int) ([]MediaServerPlayItem, error)
	ListMediaServerPlaying(context.Context, string, int) ([]MediaServerPlayItem, error)
}

type LocalMediaExistsQuery struct {
	Title  string
	Year   string
	Type   string
	TMDBID int64
	Season *int
}

type LocalMediaIndexSource interface {
	FindLocalMediaServerItem(context.Context, LocalMediaExistsQuery) (string, bool, error)
}

type RemoteMediaInfo struct {
	Title        string `json:"title"`
	Type         string `json:"type"`
	Year         string `json:"year"`
	TMDBID       int64  `json:"tmdbid"`
	Season       *int   `json:"season"`
	TotalEpisode int    `json:"total_episode"`
	Path         string `json:"path"`
}

type RemoteMediaSource interface {
	ExistingEpisodes(context.Context, RemoteMediaInfo) (map[int][]int, error)
	MissingEpisodes(context.Context, RemoteMediaInfo) ([]map[string]any, error)
}

func (d Dependencies) mediaServerSettingStore() (MediaServerSettingStore, error) {
	if d.MediaServerSettings != nil {
		return d.MediaServerSettings, nil
	}
	if d.Store != nil {
		return d.Store, nil
	}
	return nil, errors.New("媒体服务器设置存储不可用")
}

func (d Dependencies) getMediaServersSetting(w http.ResponseWriter, r *http.Request) {
	store, err := d.mediaServerSettingStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取媒体服务器设置失败")
		return
	}
	snapshot, found, err := store.LoadMediaServers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取媒体服务器设置失败")
		return
	}
	if !found {
		writeSuccess(w, map[string]any{"value": nil, "required": ""})
		return
	}
	redacted := mediaconfig.RedactSecrets(snapshot)
	writeSuccess(w, map[string]any{"value": redacted.Servers, "required": redacted.RequiredID})
}

func (d Dependencies) setMediaServersSetting(w http.ResponseWriter, r *http.Request) {
	incoming, err := readMediaServersRequest(r, d.Config.MaxRequestBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, "媒体服务器设置无效")
		return
	}
	incomingSnapshot, _, err := mediaconfig.Normalize(incoming)
	if err != nil {
		writeError(w, http.StatusBadRequest, "媒体服务器设置无效")
		return
	}
	incoming = incomingSnapshot.Servers
	store, err := d.mediaServerSettingStore()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存媒体服务器设置失败")
		return
	}
	existing, found, err := store.LoadMediaServers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存媒体服务器设置失败")
		return
	}
	if found {
		incoming, err = mediaconfig.MergeSecrets(incoming, existing.Servers)
		if err != nil {
			writeError(w, http.StatusBadRequest, "媒体服务器设置无效")
			return
		}
	}
	if err := mediaconfig.Validate(incoming); err != nil {
		writeError(w, http.StatusBadRequest, "媒体服务器设置无效")
		return
	}
	saved, err := store.SaveMediaServers(r.Context(), incoming)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "保存媒体服务器设置失败")
		return
	}
	if d.ReloadMediaServers != nil {
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), mediaServersReloadTimeout)
		reloadErr := d.ReloadMediaServers(reloadCtx)
		cancel()
		if reloadErr != nil {
			if d.Logger != nil {
				d.Logger.Warn("媒体服务器运行态重载失败", "error", reloadErr)
			}
			writeJSON(w, http.StatusServiceUnavailable, Response{
				Success: false,
				Message: "媒体服务器配置已持久化，但运行态重载失败，旧快照仍在使用，请稍后重试",
				Data:    map[string]any{"reload": false},
			})
			return
		}
	}
	redacted := mediaconfig.RedactSecrets(saved)
	writeSuccess(w, map[string]any{"value": redacted.Servers, "required": redacted.RequiredID})
}

func readMediaServersRequest(r *http.Request, maxBytes int64) ([]mediaconfig.Server, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	value, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil || int64(len(value)) > maxBytes {
		return nil, errors.New("读取媒体服务器设置请求失败")
	}
	return mediaconfig.Decode(value)
}

func (d Dependencies) mediaServerClients(w http.ResponseWriter, r *http.Request) {
	if d.MediaServerClients == nil {
		writeJSON(w, http.StatusOK, []MediaServerClient{})
		return
	}
	writeJSON(w, http.StatusOK, d.MediaServerClients.ListMediaServerClients(r.Context()))
}

func (d Dependencies) mediaServerExists(w http.ResponseWriter, r *http.Request) {
	query := LocalMediaExistsQuery{
		Title: r.URL.Query().Get("title"), Year: r.URL.Query().Get("year"), Type: r.URL.Query().Get("mtype"),
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("tmdbid")); raw != "" {
		query.TMDBID, _ = strconv.ParseInt(raw, 10, 64)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("season")); raw != "" {
		if season, parseErr := strconv.Atoi(raw); parseErr == nil {
			query.Season = &season
		}
	}
	if d.LocalMediaIndex == nil {
		writeError(w, http.StatusOK, "未找到媒体")
		return
	}
	itemID, found, err := d.LocalMediaIndex.FindLocalMediaServerItem(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusBadGateway, "媒体索引查询失败")
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, Response{Success: false, Data: map[string]any{"item": map[string]any{}}})
		return
	}
	writeJSON(w, http.StatusOK, Response{Success: true, Data: map[string]any{"item": map[string]any{"id": itemID}}})
}

func (d Dependencies) mediaServerExistsRemote(w http.ResponseWriter, r *http.Request) {
	input, err := readRemoteMediaInfoRequest(r, d.Config.MaxRequestBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, "媒体参数无效")
		return
	}
	if source, ok := d.MediaServerRemote.(RemoteMediaSource); ok && source != nil {
		result, err := source.ExistingEpisodes(r.Context(), input)
		if err != nil {
			writeError(w, http.StatusBadGateway, "媒体服务器请求失败")
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeJSON(w, http.StatusOK, map[int][]int{})
}

func (d Dependencies) mediaServerNotExists(w http.ResponseWriter, r *http.Request) {
	input, err := readRemoteMediaInfoRequest(r, d.Config.MaxRequestBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, "媒体参数无效")
		return
	}
	if source, ok := d.MediaServerRemote.(RemoteMediaSource); ok && source != nil {
		result, err := source.MissingEpisodes(r.Context(), input)
		if err != nil {
			writeError(w, http.StatusBadGateway, "媒体服务器请求失败")
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeJSON(w, http.StatusOK, []map[string]any{})
}

func readRemoteMediaInfoRequest(r *http.Request, maxBytes int64) (RemoteMediaInfo, error) {
	if r == nil || r.Body == nil {
		return RemoteMediaInfo{}, errors.New("invalid media server request")
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return RemoteMediaInfo{}, errors.New("invalid media server request")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var input RemoteMediaInfo
	if err := decoder.Decode(&input); err != nil {
		return RemoteMediaInfo{}, errors.New("invalid media server request")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RemoteMediaInfo{}, errors.New("invalid media server request")
	}
	return input, nil
}

func (d Dependencies) mediaServerLatest(w http.ResponseWriter, r *http.Request) {
	if d.MediaServerLibraries == nil {
		writeJSON(w, http.StatusOK, []MediaServerPlayItem{})
		return
	}
	count := mediaServerCount(r, 20, 100)
	result, err := d.MediaServerLibraries.ListMediaServerLatest(r.Context(), r.URL.Query().Get("server"), count)
	if err != nil {
		writeError(w, http.StatusBadGateway, "媒体服务器请求失败")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (d Dependencies) mediaServerPlaying(w http.ResponseWriter, r *http.Request) {
	if d.MediaServerLibraries == nil {
		writeJSON(w, http.StatusOK, []MediaServerPlayItem{})
		return
	}
	count := mediaServerCount(r, 12, 100)
	result, err := d.MediaServerLibraries.ListMediaServerPlaying(r.Context(), r.URL.Query().Get("server"), count)
	if err != nil {
		writeError(w, http.StatusBadGateway, "媒体服务器请求失败")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// mediaServerLibraryTimeout 限制一次媒体库列表调用的总耗时。
const mediaServerLibraryTimeout = 10 * time.Second

func (d Dependencies) mediaServerLibrary(w http.ResponseWriter, r *http.Request) {
	if d.MediaServerLibraries == nil {
		writeJSON(w, http.StatusOK, []MediaServerLibrary{})
		return
	}
	hidden := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("hidden")), "true")
	// 媒体服务器不可达时，客户端自身会重试（15 秒 × 2），dashboard 就要等 30 秒。
	// 这里给整次调用一个上限，快速返回如实的失败信息，而不是把前端挂住。
	ctx, cancel := context.WithTimeout(r.Context(), mediaServerLibraryTimeout)
	defer cancel()
	result, err := d.MediaServerLibraries.ListMediaServerLibraries(ctx, r.URL.Query().Get("server"), hidden)
	if err != nil {
		writeError(w, http.StatusBadGateway, "媒体服务器请求失败")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func mediaServerCount(r *http.Request, fallback, max int) int {
	count, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("count")))
	if err != nil || count < 1 {
		return fallback
	}
	if count > max {
		return max
	}
	return count
}
