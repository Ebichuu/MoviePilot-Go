package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
	"github.com/moviepilot-go/moviepilot-go/internal/organizer"
)

type mediaServerRuntimeStore interface {
	LoadMediaServers(context.Context) (mediaconfig.Snapshot, bool, error)
}

const (
	existingEpisodePageSize = 200
	existingEpisodeMaxItems = 10_000
)

type mediaServerEntry struct {
	setting     mediaconfig.Server
	client      *emby.Client
	fingerprint string

	userMu       sync.Mutex
	userID       string
	userResolved bool
}

type mediaServerRuntime struct {
	reloadMu sync.Mutex
	mu       sync.RWMutex
	snapshot mediaconfig.Snapshot
	entries  []*mediaServerEntry
	byID     map[string]*mediaServerEntry

	logger            *slog.Logger
	itemStore         mediaserver.ItemStore
	itemLookup        mediaserver.ItemLookup
	bootstrapUsername string
	syncMu            sync.Mutex
	now               func() time.Time
}

func loadMediaServerRuntime(
	ctx context.Context,
	store mediaServerRuntimeStore,
	timeout time.Duration,
	logger *slog.Logger,
) (*mediaServerRuntime, error) {
	if store == nil {
		return nil, errors.New("媒体服务器设置存储不可用")
	}
	snapshot, found, err := store.LoadMediaServers(ctx)
	if err != nil {
		return nil, errors.New("加载媒体服务器设置失败")
	}
	if !found {
		snapshot = mediaconfig.Snapshot{Servers: []mediaconfig.Server{}}
	}
	return buildMediaServerRuntime(snapshot, timeout, logger)
}

func buildMediaServerRuntime(
	snapshot mediaconfig.Snapshot,
	timeout time.Duration,
	logger *slog.Logger,
) (*mediaServerRuntime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	runtime := &mediaServerRuntime{
		snapshot: snapshot.Clone(),
		entries:  make([]*mediaServerEntry, 0, len(snapshot.Servers)),
		byID:     make(map[string]*mediaServerEntry, len(snapshot.Servers)),
		logger:   logger,
		now:      time.Now,
	}
	keepClients := false
	defer func() {
		if !keepClients {
			runtime.closeEntries(runtime.entries)
		}
	}()
	for _, setting := range snapshot.Servers {
		if !setting.Enabled {
			continue
		}
		if setting.Type != "emby" {
			return nil, errors.New("媒体服务器类型当前不受支持")
		}
		client, err := emby.NewClient(emby.Config{
			BaseURL:       setting.Config.Host,
			PlayHost:      setting.Config.PlayHost,
			Token:         setting.Config.APIKey,
			Authorization: embyAuthorization(setting.ID),
			Timeout:       timeout,
		})
		if err != nil {
			return nil, errors.New("Emby 运行时配置无效")
		}
		entry := &mediaServerEntry{
			setting: setting.Clone(), client: client,
			fingerprint: mediaServerEndpointFingerprint(setting),
		}
		runtime.entries = append(runtime.entries, entry)
		runtime.byID[setting.ID] = entry
		logger.Info("已装载媒体服务器", "server_id", setting.ID, "server", setting.Name, "type", setting.Type)
	}
	keepClients = true
	return runtime, nil
}

