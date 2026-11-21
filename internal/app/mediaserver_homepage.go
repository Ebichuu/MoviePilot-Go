package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/httpapi"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/plex"
)

// embyHomepageClient 抽象 Emby/Jellyfin 首页最近入库与继续观看接口。
type embyHomepageClient interface {
	ListLatestItems(ctx context.Context, userID string, limit int) ([]emby.PlayItem, error)
	ListResumeItems(ctx context.Context, userID string, limit int) ([]emby.PlayItem, error)
	PrimaryImageURL(itemID string) string
	BackdropImageURL(itemID, imageTag string) string
	PlayURLWithServer(itemID, serverID string) (string, error)
	LibraryURL(libraryID, serverID string) string
}

// plexHomepageClient 抽象 Plex 首页最近入库、继续观看与媒体库跳转/图片接口。
type plexHomepageClient interface {
	ListPlexLibraries(ctx context.Context) ([]plex.Library, error)
	ListContinueWatching(ctx context.Context, sectionIDs []string, limit int) ([]plex.PlayItem, error)
	ListRecentlyAdded(ctx context.Context, sectionIDs []string, limit int) ([]plex.PlayItem, error)
	ImageURL(imgPath string) string
	LibraryURL(sectionKey string) string
	PlayURL(itemID string) (string, error)
}

// ListMediaServerLatest 复刻 V2 Emby `get_latest`：查询 `/Users/{user}/Items/Latest`，
// 过滤 Movie/Series 与已配置的同步媒体库目录，输出前端首页「最近入库」卡片数据。
func (r *mediaServerRuntime) ListMediaServerLatest(
	ctx context.Context,
	selector string,
	count int,
) ([]httpapi.MediaServerPlayItem, error) {
	if ctx == nil {
		return nil, errors.New("媒体服务器请求失败")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if count <= 0 {
		count = 20
	}
	entries := r.entriesSnapshot(selector)
	result := make([]httpapi.MediaServerPlayItem, 0, count)
	for _, entry := range entries {
		if len(result) >= count {
			break
		}
		if php, ok := entry.client.(plexHomepageClient); ok {
			items, err := r.listPlexLatest(ctx, entry, php, count-len(result))
			if err != nil {
				if r.logger != nil {
					r.logger.Warn("读取 Plex 最近入库失败",
						"server", entry.setting.Name, "type", entry.setting.Type, "error", err)
				}
				return nil, errors.New("媒体服务器请求失败")
			}
			result = append(result, items...)
			continue
		}
		hp, ok := entry.client.(embyHomepageClient)
		if !ok {
			continue
		}
		userID := r.entryUserID(ctx, entry)
		if userID == "" {
			continue
		}
		rawItems, err := hp.ListLatestItems(ctx, userID, 100)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("读取媒体服务器最近入库失败",
					"server", entry.setting.Name, "type", entry.setting.Type, "error", err)
			}
			return nil, errors.New("媒体服务器请求失败")
		}
		allowedFolders := r.allowedLibraryFolders(ctx, entry)
		for _, item := range rawItems {
			if len(result) >= count {
				break
			}
			if item.Type != "Movie" && item.Type != "Series" {
				continue
			}
			if !itemPathInFolders(item.Path, allowedFolders) {
				continue
			}
			mediaType := "电影"
			if item.Type == "Series" {
				mediaType = "电视剧"
			}
			serverID := strings.TrimSpace(item.ServerID)
			link, _ := hp.PlayURLWithServer(item.ID, serverID)
			subtitle := ""
			if item.ProductionYear > 0 {
				subtitle = strconv.Itoa(item.ProductionYear)
			}
			result = append(result, httpapi.MediaServerPlayItem{
				ID:                item.ID,
				ItemID:            item.ID,
				ServerID:          serverID,
				Title:             item.Name,
				Subtitle:          subtitle,
				Type:              mediaType,
				Image:             hp.PrimaryImageURL(item.ID),
				Link:              link,
				URL:               link,
				BackdropImageTags: append([]string(nil), item.BackdropImageTags...),
				ServerType:        entry.setting.Type,
			})
		}
	}
	return result, nil
}

