package app

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaconfig"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
)

const (
	// mediaServerItemPageSize bounds every user-scoped item page the same way
	// the R07 A client slice does.
	mediaServerItemPageSize = 200
	// V2 fetches a whole library and its episodes in single unbounded requests.
	// Go enumerates the same data through bounded pages and refuses to treat a
	// truncated enumeration as a complete one.
	mediaServerItemMaxPerLibrary = 10_000
	mediaServerItemMaxEpisodes   = 10_000
	// 常见媒体库会把每个影片放在单独目录（Emby 返回 Folder 条目），2500 部电影就会
	// 产生 2500 个目录，因此目录上限必须远高于媒体库条目数，否则正常库会被判成
	// 「未完整枚举」。
	mediaServerItemMaxFolders = 50_000
	mediaServerItemMaxDepth   = 8
	// mediaServerSyncTimeLayout is V2's `%Y-%m-%d %H:%M:%S.%f` sync stamp.
	mediaServerSyncTimeLayout = "2006-01-02 15:04:05.000000"
)

var (
	errMediaServerItemSyncUnavailable = errors.New("媒体服务器条目同步不可用")
	errMediaServerItemSyncFailed      = errors.New("媒体服务器条目同步失败")
)

type mediaServerFolder struct {
	id    string
	depth int
}

type mediaServerLibrarySyncResult struct {
	items    int
	folders  int
	complete bool
}