func (r *mediaServerRuntime) Reload(
	ctx context.Context,
	store mediaServerRuntimeStore,
	timeout time.Duration,
	logger *slog.Logger,
) error {
	if r == nil {
		return errors.New("媒体服务器运行时不可用")
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	next, err := loadMediaServerRuntime(ctx, store, timeout, logger)
	if err != nil {
		return err
	}
	r.mu.Lock()
	previous := r.entries
	r.snapshot = next.snapshot
	r.entries = next.entries
	r.byID = next.byID
	next.entries = nil
	r.mu.Unlock()
	r.closeEntries(previous)
	return nil
}

// SetItemCache wires the durable V2 `mediaserveritem` cache that the movie
// fast path reads and the sync task writes. Composition calls it once before
// the scheduled sync can run; Reload keeps the same cache because the cache is
// server-name keyed rather than entry pointer keyed.
func (r *mediaServerRuntime) SetItemCache(
	store mediaserver.ItemStore,
	lookup mediaserver.ItemLookup,
	bootstrapUsername string,
) error {
	if r == nil || store == nil || lookup == nil {
		return errMediaServerItemSyncUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.itemStore = store
	r.itemLookup = lookup
	r.bootstrapUsername = strings.TrimSpace(bootstrapUsername)
	return nil
}

func (r *mediaServerRuntime) itemCache() (mediaserver.ItemStore, mediaserver.ItemLookup) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.itemStore, r.itemLookup
}

func (r *mediaServerRuntime) CloseIdleConnections() {
	if r == nil {
		return
	}
	r.mu.Lock()
	entries := r.entries
	r.entries = nil
	r.byID = map[string]*mediaServerEntry{}
	r.mu.Unlock()
	r.closeEntries(entries)
}

func (r *mediaServerRuntime) closeEntries(entries []*mediaServerEntry) {
	for _, entry := range entries {
		if entry != nil && entry.client != nil {
			entry.client.CloseIdleConnections()
		}
	}
}

func (r *mediaServerRuntime) Resolve(ref mediaindex.ServerRef) (mediaindex.Server, bool) {
	if r == nil || !ref.Valid() {
		return nil, false
	}
	r.mu.RLock()
	entry, found := r.byID[strings.TrimSpace(ref.ID)]
	r.mu.RUnlock()
	if !found || entry == nil || entry.client == nil ||
		!strings.EqualFold(strings.TrimSpace(ref.Type), entry.setting.Type) ||
		ref.Fingerprint != entry.fingerprint {
		return nil, false
	}
	return mediaIndexEmbyServer{client: entry.client}, true
}

// MediaIndexCompletionPlan maps only video targets for the required server.
// A configured required server without an explicit matching path mapping is a
// retryable configuration error; the batch is not falsely completed.
func (r *mediaServerRuntime) MediaIndexCompletionPlan(
	ctx context.Context,
	plan organizer.Plan,
) (mediaindex.CompletionPlan, error) {
	if ctx == nil || r == nil {
		return mediaindex.CompletionPlan{}, errors.New("媒体入库确认规划不可用")
	}
	if err := ctx.Err(); err != nil {
		return mediaindex.CompletionPlan{}, err
	}
	r.mu.RLock()
	requiredID := r.snapshot.RequiredID
	entry := r.byID[requiredID]
	r.mu.RUnlock()
	if requiredID == "" {
		return mediaindex.CompletionPlan{}, nil
	}
	if entry == nil || entry.client == nil {
		return mediaindex.CompletionPlan{}, mediaindex.ErrRequiredServerUnavailable
	}
	server := mediaindex.ServerRef{
		ID: entry.setting.ID, Name: entry.setting.Name, Type: entry.setting.Type,
		Fingerprint: entry.fingerprint,
	}
	targets := make([]mediaindex.TargetFile, 0, len(plan.Operations))
	for _, operation := range plan.Operations {
		if err := ctx.Err(); err != nil {
			return mediaindex.CompletionPlan{}, err
		}
		if !isVideoTarget(operation.Target) {
			continue
		}
		mapped, found, err := mediaconfig.ResolvePath(entry.setting.PathMapping, operation.Target)
		if err != nil || !found {
			return mediaindex.CompletionPlan{}, mediaindex.ErrRequiredServerPathMapping
		}
		targets = append(targets, mediaindex.TargetFile{
			LocalPath: operation.Target, RemotePath: mapped.Path,
			PathStyle: string(mapped.Style), CaseSensitive: mapped.CasePolicy == mediaconfig.CasePolicySensitive,
			ExpectedSize: operation.ExpectedSize, ExpectedSHA256: operation.ExpectedSHA256,
			MediaType: "video",
		})
	}
	if len(targets) == 0 {
		return mediaindex.CompletionPlan{}, errors.New("整理计划没有可确认的视频文件")
	}
	return mediaindex.CompletionPlan{RequiredServer: &server, Files: targets}, nil
}

func (r *mediaServerRuntime) ListMediaServerClients(context.Context) []httpapi.MediaServerClient {
	entries := r.entriesSnapshot("")
	result := make([]httpapi.MediaServerClient, 0, len(entries))
	for _, entry := range entries {
		result = append(result, httpapi.MediaServerClient{Name: entry.setting.Name, Type: entry.setting.Type})
	}
	return result
}

func (r *mediaServerRuntime) PlayMediaServerItem(
	ctx context.Context,
	itemID string,
) (httpapi.MediaServerPlayback, bool, error) {
	if ctx == nil || strings.TrimSpace(itemID) == "" {
		return httpapi.MediaServerPlayback{}, false, nil
	}
	for _, entry := range r.entriesSnapshot("") {
		if err := ctx.Err(); err != nil {
			return httpapi.MediaServerPlayback{}, false, err
		}
		playURL, err := entry.client.PlayURL(itemID)
		if err != nil {
			continue
		}
		return httpapi.MediaServerPlayback{
			URL: playURL, ItemID: itemID, ServerID: entry.setting.ID, ServerType: entry.setting.Type,
		}, true, nil
	}
	return httpapi.MediaServerPlayback{}, false, nil
}

func (r *mediaServerRuntime) ListMediaServerLibraries(
	ctx context.Context,
	selector string,
	_ bool,
) ([]httpapi.MediaServerLibrary, error) {
	entries := r.entriesSnapshot(selector)
	result := make([]httpapi.MediaServerLibrary, 0)
	for _, entry := range entries {
		libraries, err := entry.client.ListLibraries(ctx)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("读取媒体服务器媒体库失败",
					"server", entry.setting.Name, "type", entry.setting.Type, "error", err)
			}
			return nil, errors.New("媒体服务器请求失败")
		}
		for _, library := range libraries {
			result = append(result, httpapi.MediaServerLibrary{
				Name: library.Name, Server: entry.setting.Name, Type: entry.setting.Type,
				Collection: library.CollectionType, Locations: append([]string(nil), library.Locations...), ItemID: library.ItemID,
			})
		}
	}
	return result, nil
}

