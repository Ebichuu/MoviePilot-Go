package app

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/mediaindex"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/emby"
	"github.com/moviepilot-go/moviepilot-go/internal/mediaserver/plex"
)

// mediaServerClient 抽象 Emby / Jellyfin / Plex 客户端在运行时所需的最小统一能力集合。
type mediaServerClient interface {
	CloseIdleConnections()
	PlayURL(itemID string) (string, error)
	ListLibraries(ctx context.Context) ([]emby.Library, error)
	ListUsers(ctx context.Context) ([]emby.User, error)
	ListUserViews(ctx context.Context, userID string) ([]emby.UserView, error)
	ListUserItems(ctx context.Context, options emby.UserItemsOptions) (emby.ItemsResponse, error)
	ListEpisodes(ctx context.Context, options emby.ListEpisodesOptions) (emby.ItemsResponse, error)
	GetItemInfo(ctx context.Context, userID, itemID string) (emby.Item, bool, error)
	FindItems(ctx context.Context, options emby.FindItemsOptions) (emby.ItemsResponse, error)
	SearchSeries(ctx context.Context, options emby.SearchSeriesOptions) (emby.ItemsResponse, error)
	SearchMovies(ctx context.Context, options emby.SearchMoviesOptions) (emby.ItemsResponse, error)
	RefreshAll(ctx context.Context) error
}

var _ mediaServerClient = (*emby.Client)(nil)

// plexMediaServerAdapter 将纯 Go *plex.Client 适配到统一 mediaServerClient 契约，
// 让媒体库列表、条目同步、入库确认、订阅去重、刷新与播放链接全链路无差异支持 Plex。
type plexMediaServerAdapter struct {
	client *plex.Client
}

var _ mediaServerClient = (*plexMediaServerAdapter)(nil)

func (a *plexMediaServerAdapter) CloseIdleConnections() {
	if a != nil && a.client != nil {
		a.client.CloseIdleConnections()
	}
}

func (a *plexMediaServerAdapter) PlayURL(itemID string) (string, error) {
	return a.client.PlayURL(itemID)
}

func (a *plexMediaServerAdapter) ListLibraries(ctx context.Context) ([]emby.Library, error) {
	libs, err := a.client.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]emby.Library, 0, len(libs))
	for _, lib := range libs {
		out = append(out, emby.Library{
			Name:           lib.Title,
			CollectionType: plexCollectionType(lib.Type),
			Locations:      append([]string(nil), lib.Locations...),
			ItemID:         lib.Key,
		})
	}
	return out, nil
}

// ListUsers Plex 单 Token 即可访问全部媒体库，返回单一默认管理员占位用户以复用同步流程。
func (a *plexMediaServerAdapter) ListUsers(context.Context) ([]emby.User, error) {
	return []emby.User{{ID: "plex-owner", Name: "plex", Policy: emby.UserPolicy{IsAdministrator: true}}}, nil
}

func (a *plexMediaServerAdapter) ListUserViews(ctx context.Context, _ string) ([]emby.UserView, error) {
	libs, err := a.client.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]emby.UserView, 0, len(libs))
	for _, lib := range libs {
		if lib.Type != "movie" && lib.Type != "show" {
			continue
		}
		views = append(views, emby.UserView{
			ID:             lib.Key,
			Name:           lib.Title,
			CollectionType: plexCollectionType(lib.Type),
		})
	}
	return views, nil
}

func (a *plexMediaServerAdapter) ListUserItems(ctx context.Context, options emby.UserItemsOptions) (emby.ItemsResponse, error) {
	page, err := a.client.ListSectionItems(ctx, options.ParentID, options.StartIndex, options.Limit)
	if err != nil {
		return emby.ItemsResponse{}, err
	}
	return plexPageToEmby(page), nil
}

func (a *plexMediaServerAdapter) ListEpisodes(ctx context.Context, options emby.ListEpisodesOptions) (emby.ItemsResponse, error) {
	page, err := a.client.ListEpisodes(ctx, options.SeriesID, options.StartIndex, options.Limit)
	if err != nil {
		return emby.ItemsResponse{}, err
	}
	return plexPageToEmby(page), nil
}