// SyncMediaServerItems refreshes the V2 `mediaserveritem` cache and is the only
// writer of that cache. An empty selector syncs every enabled server (V2's
// manual `mediaserver_sync` job); a named selector targets one server the way
// V2's per-server schedule does. V2 serializes syncs with a process-wide lock
// and so does this implementation: the cache is the fact source for the movie
// fast path, so two runs must never interleave stale deletion.
func (r *mediaServerRuntime) SyncMediaServerItems(ctx context.Context, selector string) error {
	if r == nil || ctx == nil {
		return errMediaServerItemSyncUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.syncMu.Lock()
	defer r.syncMu.Unlock()

	selector = strings.TrimSpace(selector)
	r.mu.RLock()
	store := r.itemStore
	entries := make([]*mediaServerEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		if entry != nil && entry.client != nil {
			entries = append(entries, entry)
		}
	}
	r.mu.RUnlock()
	if len(entries) == 0 {
		// V2 skips the run entirely when no media server is configured.
		return nil
	}
	if store == nil {
		return errMediaServerItemSyncUnavailable
	}

	// V2 reconciles the cache against every enabled server name even when the
	// run only targets one server.
	names := make([]string, 0, len(entries))
	selected := make([]*mediaServerEntry, 0, len(entries))
	for _, entry := range entries {
		names = append(names, strings.TrimSpace(entry.setting.Name))
		if selector == "" || mediaServerEntryMatchesSelector(entry, selector) {
			selected = append(selected, entry)
		}
	}
	if _, err := store.DeleteExcludedMediaServerItems(ctx, names); err != nil {
		return mediaServerSyncError(ctx, err)
	}

	failed := false
	for _, entry := range selected {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.syncServerItems(ctx, store, entry); err != nil {
			if contextErr := mediaServerSubscriptionContextError(ctx, err); contextErr != nil {
				return contextErr
			}
			failed = true
			r.logWarn("同步媒体服务器条目失败", "server", entry.setting.Name, "error", err)
		}
	}
	if failed {
		return errMediaServerItemSyncFailed
	}
	return nil
}

// SyncSchedules lists the enabled servers and their effective sync intervals.
// A server without a positive `sync_interval` falls back to the default, which
// mirrors V2's absent value; V2 treats an explicit `0` as "no schedule", which
// Go cannot distinguish from an absent field and therefore also maps to the
// default.
func (r *mediaServerRuntime) SyncSchedules(defaultInterval time.Duration) []mediaserver.SyncSchedule {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	servers := append([]mediaconfig.Server(nil), r.snapshot.Servers...)
	r.mu.RUnlock()
	schedules := make([]mediaserver.SyncSchedule, 0, len(servers))
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		name := strings.TrimSpace(server.Name)
		if name == "" {
			continue
		}
		interval := time.Duration(server.SyncInterval) * time.Hour
		if server.SyncInterval <= 0 {
			interval = defaultInterval
		}
		if interval <= 0 {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		schedules = append(schedules, mediaserver.SyncSchedule{Server: name, Interval: interval})
	}
	return schedules
}

func mediaServerEntryMatchesSelector(entry *mediaServerEntry, selector string) bool {
	if entry == nil {
		return false
	}
	return selector == entry.setting.ID || strings.EqualFold(selector, strings.TrimSpace(entry.setting.Name))
}

func (r *mediaServerRuntime) syncServerItems(
	ctx context.Context,
	store mediaserver.ItemStore,
	entry *mediaServerEntry,
) error {
	userID := r.entryUserID(ctx, entry)
	if strings.TrimSpace(userID) == "" {
		// V2 cannot enumerate user-scoped items without a resolved user and
		// would still run stale deletion on the resulting empty list. Go keeps
		// the previous rows for a server it could not read.
		return errMediaServerItemSyncFailed
	}
	views, err := entry.client.ListUserViews(ctx, userID)
	if err != nil {
		return err
	}
	libraries := selectMediaServerSyncLibraries(views, entry.setting.SyncLibraries)
	if len(libraries) == 0 {
		// V2 skips a server with no library (or no selected library) and keeps
		// the previous rows.
		return nil
	}
	syncTime := r.currentTime().Format(mediaServerSyncTimeLayout)
	complete := true
	var firstErr error
	for _, library := range libraries {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, libraryErr := r.syncLibraryItems(ctx, store, entry, userID, library, syncTime)
		switch {
		case libraryErr != nil:
			if contextErr := mediaServerSubscriptionContextError(ctx, libraryErr); contextErr != nil {
				return contextErr
			}
			r.logWarn("媒体库条目同步失败",
				"server", entry.setting.Name, "library", library.Name, "library_id", library.ID,
				"items", result.items, "folders", result.folders, "error", libraryErr)
			complete = false
			if firstErr == nil {
				firstErr = libraryErr
			}
		case !result.complete:
			r.logWarn("媒体库条目同步未完成",
				"server", entry.setting.Name, "library", library.Name, "library_id", library.ID,
				"items", result.items, "folders", result.folders)
			complete = false
			if firstErr == nil {
				firstErr = errMediaServerItemSyncFailed
			}
		}
	}
	if !complete {
		// V2 deletes stale rows unconditionally, which lets an adapter-side
		// empty result clear a server's cache. Go only removes rows after a
		// complete enumeration of that server.
		return firstErr
	}
	if _, err := store.DeleteStaleMediaServerItems(ctx, strings.TrimSpace(entry.setting.Name), syncTime); err != nil {
		return err
	}
	return nil
}

func selectMediaServerSyncLibraries(views []emby.UserView, syncLibraries []string) []emby.UserView {
	if len(views) == 0 {
		return nil
	}
	all := false
	allowed := make(map[string]struct{}, len(syncLibraries))
	for _, library := range syncLibraries {
		if library == "all" {
			all = true
			continue
		}
		allowed[library] = struct{}{}
	}
	selected := make([]emby.UserView, 0, len(views))
	for _, view := range views {
		id := strings.TrimSpace(view.ID)
		if id == "" {
			continue
		}
		if len(syncLibraries) > 0 && !all {
			if _, ok := allowed[id]; !ok {
				continue
			}
		}
		selected = append(selected, view)
	}
	return selected
}

func mediaServerSyncError(ctx context.Context, err error) error {
	if contextErr := mediaServerSubscriptionContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	return errMediaServerItemSyncFailed
}

func (r *mediaServerRuntime) currentTime() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *mediaServerRuntime) logWarn(message string, args ...any) {
	if r == nil || r.logger == nil {
		return
	}
	r.logger.Warn(message, args...)
}

// entryUserID resolves V2's connection-time user for one server. V2 keeps that
// user for the lifetime of the connection; Go caches the first successful
// resolution and retries after a failure. The caller-supplied name is the
// explicitly configured local username and is never guessed inside the client.
func (r *mediaServerRuntime) entryUserID(ctx context.Context, entry *mediaServerEntry) string {
	if entry == nil || entry.client == nil {
		return ""
	}
	entry.userMu.Lock()
	defer entry.userMu.Unlock()
	if entry.userResolved {
		return entry.userID
	}
	users, err := entry.client.ListUsers(ctx)
	if err != nil {
		// 非管理员令牌（Emby 只读密钥）没有用户枚举权限，拿不到真实用户 ID；
		// 退回 Emby 的 Me 别名后，媒体库与条目枚举仍按该令牌的可见范围工作。
		entry.userID = emby.SelfUserID
		entry.userResolved = true
		return entry.userID
	}
	userID := emby.ResolveUserID(users, r.bootstrapUsername, entry.setting.Config.Username)
	entry.userID = userID
	entry.userResolved = true
	return entry.userID
}