// ListMediaServerPlaying 复刻 V2 Emby `get_resume`：查询 `/Users/{user}/Items/Resume`，
// 过滤 Movie/Episode 与已配置的同步媒体库目录，输出前端首页「继续观看」卡片数据。
func (r *mediaServerRuntime) ListMediaServerPlaying(
	ctx context.Context,
	selector string,
	count int,
) ([]httpapi.MediaServerPlayItem, error) {
	if ctx == nil {
		return nil, errors.New("媒体服务器请求失败")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if count <= 0 {
		count = 12
	}
	entries := r.entriesSnapshot(selector)
	result := make([]httpapi.MediaServerPlayItem, 0, count)
	for _, entry := range entries {
		if len(result) >= count {
			break
		}
		if php, ok := entry.client.(plexHomepageClient); ok {
			items, err := r.listPlexPlaying(ctx, entry, php, count-len(result))
			if err != nil {
				if r.logger != nil {
					r.logger.Warn("读取 Plex 继续观看失败",
						"server", entry.setting.Name, "type", entry.setting.Type, "error", err)
				}
				return nil, errors.New("媒体服务器请求失败")
			}
			result = append(result, items...)
			continue
		}
		hp, ok := entry.client.(embyHomepageClient)
		if !ok {
			continue
		}
		userID := r.entryUserID(ctx, entry)
		if userID == "" {
			continue
		}
		rawItems, err := hp.ListResumeItems(ctx, userID, 100)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("读取媒体服务器继续观看失败",
					"server", entry.setting.Name, "type", entry.setting.Type, "error", err)
			}
			return nil, errors.New("媒体服务器请求失败")
		}
		allowedFolders := r.allowedLibraryFolders(ctx, entry)
		for _, item := range rawItems {
			if len(result) >= count {
				break
			}
			if item.Type != "Movie" && item.Type != "Episode" {
				continue
			}
			if !itemPathInFolders(item.Path, allowedFolders) {
				continue
			}
			serverID := strings.TrimSpace(item.ServerID)
			link, _ := hp.PlayURLWithServer(item.ID, serverID)

			var (
				mediaType string
				title     string
				subtitle  string
				image     string
			)
			if item.Type == "Movie" {
				mediaType = "电影"
				title = item.Name
				if item.ProductionYear > 0 {
					subtitle = strconv.Itoa(item.ProductionYear)
				}
				if len(item.BackdropImageTags) > 0 && strings.TrimSpace(item.BackdropImageTags[0]) != "" {
					image = hp.BackdropImageURL(item.ID, item.BackdropImageTags[0])
				} else {
					image = hp.PrimaryImageURL(item.ID)
				}
			} else {
				mediaType = "电视剧"
				title = strings.TrimSpace(item.SeriesName)
				if title == "" {
					title = item.Name
				}
				if item.ParentIndexNumber != nil && item.IndexNumber != nil {
					subtitle = fmt.Sprintf("S%d:%d - %s", *item.ParentIndexNumber, *item.IndexNumber, item.Name)
				} else {
					subtitle = item.Name
				}
				seriesID := strings.TrimSpace(item.SeriesID)
				if seriesID != "" && strings.TrimSpace(item.SeriesPrimaryImageTag) != "" {
					image = hp.BackdropImageURL(seriesID, item.SeriesPrimaryImageTag)
				}
				if image == "" {
					targetImgID := seriesID
					if targetImgID == "" {
						targetImgID = item.ID
					}
					image = hp.PrimaryImageURL(targetImgID)
				}
			}

			var pct *float64
			if item.UserData.PlayedPercentage != nil {
				v := *item.UserData.PlayedPercentage
				pct = &v
			}
			result = append(result, httpapi.MediaServerPlayItem{
				ID:         item.ID,
				ItemID:     item.ID,
				ServerID:   serverID,
				Title:      title,
				Subtitle:   subtitle,
				Type:       mediaType,
				Image:      image,
				Link:       link,
				URL:        link,
				Percent:    pct,
				ServerType: entry.setting.Type,
			})
		}
	}
	return result, nil
}

func (r *mediaServerRuntime) allowedLibraryFolders(ctx context.Context, entry *mediaServerEntry) []string {
	if entry == nil || entry.client == nil || len(entry.setting.SyncLibraries) == 0 {
		return nil
	}
	if slices.Contains(entry.setting.SyncLibraries, "all") {
		return nil
	}
	libs, err := entry.client.ListLibraries(ctx)
	if err != nil {
		return nil
	}
	folders := make([]string, 0)
	for _, lib := range libs {
		if !slices.Contains(entry.setting.SyncLibraries, lib.ItemID) {
			continue
		}
		for _, loc := range lib.Locations {
			if trimmed := strings.TrimSpace(loc); trimmed != "" {
				folders = append(folders, trimmed)
			}
		}
	}
	return folders
}