func (*mediaServerRuntime) ListMediaServerLatest(context.Context, string, int) ([]httpapi.MediaServerPlayItem, error) {
	return []httpapi.MediaServerPlayItem{}, nil
}

func (*mediaServerRuntime) ListMediaServerPlaying(context.Context, string, int) ([]httpapi.MediaServerPlayItem, error) {
	return []httpapi.MediaServerPlayItem{}, nil
}

func (r *mediaServerRuntime) ExistingEpisodes(ctx context.Context, input httpapi.RemoteMediaInfo) (map[int][]int, error) {
	if strings.TrimSpace(input.Path) == "" {
		return map[int][]int{}, nil
	}
	found := make(map[int]map[int]struct{})
	for _, entry := range r.entriesSnapshot("") {
		for start := 0; start < existingEpisodeMaxItems; {
			response, err := entry.client.FindItems(ctx, emby.FindItemsOptions{
				Path: input.Path, AnyProviderIDEquals: tmdbProviderID(input.TMDBID),
				Recursive: true, Fields: []string{"Path", "ProviderIds"},
				StartIndex: start, Limit: existingEpisodePageSize,
			})
			if err != nil {
				return nil, errors.New("媒体服务器请求失败")
			}
			if response.StartIndex != start || len(response.Items) > existingEpisodePageSize ||
				response.TotalRecordCount < start+len(response.Items) {
				return nil, errors.New("媒体服务器返回了无效分页")
			}
			for _, item := range response.Items {
				if item.Type != "Episode" || item.ParentIndexNumber == nil || item.IndexNumber == nil ||
					*item.ParentIndexNumber < 0 || *item.IndexNumber < 1 {
					continue
				}
				if found[*item.ParentIndexNumber] == nil {
					found[*item.ParentIndexNumber] = make(map[int]struct{})
				}
				found[*item.ParentIndexNumber][*item.IndexNumber] = struct{}{}
			}
			next := start + len(response.Items)
			if next >= response.TotalRecordCount {
				break
			}
			if len(response.Items) == 0 || next >= existingEpisodeMaxItems {
				return nil, errors.New("媒体服务器结果超过有界分页限制")
			}
			start = next
		}
	}
	result := make(map[int][]int, len(found))
	for season, episodes := range found {
		result[season] = make([]int, 0, len(episodes))
		for episode := range episodes {
			result[season] = append(result[season], episode)
		}
		slices.Sort(result[season])
	}
	return result, nil
}