// loadMediaServerSeasonInfo builds V2's `seasoninfo` object for one series.
// V2 copies the raw episode response order; Go de-duplicates and sorts the
// episode numbers so the cached object is deterministic.
func (r *mediaServerRuntime) loadMediaServerSeasonInfo(
	ctx context.Context,
	entry *mediaServerEntry,
	seriesID string,
) (map[int][]int, bool, error) {
	seasons := make(map[int][]int)
	seen := make(map[int]map[int]struct{})
	complete := true
	for start := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		response, err := entry.client.ListEpisodes(ctx, emby.ListEpisodesOptions{
			SeriesID: seriesID, StartIndex: start, Limit: mediaServerItemPageSize,
		})
		if err != nil {
			return nil, false, err
		}
		if !mediaServerPageConsistent(response, start, mediaServerItemPageSize) {
			return nil, false, errMediaServerItemSyncFailed
		}
		for _, item := range response.Items {
			if item.ParentIndexNumber == nil || item.IndexNumber == nil ||
				*item.ParentIndexNumber < 0 || *item.ParentIndexNumber > 999 ||
				*item.IndexNumber < 1 || *item.IndexNumber > 5000 {
				continue
			}
			season, episode := *item.ParentIndexNumber, *item.IndexNumber
			if _, ok := seen[season][episode]; ok {
				continue
			}
			if seen[season] == nil {
				seen[season] = make(map[int]struct{})
			}
			seen[season][episode] = struct{}{}
			seasons[season] = append(seasons[season], episode)
		}
		next := start + len(response.Items)
		if mediaServerPageDone(response, next, mediaServerItemPageSize) {
			break
		}
		if len(response.Items) == 0 || next >= mediaServerItemMaxEpisodes {
			complete = false
			break
		}
		start = next
	}
	if !complete {
		return nil, false, nil
	}
	for season := range seasons {
		slices.Sort(seasons[season])
	}
	return seasons, true, nil
}

// mediaServerPageConsistent 校验一页分页响应。Emby 的部分分支（如 fnOS 4.8）不回填
// StartIndex，另有分支对不带用户段的 /Items 查询返回 TotalRecordCount=0，因此这两项只在
// 服务端确实给出时才当作硬约束，避免把正常分页误判成同步失败。
func mediaServerPageConsistent(response emby.ItemsResponse, start, pageSize int) bool {
	if len(response.Items) > pageSize {
		return false
	}
	if response.StartIndex != 0 && response.StartIndex != start {
		return false
	}
	if response.TotalRecordCount > 0 && response.TotalRecordCount < start+len(response.Items) {
		return false
	}
	return true
}

// mediaServerPageDone 判断分页是否读完：有总数时按总数判断，没有总数时退回「短页即结束」。
func mediaServerPageDone(response emby.ItemsResponse, next, pageSize int) bool {
	if response.TotalRecordCount > 0 {
		return next >= response.TotalRecordCount
	}
	return len(response.Items) < pageSize
}

func isMediaServerFolder(itemType string) bool {
	return strings.Contains(itemType, "Folder")
}

// mediaServerItemType maps Emby item types to the two stored values. V2 maps
// everything that is not `Series`/`show` to a movie, but its generator already
// restricts the stream to Movie/Series entries, so this is the same set.
func mediaServerItemType(itemType string) (string, bool) {
	switch strings.TrimSpace(itemType) {
	case "Series", "show":
		return mediaserver.ItemTypeTV, true
	case "Movie":
		return mediaserver.ItemTypeMovie, true
	default:
		return "", false
	}
}