func itemPathInFolders(itemPath string, folders []string) bool {
	if len(folders) == 0 || strings.TrimSpace(itemPath) == "" {
		return true
	}
	normPath := strings.ReplaceAll(strings.TrimSpace(itemPath), `\`, "/")
	for _, folder := range folders {
		normFolder := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(folder), `\`, "/"), "/")
		if normFolder == "" {
			continue
		}
		if normPath == normFolder || strings.HasPrefix(normPath, normFolder+"/") {
			return true
		}
	}
	return false
}

func libraryAllowedBySyncConfig(syncLibraries []string, libraryID string) bool {
	if len(syncLibraries) == 0 || slices.Contains(syncLibraries, "all") {
		return true
	}
	return slices.Contains(syncLibraries, strings.TrimSpace(libraryID))
}

func plexAllowedSectionIDs(ctx context.Context, entry *mediaServerEntry, php plexHomepageClient) ([]string, error) {
	libs, err := php.ListPlexLibraries(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(libs))
	for _, lib := range libs {
		if lib.Type != "movie" && lib.Type != "show" {
			continue
		}
		if !libraryAllowedBySyncConfig(entry.setting.SyncLibraries, lib.Key) {
			continue
		}
		if lib.Key != "" {
			ids = append(ids, lib.Key)
		}
	}
	return ids, nil
}

func (r *mediaServerRuntime) listPlexLatest(
	ctx context.Context,
	entry *mediaServerEntry,
	php plexHomepageClient,
	limit int,
) ([]httpapi.MediaServerPlayItem, error) {
	sectionIDs, err := plexAllowedSectionIDs(ctx, entry, php)
	if err != nil {
		return nil, err
	}
	if len(sectionIDs) == 0 {
		return nil, nil
	}
	rawItems, err := php.ListRecentlyAdded(ctx, sectionIDs, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.MediaServerPlayItem, 0, len(rawItems))
	for _, item := range rawItems {
		if len(out) >= limit {
			break
		}
		var mediaType, title, image string
		switch item.Type {
		case "movie":
			mediaType = "电影"
			title = item.Title
			image = php.ImageURL(item.Thumb)
		case "season":
			mediaType = "电视剧"
			idx := 0
			if item.Index != nil {
				idx = *item.Index
			}
			title = fmt.Sprintf("%s 第%d季", item.ParentTitle, idx)
			image = php.ImageURL(item.Thumb)
		case "episode":
			mediaType = "电视剧"
			season, ep := 0, 0
			if item.ParentIndex != nil {
				season = *item.ParentIndex
			}
			if item.Index != nil {
				ep = *item.Index
			}
			title = fmt.Sprintf("%s 第%d季 第%d集", item.GrandparentTitle, season, ep)
			thumb := firstNonEmptyString(item.ParentThumb, item.GrandparentThumb, item.Thumb)
			image = php.ImageURL(thumb)
		case "show":
			mediaType = "电视剧"
			title = fmt.Sprintf("%s 共%d季", item.Title, item.ChildCount)
			image = php.ImageURL(item.Thumb)
		default:
			continue
		}
		subtitle := ""
		if item.Year > 0 {
			subtitle = strconv.Itoa(item.Year)
		}
		link, _ := php.PlayURL(item.Key)
		id := firstNonEmptyString(item.Key, item.RatingKey)
		out = append(out, httpapi.MediaServerPlayItem{
			ID:         id,
			ItemID:     firstNonEmptyString(item.RatingKey, item.Key),
			Title:      title,
			Subtitle:   subtitle,
			Type:       mediaType,
			Image:      image,
			Link:       link,
			URL:        link,
			ServerType: entry.setting.Type,
		})
	}
	return out, nil
}

func (r *mediaServerRuntime) listPlexPlaying(
	ctx context.Context,
	entry *mediaServerEntry,
	php plexHomepageClient,
	limit int,
) ([]httpapi.MediaServerPlayItem, error) {
	sectionIDs, err := plexAllowedSectionIDs(ctx, entry, php)
	if err != nil {
		return nil, err
	}
	if len(sectionIDs) == 0 {
		return nil, nil
	}
	rawItems, err := php.ListContinueWatching(ctx, sectionIDs, limit)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.MediaServerPlayItem, 0, len(rawItems))
	for _, item := range rawItems {
		if len(out) >= limit {
			break
		}
		var mediaType, title, subtitle string
		if item.Type == "movie" {
			mediaType = "电影"
			title = item.Title
			if item.Year > 0 {
				subtitle = strconv.Itoa(item.Year)
			}
		} else {
			mediaType = "电视剧"
			title = firstNonEmptyString(item.GrandparentTitle, item.Title)
			if item.ParentIndex != nil && item.Index != nil {
				subtitle = fmt.Sprintf("S%d:E%d - %s", *item.ParentIndex, *item.Index, item.Title)
			} else {
				subtitle = item.Title
			}
		}
		link, _ := php.PlayURL(item.Key)
		image := php.ImageURL(firstNonEmptyString(item.Art, item.Thumb))
		pctVal := 0.0
		if item.ViewOffset > 0 && item.Duration > 0 {
			pctVal = float64(item.ViewOffset) / float64(item.Duration) * 100
		}
		pct := pctVal
		id := firstNonEmptyString(item.Key, item.RatingKey)
		out = append(out, httpapi.MediaServerPlayItem{
			ID:         id,
			ItemID:     firstNonEmptyString(item.RatingKey, item.Key),
			Title:      title,
			Subtitle:   subtitle,
			Type:       mediaType,
			Image:      image,
			Link:       link,
			URL:        link,
			Percent:    &pct,
			ServerType: entry.setting.Type,
		})
	}
	return out, nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