func (*mediaServerRuntime) MissingEpisodes(context.Context, httpapi.RemoteMediaInfo) ([]map[string]any, error) {
	return []map[string]any{}, nil
}

func (r *mediaServerRuntime) entriesSnapshot(selector string) []*mediaServerEntry {
	if r == nil {
		return nil
	}
	selector = strings.TrimSpace(selector)
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*mediaServerEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		if entry == nil || entry.client == nil {
			continue
		}
		if selector != "" && selector != entry.setting.ID && !strings.EqualFold(selector, entry.setting.Name) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

type mediaIndexEmbyServer struct{ client *emby.Client }

func (s mediaIndexEmbyServer) RefreshAll(ctx context.Context) error {
	if s.client == nil {
		return mediaindex.ErrServerUnavailable
	}
	if err := s.client.RefreshAll(ctx); err != nil {
		return translateEmbyError(err)
	}
	return nil
}

func (s mediaIndexEmbyServer) FindItems(ctx context.Context, query mediaindex.Query) ([]mediaindex.Item, error) {
	if s.client == nil {
		return nil, mediaindex.ErrServerUnavailable
	}
	response, err := s.client.FindItems(ctx, emby.FindItemsOptions{
		Path: query.Path, AnyProviderIDEquals: query.ProviderID,
		Recursive: true, Fields: []string{"Path", "ProviderIds"}, Limit: 2,
	})
	if err != nil {
		return nil, translateEmbyError(err)
	}
	items := make([]mediaindex.Item, 0, len(response.Items))
	for _, item := range response.Items {
		items = append(items, mediaindex.Item{
			ID: item.ID, Type: item.Type, Path: item.Path, ProviderIDs: cloneStringMap(item.ProviderIDs),
		})
	}
	return items, nil
}

func translateEmbyError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, emby.ErrOutcomeUnknown) {
		return mediaindex.ErrOutcomeUnknown
	}
	var upstream *emby.Error
	if errors.As(err, &upstream) {
		return &mediaindex.ServerError{Cause: upstream, Retryable: upstream.Code != emby.ErrorCodeInvalidConfig}
	}
	return mediaindex.ErrServerUnavailable
}

func embyAuthorization(deviceID string) string {
	return fmt.Sprintf(`Emby Client="MoviePilot-Go", Device="MoviePilot-Go", DeviceId="%s", Version="0.1"`, deviceID)
}

func tmdbProviderID(id int64) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("tmdb.%d", id)
}

func mediaServerEndpointFingerprint(server mediaconfig.Server) string {
	identity := strings.ToLower(strings.TrimSpace(server.Type)) + "\x00" + strings.TrimSpace(server.Config.Host)
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func isVideoTarget(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".avi", ".mov", ".ts", ".m2ts", ".wmv", ".flv", ".webm", ".iso":
		return true
	default:
		return false
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var _ mediaindex.Registry = (*mediaServerRuntime)(nil)
var _ httpapi.MediaServerClientSource = (*mediaServerRuntime)(nil)
var _ httpapi.MediaServerPlaybackSource = (*mediaServerRuntime)(nil)
var _ httpapi.MediaServerLibrarySource = (*mediaServerRuntime)(nil)
var _ httpapi.RemoteMediaSource = (*mediaServerRuntime)(nil)