// mediaServerCachedItem projects one Emby item onto the cache row. V2 drops an
// item whose formatter fails, including a TMDB provider ID that cannot be
// converted to an integer, so an unparsable TMDB ID is skipped rather than
// cached as zero.
func mediaServerCachedItem(
	entry *mediaServerEntry,
	item emby.Item,
	itemType string,
	syncTime string,
) (mediaserver.Item, bool) {
	tmdbID, ok := mediaServerTMDBID(item.ProviderIDs)
	if !ok {
		return mediaserver.Item{}, false
	}
	return mediaserver.Item{
		Server:        strings.TrimSpace(entry.setting.Name),
		Library:       strings.TrimSpace(item.ParentID),
		ItemID:        strings.TrimSpace(item.ID),
		ItemType:      itemType,
		Title:         item.Name,
		OriginalTitle: item.OriginalTitle,
		Year:          mediaServerProductionYear(item.ProductionYear),
		TMDBID:        tmdbID,
		IMDBID:        embyProviderID(item.ProviderIDs, "imdb"),
		TVDBID:        embyProviderID(item.ProviderIDs, "tvdb"),
		Path:          item.Path,
		LstModDate:    syncTime,
	}, true
}

func mediaServerTMDBID(providerIDs map[string]string) (int64, bool) {
	value := embyProviderID(providerIDs, "tmdb")
	if value == "" {
		return 0, true
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func embyProviderID(providerIDs map[string]string, name string) string {
	for provider, value := range providerIDs {
		if strings.EqualFold(strings.TrimSpace(provider), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// syncLibraryItems enumerates one library the way V2's generator does: item
// pages below the library parent, with any `Folder` type recursed into, and
// only Movie/Series entries upserted. The returned result reports whether the
// enumeration stayed inside the bounded limits; V2 has no such limit and simply
// streams everything.
func (r *mediaServerRuntime) syncLibraryItems(
	ctx context.Context,
	store mediaserver.ItemStore,
	entry *mediaServerEntry,
	userID string,
	library emby.UserView,
	syncTime string,
) (mediaServerLibrarySyncResult, error) {
	parentID := strings.TrimSpace(library.ID)
	if parentID == "" {
		return mediaServerLibrarySyncResult{}, errMediaServerItemSyncFailed
	}
	result := mediaServerLibrarySyncResult{complete: true}
	pending := []mediaServerFolder{{id: parentID}}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		for start := 0; ; {
			if err := ctx.Err(); err != nil {
				return mediaServerLibrarySyncResult{}, err
			}
			response, err := entry.client.ListUserItems(ctx, emby.UserItemsOptions{
				UserID: userID, ParentID: current.id,
				StartIndex: start, Limit: mediaServerItemPageSize,
			})
			if err != nil {
				return mediaServerLibrarySyncResult{}, err
			}
			if !mediaServerPageConsistent(response, start, mediaServerItemPageSize) {
				return mediaServerLibrarySyncResult{}, errMediaServerItemSyncFailed
			}
			for _, item := range response.Items {
				itemID := strings.TrimSpace(item.ID)
				if itemID == "" {
					continue
				}
				if isMediaServerFolder(item.Type) {
					if result.folders+1 > mediaServerItemMaxFolders || current.depth+1 > mediaServerItemMaxDepth {
						result.complete = false
						continue
					}
					result.folders++
					pending = append(pending, mediaServerFolder{id: itemID, depth: current.depth + 1})
					continue
				}
				itemType, matched := mediaServerItemType(item.Type)
				if !matched {
					continue
				}
				record, ok := mediaServerCachedItem(entry, item, itemType, syncTime)
				if !ok {
					continue
				}
				if itemType == mediaserver.ItemTypeTV {
					seasonInfo, seasonsComplete, seasonErr := r.loadMediaServerSeasonInfo(ctx, entry, itemID)
					if seasonErr != nil {
						return mediaServerLibrarySyncResult{}, seasonErr
					}
					if !seasonsComplete {
						result.complete = false
						continue
					}
					record.SeasonInfo = seasonInfo
				}
				if result.items+1 > mediaServerItemMaxPerLibrary {
					result.complete = false
					continue
				}
				if err := store.UpsertMediaServerItem(ctx, record); err != nil {
					return mediaServerLibrarySyncResult{}, err
				}
				result.items++
			}
			next := start + len(response.Items)
			if mediaServerPageDone(response, next, mediaServerItemPageSize) {
				break
			}
			if len(response.Items) == 0 || next >= mediaServerItemMaxPerLibrary {
				result.complete = false
				break
			}
			start = next
		}
	}
	return result, nil
}