func (a *plexMediaServerAdapter) GetItemInfo(ctx context.Context, _, itemID string) (emby.Item, bool, error) {
	item, found, err := a.client.GetItem(ctx, itemID)
	if err != nil || !found {
		return emby.Item{}, found, err
	}
	return plexItemToEmby(item), true, nil
}

func (a *plexMediaServerAdapter) SearchSeries(ctx context.Context, options emby.SearchSeriesOptions) (emby.ItemsResponse, error) {
	items, err := a.client.SearchItems(ctx, options.SearchTerm, "show", options.Limit)
	if err != nil {
		return emby.ItemsResponse{}, err
	}
	return plexItemsToEmby(items), nil
}

func (a *plexMediaServerAdapter) SearchMovies(ctx context.Context, options emby.SearchMoviesOptions) (emby.ItemsResponse, error) {
	items, err := a.client.SearchItems(ctx, options.SearchTerm, "movie", options.Limit)
	if err != nil {
		return emby.ItemsResponse{}, err
	}
	return plexItemsToEmby(items), nil
}

func (a *plexMediaServerAdapter) FindItems(ctx context.Context, options emby.FindItemsOptions) (emby.ItemsResponse, error) {
	libs, err := a.client.ListLibraries(ctx)
	if err != nil {
		return emby.ItemsResponse{}, err
	}
	wantPath := strings.TrimSpace(options.Path)
	wantProvider := strings.ToLower(strings.TrimSpace(options.AnyProviderIDEquals))
	limit := options.Limit
	if limit <= 0 {
		limit = 200
	}

	matched := make([]emby.Item, 0)
	for _, lib := range libs {
		if lib.Type != "movie" && lib.Type != "show" {
			continue
		}
		if wantPath != "" && len(lib.Locations) > 0 && !pathUnderAnyLocation(wantPath, lib.Locations) {
			continue
		}
		for start := 0; start < 10_000; {
			page, err := a.client.ListSectionItems(ctx, lib.Key, start, 200)
			if err != nil {
				return emby.ItemsResponse{}, err
			}
			for _, item := range page.Items {
				if item.Type == "show" {
					if wantProvider != "" && !matchesPlexProvider(item.ProviderIDs, wantProvider) && wantPath == "" {
						continue
					}
					epPage, epErr := a.client.ListEpisodes(ctx, item.RatingKey, 0, 500)
					if epErr != nil {
						return emby.ItemsResponse{}, epErr
					}
					for _, ep := range epPage.Items {
						if plexItemMatchesFind(ep, item.ProviderIDs, wantPath, wantProvider) {
							matched = append(matched, plexItemToEmby(ep))
						}
					}
					continue
				}
				if plexItemMatchesFind(item, item.ProviderIDs, wantPath, wantProvider) {
					matched = append(matched, plexItemToEmby(item))
				}
			}
			next := start + len(page.Items)
			if len(page.Items) == 0 || next >= page.TotalSize {
				break
			}
			start = next
		}
	}

	total := len(matched)
	startIdx := options.StartIndex
	if startIdx < 0 || startIdx > total {
		startIdx = total
	}
	endIdx := startIdx + limit
	if endIdx > total {
		endIdx = total
	}
	return emby.ItemsResponse{
		Items:            matched[startIdx:endIdx],
		TotalRecordCount: total,
		StartIndex:       options.StartIndex,
	}, nil
}

func (a *plexMediaServerAdapter) RefreshAll(ctx context.Context) error {
	return a.client.RefreshAll(ctx)
}

func (a *plexMediaServerAdapter) ListPlexLibraries(ctx context.Context) ([]plex.Library, error) {
	return a.client.ListLibraries(ctx)
}

func (a *plexMediaServerAdapter) ListContinueWatching(ctx context.Context, sectionIDs []string, limit int) ([]plex.PlayItem, error) {
	return a.client.ListContinueWatching(ctx, sectionIDs, limit)
}

func (a *plexMediaServerAdapter) ListRecentlyAdded(ctx context.Context, sectionIDs []string, limit int) ([]plex.PlayItem, error) {
	return a.client.ListRecentlyAdded(ctx, sectionIDs, limit)
}

func (a *plexMediaServerAdapter) ImageURL(imgPath string) string {
	return a.client.ImageURL(imgPath)
}

func (a *plexMediaServerAdapter) LibraryURL(sectionKey string) string {
	return a.client.LibraryURL(sectionKey)
}

func plexCollectionType(libType string) string {
	switch strings.ToLower(strings.TrimSpace(libType)) {
	case "movie":
		return "movies"
	case "show":
		return "tvshows"
	default:
		return libType
	}
}

func plexItemToEmby(item plex.Item) emby.Item {
	embyType := item.Type
	switch strings.ToLower(item.Type) {
	case "movie":
		embyType = "Movie"
	case "show":
		embyType = "Series"
	case "episode":
		embyType = "Episode"
	}
	return emby.Item{
		ID:                item.RatingKey,
		Name:              item.Title,
		Type:              embyType,
		Path:              item.Path,
		ParentID:          item.LibraryKey,
		OriginalTitle:     item.OriginalTitle,
		IndexNumber:       item.Index,
		ParentIndexNumber: item.ParentIndex,
		ProductionYear:    item.Year,
		ProviderIDs:       cloneStringMap(item.ProviderIDs),
	}
}

func plexPageToEmby(page plex.ItemsPage) emby.ItemsResponse {
	items := make([]emby.Item, 0, len(page.Items))
	for _, it := range page.Items {
		items = append(items, plexItemToEmby(it))
	}
	return emby.ItemsResponse{
		Items:            items,
		TotalRecordCount: page.TotalSize,
		StartIndex:       page.StartIndex,
	}
}

func plexItemsToEmby(items []plex.Item) emby.ItemsResponse {
	out := make([]emby.Item, 0, len(items))
	for _, it := range items {
		out = append(out, plexItemToEmby(it))
	}
	return emby.ItemsResponse{Items: out, TotalRecordCount: len(out), StartIndex: 0}
}

func pathUnderAnyLocation(target string, locations []string) bool {
	normTarget := strings.ToLower(strings.ReplaceAll(target, `\`, "/"))
	for _, loc := range locations {
		normLoc := strings.ToLower(strings.TrimRight(strings.ReplaceAll(loc, `\`, "/"), "/"))
		if normLoc == "" {
			continue
		}
		if normTarget == normLoc || strings.HasPrefix(normTarget, normLoc+"/") {
			return true
		}
	}
	return false
}

func plexItemMatchesFind(item plex.Item, showProviders map[string]string, wantPath, wantProvider string) bool {
	if wantPath != "" {
		normWant := strings.ToLower(path.Clean(strings.ReplaceAll(wantPath, `\`, "/")))
		normItem := strings.ToLower(path.Clean(strings.ReplaceAll(item.Path, `\`, "/")))
		if normItem != normWant && !strings.HasPrefix(normItem, normWant+"/") {
			return false
		}
	}
	if wantProvider != "" {
		if !matchesPlexProvider(item.ProviderIDs, wantProvider) && !matchesPlexProvider(showProviders, wantProvider) {
			return false
		}
	}
	return true
}

func matchesPlexProvider(providers map[string]string, want string) bool {
	if len(providers) == 0 {
		return false
	}
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(want)), ".", 2)
	if len(parts) != 2 {
		return false
	}
	wantKey, wantVal := parts[0], parts[1]
	for k, v := range providers {
		if strings.ToLower(k) == wantKey && strings.ToLower(strings.TrimSpace(v)) == wantVal {
			return true
		}
	}
	return false
}

func translateMediaServerError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, emby.ErrOutcomeUnknown) || errors.Is(err, plex.ErrOutcomeUnknown) {
		return mediaindex.ErrOutcomeUnknown
	}
	var embyErr *emby.Error
	if errors.As(err, &embyErr) {
		return &mediaindex.ServerError{Cause: embyErr, Retryable: embyErr.Code != emby.ErrorCodeInvalidConfig}
	}
	var plexErr *plex.Error
	if errors.As(err, &plexErr) {
		return &mediaindex.ServerError{Cause: plexErr, Retryable: plexErr.Code != plex.ErrorCodeInvalidConfig}
	}
	return mediaindex.ErrServerUnavailable
}
